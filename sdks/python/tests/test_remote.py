import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest
from opentelemetry.sdk.trace.sampling import Decision

import bindle
from bindle import _config, _remote
from bindle._export import RedactingSpanExporter
from bindle._redact import Redactor
from bindle._sampling import DynamicRatioSampler


class FakeIngest:
    """Serves /v1/config with ETag support and records requests."""

    def __init__(self, body: dict, etag='"v3"'):
        self.body, self.etag, self.requests, self.status = body, etag, [], 200
        fake = self

        class H(BaseHTTPRequestHandler):
            def do_GET(self):
                fake.requests.append(dict(self.headers))
                if fake.status != 200:
                    self.send_response(fake.status)
                    self.end_headers()
                    return
                if self.headers.get("If-None-Match") == fake.etag:
                    self.send_response(304)
                    self.end_headers()
                    return
                data = fake.body if isinstance(fake.body, bytes) else json.dumps(fake.body).encode()
                self.send_response(200)
                self.send_header("ETag", fake.etag)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(data)

            def log_message(self, *a):
                pass

        self.server = HTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.server.server_port}/v1/config"

    def close(self):
        self.server.shutdown()


CONFIG = {"version": 3, "sample_rate": 0.25, "capture_content": False, "poll_interval_seconds": 60,
          "redact_patterns": [{"name": "ORDER", "pattern": r"ord_\d+"}, {"name": "bad name", "pattern": "x"}]}


def test_parse_validates():
    s = _remote.parse(json.dumps(CONFIG).encode())
    assert s and s.version == 3 and s.sample_rate == 0.25 and s.capture_content is False
    assert s.redact_patterns == {"ORDER": r"ord_\d+"}  # invalid name dropped
    for bad in [b"not json", b"{}", json.dumps({**CONFIG, "sample_rate": 2}).encode(),
                json.dumps({**CONFIG, "capture_content": "no"}).encode()]:
        assert _remote.parse(bad) is None
    assert _remote.parse(json.dumps({**CONFIG, "poll_interval_seconds": 1}).encode()).poll_interval == 30


def test_poller_fetches_sends_key_and_uses_etag():
    fake = FakeIngest(CONFIG)
    got = []
    try:
        p = _remote.Poller(fake.url, "bd_live_k", got.append, user_agent="test")
        p.poll_once()
        p.poll_once()  # 304: no second update
        assert len(got) == 1 and got[0].version == 3
        assert fake.requests[0]["Authorization"] == "Bearer bd_live_k"
        assert fake.requests[1]["If-None-Match"] == '"v3"'
        fake.etag, fake.body = '"v4"', {**CONFIG, "version": 4, "sample_rate": 0.5}
        p.poll_once()
        assert len(got) == 2 and got[1].sample_rate == 0.5
    finally:
        fake.close()


def test_poller_swallows_failures_and_backs_off():
    fake = FakeIngest(b"garbage")
    got = []
    try:
        p = _remote.Poller(fake.url, "k", got.append, user_agent="test")
        p.poll_once()  # unusable body
        fake.status = 401
        p.poll_once()
        fake.status = 503
        p.poll_once()
        assert got == [] and p._failures == 3
        assert p._next_delay() > 60 * 4  # exponential backoff
        unreachable = _remote.Poller("http://127.0.0.1:1/v1/config", "k", got.append, user_agent="t", timeout=0.5)
        unreachable.poll_once()
        assert unreachable._failures == 1
    finally:
        fake.close()


def test_dynamic_sampler_rate_changes():
    s = DynamicRatioSampler(1.0)
    tid = 0x0000000000000000FFFFFFFFFFFFFFFF  # dropped at any rate < 1
    assert s.should_sample(None, tid, "x").decision == Decision.RECORD_AND_SAMPLE
    s.set_rate(0.5)
    assert s.should_sample(None, tid, "x").decision == Decision.DROP
    s.set_rate(5)
    assert s.rate == 1.0


