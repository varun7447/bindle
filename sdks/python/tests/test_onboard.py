import io
import json
import os
import stat
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from bindle import _cli, _mcp, _onboard as ob

KEY = "bd_live_" + "a1" * 26


class _Ingest(BaseHTTPRequestHandler):
    traces = []

    def log_message(self, *a):
        pass

    def _auth(self):
        if self.headers.get("Authorization") != "Bearer " + KEY:
            self.send_response(401)
            self.end_headers()
            return False
        return True

    def do_GET(self):
        if self._auth():
            body = json.dumps({"version": 1, "sample_rate": 1, "capture_content": False}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(body)

    def do_POST(self):
        if self._auth():
            n = int(self.headers.get("Content-Length", 0))
            _Ingest.traces.append(json.loads(self.rfile.read(n)))
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"{}")


@pytest.fixture
def ingest():
    _Ingest.traces = []
    srv = HTTPServer(("127.0.0.1", 0), _Ingest)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_port}"
    srv.shutdown()


def test_detect_framework(tmp_path):
    assert ob.detect_framework(tmp_path).id == "python"
    (tmp_path / "requirements.txt").write_text("openai>=1\nlanggraph==0.2  # graphs\n")
    assert ob.detect_framework(tmp_path).id == "langchain"
    (tmp_path / "requirements.txt").write_text("")
    (tmp_path / "pyproject.toml").write_text('[project]\ndependencies = [\n  "crewai[tools]>=0.80",\n  "anthropic",\n]\n')
    assert ob.detect_framework(tmp_path).id == "crewai"
    (tmp_path / "pyproject.toml").write_text('[project]\ndependencies = ["Strands_Agents>=1"]\n')
    assert ob.detect_framework(tmp_path).id == "strands"


def test_endpoint_rules():
    assert ob.check_endpoint("https://ingest.example.com/") == "https://ingest.example.com"
    assert ob.check_endpoint("http://localhost:4318") == "http://localhost:4318"
    for bad in ("http://ingest.example.com", "ftp://x", "https://u:p@x.com", "nope"):
        with pytest.raises(ValueError):
            ob.check_endpoint(bad)
    assert ob.console_url("https://ingest.example.com") == "https://app.example.com"
    assert ob.console_url("https://bindle-ingest.internal.example") == ""
    assert ob.console_url("http://127.0.0.1:4318") == "http://localhost:3000"
    assert ob.mask_key(KEY) == KEY[:12] + "…"


def test_write_env_keeps_and_protects(tmp_path):
    (tmp_path / ".git").mkdir()
    (tmp_path / ".env").write_text("OTHER=1\nBINDLE_API_KEY=old\n")
    written, kept = ob.write_env(tmp_path, {"BINDLE_API_KEY": KEY})
    assert written == [] and kept == ["BINDLE_API_KEY"]
    written, _ = ob.write_env(tmp_path, {"BINDLE_API_KEY": KEY}, force=True)
    env = ob.read_env_file(tmp_path / ".env")
    assert written == ["BINDLE_API_KEY"] and env == {"OTHER": "1", "BINDLE_API_KEY": KEY}
    assert stat.S_IMODE(os.stat(tmp_path / ".env").st_mode) == 0o600
    assert ".env" in (tmp_path / ".gitignore").read_text().splitlines()
    ob.write_env(tmp_path, {"BINDLE_ENDPOINT": "http://localhost:4318"})
    assert (tmp_path / ".gitignore").read_text().count(".env\n") == 1


def test_connection_and_test_trace(ingest):
    assert ob.check_connection(ingest, KEY).ok
    bad = ob.check_connection(ingest, "bd_live_" + "b2" * 26)
    assert not bad.ok and "rejected" in bad.message and "b2b2b2b2b2b2" not in bad.message
    assert not ob.check_connection(ingest, "sk-not-a-bindle-key").ok
    assert not ob.check_connection("http://127.0.0.1:1", KEY).ok
    res = ob.send_test_trace(ingest, KEY, now_ns=1_700_000_000_000_000_000)
    assert res.ok and res.detail["url"].endswith("/traces/" + res.detail["trace_id"])
    spans = _Ingest.traces[0]["resourceSpans"][0]["scopeSpans"][0]["spans"]
    assert len(spans) == 2 and spans[1]["parentSpanId"] == spans[0]["spanId"]


