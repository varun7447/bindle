import hashlib
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import pytest

from bindle import _cli
from bindle._skills import STATE_FILE, SkillError, build_skill, skill_name

KEY = "bd_mgmt_" + "ab" * 26
TRACE = "0123456789abcdef0123456789abcdef"
PACK = {"format": "bindle.lesson-pack/v1", "lesson": "les_0123456789abcdef", "agent": "refund-agent", "tool": "lookup_order",
        "step_name": "lookup_order", "cause": "not_found", "cause_text": "what it asked for wasn't found",
        "error_pattern": "order <N> not found", "runs": 6, "failures": 7, "content": True,
        "evidence": [{"trace_id": TRACE, "name": "lookup_order"}, {"trace_id": "nope", "name": "x"}]}


def sha(s):
    return hashlib.sha256(s.encode()).hexdigest()


class _Bindle(BaseHTTPRequestHandler):
    notes = {}
    pushed = []
    auth = []

    def log_message(self, *a):
        pass

    def _reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.auth.append(self.headers.get("Authorization"))
        if self.path.startswith("/v1/lessons/les_0123456789abcdef/pack"):
            return self._reply(200, PACK)
        if self.path.startswith("/v1/lessons"):
            return self._reply(200, {"lessons": [{"id": "les_0123456789abcdef", "agent": "refund-agent", "title": "lookup_order failed in 6 runs"}]})
        if self.path.startswith("/v1/vault"):
            files = [{"path": p, "content": c, "sha256": sha(c), "kind": "note", "version": 1} for p, c in sorted(self.notes.items())]
            return self._reply(200, {"agent": "refund-agent", "files": files})
        self._reply(404, {"error": "no such endpoint"})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.pushed.append(body)
        if "send_email" in body["content"]:
            return self._reply(422, {"error": "skill rejected", "passport_check": {"reason": "tools not in the passport: send_email"}})
        self._reply(201, {"path": "skills/lookup-order-not-found/SKILL.md", "version": 1, "status": "draft",
                          "scan": {"flagged": False, "reasons": []}, "passport_check": {"passport": True, "ok": True}})

    def do_PUT(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        cur = self.notes.get(body["path"])
        if (cur is None and body["base_sha256"] != "") or (cur is not None and sha(cur) != body["base_sha256"]):
            return self._reply(409, {"error": "conflict", "current_sha256": sha(cur or "")})
        self.notes[body["path"]] = body["content"]
        self._reply(201, {"path": body["path"], "version": 1, "sha256": sha(body["content"])})


@pytest.fixture()
def bindle_server(monkeypatch):
    _Bindle.notes, _Bindle.pushed, _Bindle.auth = {}, [], []
    srv = HTTPServer(("127.0.0.1", 0), _Bindle)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    monkeypatch.setenv("BINDLE_ENDPOINT", f"http://127.0.0.1:{srv.server_port}")
    monkeypatch.setenv("BINDLE_MGMT_KEY", KEY)
    yield srv
    srv.shutdown()


def test_build_skill_follows_the_spec():
    name, text = build_skill(PACK, {"description": "Retry lookup_order once: trim the ID first.", "body": "1. Trim.\n2. Retry."})
    assert name == "lookup-order-not-found"
    assert text.startswith("---\nname: lookup-order-not-found\ndescription: \"Retry lookup_order once: trim the ID first.\"\n")
    assert "  bindle-lesson: les_0123456789abcdef" in text and "bindle-status: draft" in text
    assert f"[[run:{TRACE}]]" in text and "[[run:nope]]" not in text
    for bad in ({"body": "x"}, {"description": "d"}, {"description": "d" * 1025, "body": "x"}, "not a dict",
                {"description": "d", "body": "x\n" * 600}):
        with pytest.raises(SkillError):
            build_skill(PACK, bad)
    assert skill_name(PACK, "Retry  Lookup!!") == "retry-lookup"
    with pytest.raises(SkillError):
        skill_name(PACK, "---")


def test_lessons_draft_push(bindle_server, tmp_path, monkeypatch, capsys):
    mod = tmp_path / "mywriter.py"
    mod.write_text("seen = []\n"
                   "def draft(pack):\n"
                   "    seen.append(pack)\n"
                   "    return {'description': 'Retry lookup_order with a trimmed ID.', 'body': 'Call lookup_order again.'}\n")
    monkeypatch.syspath_prepend(str(tmp_path))
    monkeypatch.chdir(tmp_path)
    assert _cli.main(["skills", "lessons", "--agent", "refund-agent"]) == 0
    assert "les_0123456789abcdef" in capsys.readouterr().out
    assert _cli.main(["skills", "draft", "--lesson", "les_0123456789abcdef", "--writer", "mywriter:draft", "--out", "out"]) == 0
    md = tmp_path / "out" / "lookup-order-not-found" / "SKILL.md"
    assert md.exists() and sys.modules["mywriter"].seen[0]["lesson"] == "les_0123456789abcdef"
    assert _cli.main(["skills", "push", str(md.parent), "--agent", "refund-agent"]) == 0
    assert _Bindle.pushed[0]["agent"] == "refund-agent" and _Bindle.pushed[0]["content"] == md.read_text()
    md.write_text(md.read_text().replace("Call lookup_order again.", "Then call send_email."))
    assert _cli.main(["skills", "push", str(md), "--agent", "refund-agent"]) == 1
    assert "send_email" in capsys.readouterr().err
    assert all(a == f"Bearer {KEY}" for a in _Bindle.auth)
    assert _cli.main(["skills", "draft", "--lesson", "bad", "--writer", "mywriter:draft"]) == 2


def test_vault_pull_push_conflicts(bindle_server, tmp_path, capsys):
    d = tmp_path / "vault"
    _Bindle.notes = {"notes/a.md": "first", "skills/x/SKILL.md": "skill v1"}
    assert _cli.main(["vault", "pull", "--agent", "refund-agent", "--dir", str(d)]) == 0
    assert (d / "notes" / "a.md").read_text() == "first"
    state = json.loads((d / STATE_FILE).read_text())
    assert state["agent"] == "refund-agent" and state["files"]["notes/a.md"] == sha("first")

    # Local edit pushes with the pulled version as the base.
    (d / "notes" / "a.md").write_text("second")
    (d / "notes" / "new.md").write_text("brand new")
    assert _cli.main(["vault", "push", "--agent", "refund-agent", "--dir", str(d)]) == 0
    assert _Bindle.notes["notes/a.md"] == "second" and _Bindle.notes["notes/new.md"] == "brand new"
    assert _cli.main(["vault", "push", "--agent", "refund-agent", "--dir", str(d)]) == 0  # nothing changed

    # Changed on both sides: push reports a conflict, pull leaves the local file alone.
    _Bindle.notes["notes/a.md"] = "changed in bindle"
    (d / "notes" / "a.md").write_text("changed here")
    assert _cli.main(["vault", "push", "--agent", "refund-agent", "--dir", str(d)]) == 1
    assert _cli.main(["vault", "pull", "--agent", "refund-agent", "--dir", str(d)]) == 1
    assert (d / "notes" / "a.md").read_text() == "changed here"
    assert "Conflict: notes/a.md" in capsys.readouterr().err

    # Another agent's state file is refused; export needs an empty folder.
    assert _cli.main(["vault", "pull", "--agent", "other", "--dir", str(d)]) == 2
    assert _cli.main(["vault", "export", "--agent", "refund-agent", "--dir", str(d)]) == 2
    out = tmp_path / "export"
    assert _cli.main(["vault", "export", "--agent", "refund-agent", "--dir", str(out)]) == 0
    assert (out / "notes" / "new.md").read_text() == "brand new" and not (out / STATE_FILE).exists()


def test_unsafe_server_paths_are_skipped(bindle_server, tmp_path):
    _Bindle.notes = {"../escape.md": "x", "notes/ok.md": "y"}
    d = tmp_path / "v"
    assert _cli.main(["vault", "export", "--agent", "refund-agent", "--dir", str(d)]) == 0
    assert (d / "notes" / "ok.md").exists() and not (tmp_path / "escape.md").exists()


def test_needs_a_management_key(monkeypatch, tmp_path):
    monkeypatch.chdir(tmp_path)
    monkeypatch.setenv("BINDLE_ENDPOINT", "http://127.0.0.1:9")
    monkeypatch.setenv("BINDLE_MGMT_KEY", "bd_live_" + "a" * 40)
    assert _cli.main(["skills", "lessons"]) == 2
    monkeypatch.setenv("BINDLE_MGMT_KEY", KEY)
    monkeypatch.delenv("BINDLE_ENDPOINT")
    assert _cli.main(["vault", "pull", "--agent", "a", "--dir", str(tmp_path / "x")]) == 2


def test_help_documents_usage(capsys):
    with pytest.raises(SystemExit):
        _cli.main(["skills", "--help"])
    out = capsys.readouterr().out
    assert "--writer" in out or "bindle skills draft" in out


def test_writer_failure_is_reported(bindle_server, tmp_path, monkeypatch, capsys):
    (tmp_path / "badwriter.py").write_text("def draft(pack):\n    raise RuntimeError('model quota')\n")
    monkeypatch.syspath_prepend(str(tmp_path))
    assert _cli.main(["skills", "draft", "--lesson", "les_0123456789abcdef", "--writer", "badwriter:draft", "--out", str(tmp_path / "o")]) == 1
    assert "model quota" in capsys.readouterr().err
    assert not Path(tmp_path / "o").exists()
