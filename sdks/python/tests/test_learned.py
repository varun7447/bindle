"""Learned skills in the SDK (bindle.skills) and bindle skills prove."""

import io
import json
import urllib.error

import pytest
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from bindle import _learned, _prove, _remote
from bindle._cassette import Cassette

SKILL = "---\nname: retry-refund\ndescription: Retry a refund once when the payments API times out.\nmetadata:\n  bindle-lesson: les_0123456789abcdef\n---\n\nRetry once.\n"
DOC = {"agent": "refunds", "skills": [
    {"name": "retry-refund", "description": "Retry a refund once when the payments API times out.",
     "path": "skills/retry-refund/SKILL.md", "release": 2, "sha256": "s1", "body": SKILL},
    {"name": "check-address", "description": "Validate a shipping address before creating a label.",
     "path": "skills/check-address/SKILL.md", "release": 1, "sha256": "s2", "body": "---\nname: check-address\n---\nx\n"},
]}


class Resp:
    def __init__(self, doc, etag='"e1"'):
        self._b, self.headers = json.dumps(doc).encode(), {"ETag": etag}

    def read(self, n=-1):
        return self._b

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


class Opener:
    def __init__(self):
        self.mode, self.calls = "ok", []

    def __call__(self, req, timeout=None):
        self.calls.append((req.get_header("If-none-match"), timeout))
        if self.mode == "ok":
            return Resp(DOC)
        if self.mode == "down":
            raise urllib.error.URLError("connection refused")
        code = {"304": 304, "404": 404, "500": 500}[self.mode]
        raise urllib.error.HTTPError(req.full_url, code, "x", {}, io.BytesIO(b""))


def store(tmp_path, opener, killed=()):
    return _learned.SkillStore("http://localhost:4318", "bd_test_x", "refunds", "v12", killed=lambda: killed,
                               opener=opener, cache_dir=str(tmp_path))


def test_fetch_cache_and_bindle_down(tmp_path):
    op = Opener()
    s = store(tmp_path, op)
    assert s.list() == []
    assert s.refresh() == _learned.REFRESH
    assert [x.name for x in s.list()] == ["retry-refund", "check-address"]
    assert op.calls[0] == (None, _learned.FETCH_TIMEOUT)  # time-boxed
    s.refresh()
    assert op.calls[1][0] == '"e1"'  # conditional request

    op.mode = "down"  # Bindle unreachable: keep what we have, never raise
    s.refresh()
    assert len(s.list()) == 2
    op.mode = "500"
    s.refresh()
    assert len(s.list()) == 2
    fresh = store(tmp_path, op)  # a new process starts from the disk cache
    assert [x.release for x in fresh.list()] == [2, 1]

    op.mode = "304"
    assert fresh.refresh() == _learned.REFRESH and len(fresh.list()) == 2
    op.mode = "404"  # the workspace stopped shipping skills: drop them
    assert fresh.refresh() == _learned.IDLE_REFRESH and fresh.list() == []
    assert store(tmp_path, op).list() == []


def test_kill_switches(tmp_path):
    op = Opener()
    killed = ["refunds/skills/retry-refund/SKILL.md"]
    s = store(tmp_path, op, killed)
    s.refresh()
    assert [x.name for x in s.list()] == ["check-address"]
    killed.append("refunds/*")
    assert s.list() == []
    killed.clear()
    killed.append("other-agent/*")  # another agent's switch
    assert len(s.list()) == 2


def test_relevant_and_load(tmp_path):
    op = Opener()
    s = store(tmp_path, op)
    s.refresh()
    _learned._set_store(s)
    try:
        assert [x.name for x in _learned.relevant("The refund call to the payments API timed out")] == ["retry-refund"]
        assert _learned.relevant("nothing in common here") == []
        assert _learned.relevant("the address and the refund", limit=1)[0].name in ("retry-refund", "check-address")

        mem = InMemorySpanExporter()
        tp = TracerProvider()
        tp.add_span_processor(SimpleSpanProcessor(mem))
        with tp.get_tracer("t").start_as_current_span("plan"):
            assert _learned.load("retry-refund") == SKILL
            assert _learned.load("not-shipped") == ""
        (span,) = mem.get_finished_spans()
        (ev,) = span.events
        assert ev.name == _learned.USED_EVENT
        assert ev.attributes["bindle.skill.path"] == "skills/retry-refund/SKILL.md" and ev.attributes["bindle.skill.release"] == 2
        assert _learned.load("retry-refund") == SKILL  # no active span: still returns the text
    finally:
        _learned._set_store(None)
    assert _learned.list() == [] and _learned.load("retry-refund") == ""