def test_cli_init(ingest, tmp_path, capsys):
    (tmp_path / "requirements.txt").write_text("openai-agents\n")
    rc = _cli.main(["init", "--key", KEY, "--endpoint", ingest, "--dir", str(tmp_path)])
    out = capsys.readouterr().out
    assert rc == 0, out
    assert "OpenAI Agents SDK (detected)" in out and "test trace sent" in out and "content capture is off" in out
    assert KEY not in out
    assert ob.read_env_file(tmp_path / ".env") == {"BINDLE_API_KEY": KEY, "BINDLE_ENDPOINT": ingest}
    assert _cli.main(["check", "--dir", str(tmp_path)]) == 0  # reads .env
    assert _cli.main(["init", "--key", "bd_live_" + "c3" * 26, "--endpoint", ingest, "--dir", str(tmp_path), "--no-test"]) == 1


def _rpc(server, *msgs):
    stdin = io.StringIO("".join(json.dumps(m) + "\n" for m in msgs) + "not json\n")
    stdout = io.StringIO()
    _mcp.serve(stdin, stdout, server)
    return [json.loads(ln) for ln in stdout.getvalue().splitlines()]


def test_mcp_server(ingest, tmp_path):
    server = _mcp.Server(root=tmp_path, environ={"BINDLE_API_KEY": KEY, "BINDLE_ENDPOINT": ingest})
    out = _rpc(server,
               {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": _mcp.PROTOCOL_VERSION}},
               {"jsonrpc": "2.0", "method": "notifications/initialized"},
               {"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
               {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "bindle_check_connection", "arguments": {}}},
               {"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": {"name": "bindle_send_test_trace"}},
               {"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": {"name": "bindle_setup_guide", "arguments": {"framework": "crewai"}}},
               {"jsonrpc": "2.0", "id": 6, "method": "tools/call", "params": {"name": "nope"}},
               {"jsonrpc": "2.0", "id": 7, "method": "resources/list"})
    by_id = {m.get("id"): m for m in out}
    assert by_id[1]["result"]["protocolVersion"] == _mcp.PROTOCOL_VERSION
    assert len(by_id[2]["result"]["tools"]) == 4
    assert by_id[3]["result"]["isError"] is False
    assert "/traces/" in by_id[4]["result"]["content"][0]["text"]
    assert "bindle-sdk[crewai]" in by_id[5]["result"]["content"][0]["text"]
    assert by_id[6]["error"]["code"] == -32602 and by_id[7]["error"]["code"] == -32601
    assert by_id[None]["error"]["code"] == -32700  # the "not json" line
    assert len(out) == 8  # the notification gets no reply
    assert KEY not in json.dumps(out)


def test_mcp_without_key(tmp_path):
    out = _rpc(_mcp.Server(root=tmp_path, environ={}),
               {"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "bindle_check_connection"}})
    assert out[0]["result"]["isError"] is True and "BINDLE_API_KEY" in out[0]["result"]["content"][0]["text"]


def test_detect_new_frameworks(tmp_path):
    for deps, want in [("google-adk>=2\n", "google-adk"), ("agent-framework\nazure-ai-projects\n", "agent-framework"),
                       ("azure-ai-agents==1.1\nopenai\n", "foundry-agents"), ("strands-agents\n", "strands")]:
        (tmp_path / "requirements.txt").write_text(deps)
        assert ob.detect_framework(tmp_path).id == want, deps
    assert "AIAgentsInstrumentor().instrument()" in ob.setup_guide(ob.framework_by_id("foundry-agents"))


def test_cli_requires_endpoint(tmp_path, capsys, monkeypatch):
    monkeypatch.delenv("BINDLE_ENDPOINT", raising=False)
    monkeypatch.setattr("sys.stdin", io.StringIO(""))  # not a terminal: no prompt
    assert _cli.main(["init", "--key", KEY, "--dir", str(tmp_path), "--no-test"]) == 2
    assert "No endpoint" in capsys.readouterr().err
    assert _cli.main(["check", "--key", KEY, "--dir", str(tmp_path)]) == 2
    assert not (tmp_path / ".env").exists()


def test_mcp_without_endpoint(tmp_path):
    server = _mcp.Server(root=tmp_path, environ={"BINDLE_API_KEY": KEY})
    res = server.call_tool("bindle_check_connection", {})
    assert res["isError"] is True and "BINDLE_ENDPOINT is not set" in res["content"][0]["text"]
    guide = server.call_tool("bindle_setup_guide", {"framework": "crewai"})["content"][0]["text"]
    assert "BINDLE_ENDPOINT=<your Bindle ingest URL>" in guide


def test_setup_guide_names_the_endpoint():
    fw = ob.framework_by_id("python")
    assert "export BINDLE_ENDPOINT=https://ingest.example.com" in ob.setup_guide(fw, "https://ingest.example.com")