def test_sampling_matches_ingest_vectors():
    import pathlib
    vectors = json.loads((pathlib.Path(__file__).parent / "sampling_vectors.json").read_text())
    for v in vectors:
        d = DynamicRatioSampler(v["rate"]).should_sample(None, int(v["trace_id"], 16), "x").decision
        assert (d == Decision.RECORD_AND_SAMPLE) == v["sampled"], v


def test_remote_is_a_ceiling(monkeypatch):
    cfg = _config.resolve(api_key="k", sample_rate=0.1, capture_content=True, extra_redact_patterns={"LOCAL": "loc_\\d+"})
    sampler = DynamicRatioSampler(cfg.sample_rate)
    exporter = RedactingSpanExporter(delegate=None, redactor=bindle._local_redactor(cfg), capture_content=True)
    monkeypatch.setitem(bindle._state, "config", cfg)
    monkeypatch.setitem(bindle._state, "sampler", sampler)
    monkeypatch.setitem(bindle._state, "exporter", exporter)
    monkeypatch.setitem(bindle._state, "remote", None)
    monkeypatch.setitem(bindle._state, "capture", True)
    bindle._apply_remote(_remote.RemoteSettings(4, 0.5, False, {"ORDER": r"ord_\d+"}))
    assert sampler.rate == 0.1  # local 0.1 is stricter than remote 0.5
    assert exporter.capture_content is False and bindle._capture() is False
    red = exporter._policy[1]
    assert red.apply("ord_1 loc_2 a@b.co") == "[REDACTED:ORDER] [REDACTED:LOCAL] [REDACTED:EMAIL]"
    bindle._apply_remote(_remote.RemoteSettings(5, 0.05, True, {}))
    assert sampler.rate == 0.05 and exporter.capture_content is True


def test_invalid_local_pattern_does_not_disable_tracing():
    r = Redactor(extra_patterns={"OK": r"x\d", "BROKEN": "("})
    assert r.apply("x1") == "[REDACTED:OK]"


def test_init_polls_remote_config(monkeypatch):
    fake = FakeIngest({**CONFIG, "sample_rate": 0.0})
    try:
        monkeypatch.setenv("BINDLE_REMOTE_CONFIG", "true")
        endpoint = fake.url.rsplit("/v1/config", 1)[0]
        assert bindle.init(api_key="bd_test_k", endpoint=endpoint, instrument=False)
        poller = bindle._state["poller"]
        assert poller is not None
        for _ in range(50):
            if bindle._state["remote"] is not None:
                break
            threading.Event().wait(0.05)
        assert bindle._state["remote"].version == 3
        assert bindle._state["sampler"].rate == 0.0
    finally:
        bindle.shutdown()
        fake.close()
    assert bindle._state["poller"] is None


def test_poll_is_not_traced_by_auto_instrumentation():
    from opentelemetry import context

    from bindle._remote import Poller

    seen = {}

    class Resp:
        status = 200
        headers = {}

        def read(self, n):
            return b'{"sample_rate": 1, "capture_content": true}'

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    def opener(req, timeout):
        seen["suppressed"] = context.get_value(context._SUPPRESS_INSTRUMENTATION_KEY)
        return Resp()

    Poller("http://x/v1/config", "k", lambda s: None, "ua", opener=opener).poll_once()
    assert seen["suppressed"] is True
    assert context.get_value(context._SUPPRESS_INSTRUMENTATION_KEY) is None  # restored afterwards


def test_console_pause_stops_the_agent_at_check_limits(monkeypatch):
    fake = FakeIngest({**CONFIG, "limits": [{"agent": "refunds", "paused": True}], "over_budget": []})
    try:
        monkeypatch.setenv("BINDLE_REMOTE_CONFIG", "true")
        endpoint = fake.url.rsplit("/v1/config", 1)[0]
        assert bindle.init(api_key="bd_test_k", endpoint=endpoint, agent="refunds", instrument=False)
        for _ in range(50):
            if bindle._state["remote"] is not None:
                break
            threading.Event().wait(0.05)
        with pytest.raises(bindle.LimitExceeded) as e:
            bindle.check_limits()
        assert e.value.reason == "paused"
        assert bindle.limits_status()["paused"] is True
    finally:
        bindle.shutdown()
        fake.close()
    bindle.check_limits()  # after shutdown: a no-op