def test_never_raises(tmp_path, monkeypatch):
    def boom(*a, **k):
        raise RuntimeError("unexpected")

    s = _learned.SkillStore("http://localhost:4318", "k", "refunds", killed=boom, opener=boom, cache_dir=str(tmp_path / "x"))
    s.refresh()
    assert s.list() == []
    _learned._set_store(s)
    try:
        assert _learned.relevant(None) == [] and _learned.load(None) == ""  # type: ignore[arg-type]
    finally:
        _learned._set_store(None)


def test_remote_kill_switches_parse():
    body = json.dumps({"sample_rate": 1, "capture_content": True, "killed_skills": ["refunds/*", 3, "bad"]}).encode()
    assert _remote.parse(body).killed_skills == ("refunds/*",)


# ---- bindle skills prove ---------------------------------------------------------

def cassette(trace, calls):
    return {"format": "bindle.cassette", "version": 1, "trace_id": trace, "input": f"refund {trace[:4]}",
            "tool_calls": calls, "incomplete": []}


TIMEOUT = {"tool": "refund_payment", "arguments": '{"order": "o_1"}', "result": "", "status": "failure", "error": "timed out"}
RETRY_OK = {"tool": "refund_payment", "arguments": '{"order": "o_1", "retry": true}', "result": '{"ok": true}', "status": "success"}


def agent(inp, *, skill, cassette):
    refund = cassette.tool(lambda order, retry=False: None, name="refund_payment")
    if "Retry once" in skill:
        try:
            return refund(order="o_1")
        except Exception:  # noqa: BLE001 - the skill says: retry once
            return refund(order="o_1", retry=True)
    return refund(order="o_1")


def test_replay_judges_each_run():
    fixed = cassette("a" * 32, [TIMEOUT, RETRY_OK])
    same = cassette("b" * 32, [TIMEOUT])  # no recorded retry: the retry isn't covered
    runs, gaps = _prove.replay(agent, [fixed, same], SKILL, "refund_payment")
    assert runs[0] == {"trace_id": "a" * 32, "fixed": True, "covered": True, "detail": ""}
    assert runs[1]["fixed"] is False and runs[1]["covered"] is False
    assert any("refund_payment was called but isn't in the recording" in g for g in gaps)
    runs, _ = _prove.replay(agent, [cassette("c" * 32, [TIMEOUT])], "no skill", "refund_payment")
    assert runs[0]["fixed"] is False and runs[0]["covered"] is True and runs[0]["detail"].startswith("failed the same way")


def test_skill_meta():
    assert _prove.skill_meta(SKILL) == ("retry-refund", "les_0123456789abcdef")
    with pytest.raises(ValueError):
        _prove.skill_meta("---\nname: x\n---\n")  # no lesson
    with pytest.raises(ValueError):
        _prove.skill_meta("no frontmatter")


def test_eval_regressions(tmp_path, monkeypatch):
    # The eval run installs a global tracer provider when none exists; put
    # OpenTelemetry's global state back so other tests start clean.
    from opentelemetry import trace

    monkeypatch.setattr(trace, "_TRACER_PROVIDER", trace._TRACER_PROVIDER)
    monkeypatch.setattr(trace._TRACER_PROVIDER_SET_ONCE, "_done", trace._TRACER_PROVIDER_SET_ONCE._done)
    ds = tmp_path / "ds.jsonl"
    ds.write_text(json.dumps({"input": "refund o_1", "output": "refund issued"}) + "\n"
                  + json.dumps({"input": "refund o_2", "output": "sorry"}) + "\n")
    checks = tmp_path / "checks.json"
    checks.write_text(json.dumps([{"name": "says issued", "kind": "contains", "value": "issued"}]))

    def worse(inp, *, skill, cassette):
        assert cassette is None
        return "please wait"

    n, regressions, notes = _prove.eval_regressions(worse, SKILL, str(ds), str(checks))
    assert (n, regressions) == (2, 1) and "says issued" in notes[0]
    n, regressions, _ = _prove.eval_regressions(lambda inp, **k: "refund issued", SKILL, str(ds), str(checks))
    assert regressions == 0


def test_cassette_input():
    c = Cassette(cassette("d" * 32, [TIMEOUT]))
    assert c.input.startswith("refund")
