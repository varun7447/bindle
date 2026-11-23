import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import pytest

from bindle import _cli
from bindle._passport import ExportError, export_dogwood

DATA = Path(__file__).parent / "testdata"
ARN = "arn:aws:bedrock-agentcore:us-west-2:111122223333:gateway/refunds-gw-a1b2c3"
MGMT = "bd_mgmt_" + "b2" * 26
TOOLS = ["crm_find", "crm_update", "deploy_prod", "lookup_order", "refund_payment"]


def full():
    return json.loads((DATA / "passport_full.json").read_text())


def test_dogwood_golden():
    got = export_dogwood(full(), ARN, "OrdersAPI", TOOLS)
    assert got == (DATA / "passport_full.dogwood").read_text()


def test_dogwood_shapes():
    p = full()
    text = export_dogwood(p, ARN, "OrdersAPI", None)
    # Without a tool list, wildcards can't be expanded and say so.
    assert 'SKIPPED: tools.allow pattern "crm_*"' in text
    assert 'release tool pattern "deploy_*"' in text
    assert "bindle_no_release_git_push" in text
    # Budgets and data classes are never compiled into policy.
    assert "per_run" not in text and "PLACEHOLDER (not exported): data_classes" in text
    assert "Not a security control" in text

    p.pop("tools")
    assert "    action,\n" in export_dogwood(p, ARN, "OrdersAPI", TOOLS)  # undeclared tools: all permitted
    p["tools"] = {"allow": []}
    assert "allows no tools" in export_dogwood(p, ARN, "OrdersAPI", TOOLS)


@pytest.mark.parametrize("change,arn,target", [
    (lambda p: p.update(version="x"), ARN, "OrdersAPI"),
    (lambda p: None, "arn:aws:iam::111122223333:role/x", "OrdersAPI"),
    (lambda p: None, ARN, "Orders API"),
    (lambda p: p.update(limits=[{"tool": "x", "max": 1, "window": "25h"}]), ARN, "OrdersAPI"),
    (lambda p: p.update(limits=[{"tool": "x", "max": 1, "window": "90s"}]), ARN, "OrdersAPI"),
    (lambda p: p.update(limits=[{"tool": f"t{i}", "max": 1, "window": "1m"} for i in range(21)]), ARN, "OrdersAPI"),
])
def test_dogwood_rejects(change, arn, target):
    p = full()
    change(p)
    with pytest.raises(ExportError):
        export_dogwood(p, arn, target, TOOLS)


def test_tool_names_from_mcp_list(tmp_path):
    (tmp_path / "tools.json").write_text(json.dumps({"tools": [{"name": "crm_find"}, {"name": "lookup_order"}]}))
    (tmp_path / "p.json").write_text(json.dumps(full()))
    out = tmp_path / "out.dogwood"
    rc = _cli.main(["passport", "export", "--format", "dogwood", str(tmp_path / "p.json"), "--gateway-arn", ARN,
                    "--target", "OrdersAPI", "--tools", str(tmp_path / "tools.json"), "--out", str(out)])
    assert rc == 0
    text = out.read_text()
    assert "OrdersAPI___crm_find" in text and "crm_update" not in text


class _Ingest(BaseHTTPRequestHandler):
    seen = []

    def log_message(self, *a):
        pass

    def _reply(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(data)

    def _handle(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n)
        _Ingest.seen.append((self.command, self.path, self.headers.get("Authorization"), body))
        if self.headers.get("Authorization") != "Bearer " + MGMT:
            return self._reply(401, {"error": "a management key (bd_mgmt_...) is required"})
        doc = json.loads(body)
        if doc.get("version") != "bindle.passport/v1":
            return self._reply(422, {"error": 'version must be "bindle.passport/v1"'})
        if self.path == "/v1/passports/validate":
            return self._reply(200, {"ok": True, "agent": doc["agent"]})
        return self._reply(201, {"agent": doc["agent"], "version": 1, "unchanged": False})

    do_POST = _handle
    do_PUT = _handle


@pytest.fixture
def ingest():
    _Ingest.seen = []
    srv = HTTPServer(("127.0.0.1", 0), _Ingest)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_port}"
    srv.shutdown()


def test_validate_and_push(ingest, tmp_path, capsys, monkeypatch):
    monkeypatch.delenv("BINDLE_MGMT_KEY", raising=False)
    monkeypatch.delenv("BINDLE_ENDPOINT", raising=False)
    f = tmp_path / "p.json"
    f.write_text(json.dumps(full()))
    base = ["--endpoint", ingest, "--dir", str(tmp_path)]
    assert _cli.main(["passport", "validate", str(f), "--key", MGMT] + base) == 0
    assert _cli.main(["passport", "push", str(f), "--key", MGMT] + base) == 0
    out = capsys.readouterr().out
    assert "Valid passport for refund-agent" in out and "pushed version 1" in out
    assert [(m, p) for m, p, _, _ in _Ingest.seen] == [("POST", "/v1/passports/validate"), ("PUT", "/v1/passports")]
    assert MGMT not in out

    # Ingest keys are refused before anything is sent.
    assert _cli.main(["passport", "push", str(f), "--key", "bd_live_" + "a1" * 26] + base) == 2
    assert "management key" in capsys.readouterr().err
    assert len(_Ingest.seen) == 2

    # No endpoint: no guessed default.
    assert _cli.main(["passport", "push", str(f), "--key", MGMT, "--dir", str(tmp_path)]) == 2
    assert "BINDLE_ENDPOINT" in capsys.readouterr().err

    # Server-side validation errors are shown.
    bad = tmp_path / "bad.json"
    bad.write_text(json.dumps({"version": "x"}))
    assert _cli.main(["passport", "validate", str(bad), "--key", MGMT] + base) == 1
    assert "version must be" in capsys.readouterr().err

    # Keys come from the environment too; plain http is refused except locally.
    monkeypatch.setenv("BINDLE_MGMT_KEY", MGMT)
    monkeypatch.setenv("BINDLE_ENDPOINT", "http://ingest.example.com")
    assert _cli.main(["passport", "push", str(f), "--dir", str(tmp_path)]) == 2
