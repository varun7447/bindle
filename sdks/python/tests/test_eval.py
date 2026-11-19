import io
import json
import pathlib
import sys

import pytest

from bindle import _eval
from bindle._eval import Run, evaluate

VECTORS = pathlib.Path(__file__).resolve().parents[3] / "backend" / "internal" / "evals" / "testdata" / "check_vectors.json"


def test_checks_match_the_server():
    v = json.loads(VECTORS.read_text())
    for i, c in enumerate(v["cases"]):
        run = Run(**{**v["run"], **c.get("run", {})})
        got = evaluate(c["scorer"], run)
        assert (got.passed, got.skipped) == (c["pass"], c["skipped"]), f"vector {i} {c['scorer']['kind']}: {got}"


@pytest.fixture
def project(tmp_path, monkeypatch):
    # bindle eval installs a global tracer provider when none exists; put
    # OpenTelemetry's global state back so other tests start clean.
    from opentelemetry import trace

    monkeypatch.setattr(trace, "_TRACER_PROVIDER", trace._TRACER_PROVIDER)
    monkeypatch.setattr(trace._TRACER_PROVIDER_SET_ONCE, "_done", trace._TRACER_PROVIDER_SET_ONCE._done)
    monkeypatch.chdir(tmp_path)
    monkeypatch.syspath_prepend(str(tmp_path))
    (tmp_path / "evalagent.py").write_text(
        "def good(text):\n    return {'output': 'Your refund was issued for ' + text, 'tools': ['lookup_order']}\n"
        "def bad(text):\n    return 'Sorry, I cannot help.'\n"
        "def boom(text):\n    raise RuntimeError('model down')\n"
        "def judge(line, answer):\n    return {'helpful': 'refund' in answer, 'polite': 0.5}\n"
    )
    lines = [{"trace_id": "a" * 32, "agent": "refunds", "input": "order 1", "output": "refund issued", "tools": ["lookup_order"], "steps": 4},
             {"trace_id": "b" * 32, "agent": "refunds", "input": "order 2", "output": "refund issued", "tools": ["lookup_order"], "steps": 4}]
    (tmp_path / "ds.jsonl").write_text("\n".join(json.dumps(x) for x in lines) + "\n")
    (tmp_path / "checks.json").write_text(json.dumps({"scorers": [
        {"name": "mentions refund", "kind": "contains", "value": "refund", "enabled": True},
        {"name": "looked up", "kind": "tool_called", "value": "lookup_order", "enabled": True},
        {"name": "off", "kind": "json_valid", "enabled": False},
    ]}))
    for m in ["evalagent"]:
        sys.modules.pop(m, None)
    return tmp_path


def test_eval_passes_and_reports(project):
    out = io.StringIO()
    code = _eval.run("ds.jsonl", "checks.json", "evalagent:good", "evalagent:judge", min_pass_rate=0.9, report="r.md", out=out)
    text = out.getvalue()
    assert code == 0, text
    assert "Bindle eval: passed" in text and "| mentions refund | 100% | 100% | 0 |" in text
    assert "| helpful | 1.00 |" in text and "| polite | 0.50 |" in text
    assert (project / "r.md").read_text() == text


def test_eval_fails_on_regression(project):
    out = io.StringIO()
    code = _eval.run("ds.jsonl", "checks.json", "evalagent:bad", max_drop=0.05, out=out)
    text = out.getvalue()
    assert code == 1
    assert "pass rate dropped from 100% to 0%" in text
    assert "mentions refund: output doesn't contain 'refund'" in text


def test_eval_counts_a_crash_as_a_failed_run(project):
    (project / "checks.json").write_text(json.dumps([{"name": "ok", "kind": "run_succeeded"}]))
    out = io.StringIO()
    assert _eval.run("ds.jsonl", "checks.json", "evalagent:boom", min_pass_rate=1.0, out=out) == 1
    assert "ok: a step failed" in out.getvalue()


def test_eval_rejects_bad_specs(project):
    with pytest.raises(SystemExit):
        _eval.run("ds.jsonl", "checks.json", "evalagent", out=io.StringIO())
    (project / "empty.json").write_text(json.dumps({"scorers": []}))
    with pytest.raises(SystemExit):
        _eval.run("ds.jsonl", "empty.json", "evalagent:good", out=io.StringIO())
