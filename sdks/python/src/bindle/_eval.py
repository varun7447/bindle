"""``bindle eval``: grade an agent on a dataset in CI, before it ships.

    bindle eval --dataset refunds.jsonl --checks checks.json --agent my_app.agent:answer

* ``--dataset`` is a dataset exported from Bindle (Evals → dataset → Export
  JSONL): one run per line with the original input and output.
* ``--checks`` is the checks file from Bindle (Evals → Download checks), the
  same checks Bindle runs on live traffic.
* ``--agent`` is ``module:function``. It's called with each line's input and
  returns the answer (a string), or a dict with ``output`` and optionally
  ``tools``. Tool calls and steps are also counted from the spans the agent
  emits while it runs.
* ``--judge`` (optional) is ``module:function`` called with the dataset line
  and the new answer; it returns a number, a bool, or a dict of name → number
  or bool. It runs here, with your own model credentials: Bindle never sees
  them. If Bindle is configured (BINDLE_API_KEY), judge results are sent with
  ``bindle.score()``.

Each line is graded twice: the dataset's original answer (the baseline) and
the new one. The command fails (exit 1) when the new pass rate is below
``--min-pass-rate`` or more than ``--max-drop`` below the baseline. A Markdown
report is printed and, with ``--report``, written to a file (for example a
pull-request comment).
"""

from __future__ import annotations

import importlib
import json
import re
import sys
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional

# ---- checks (the same rules as backend/internal/evals; see check_vectors.json) ----

_MAX_SCAN = 1 << 20
_TEXT_KINDS = {"contains", "not_contains", "regex", "json_valid"}


@dataclass
class Run:
    output: str = ""
    tools: List[str] = field(default_factory=list)
    steps: int = 0
    cost_usd: float = 0.0
    cost_known: bool = False
    duration_ms: float = 0.0
    failed: bool = False


@dataclass
class Result:
    passed: bool = False
    skipped: bool = False
    detail: str = ""


def _num(v: float) -> str:
    return ("%f" % v).rstrip("0").rstrip(".") if v != int(v) else str(int(v))


def evaluate(scorer: Dict[str, Any], run: Run) -> Result:
    """One check on one run. Skipped results don't count toward pass rates."""
    kind = scorer.get("kind", "")
    value = str(scorer.get("value", "") or "")
    number = float(scorer.get("number", 0) or 0)
    case = bool(scorer.get("case_sensitive", False))
    out = (run.output or "")[:_MAX_SCAN]
    fold = (lambda s: s) if case else (lambda s: s.lower())
    if kind in _TEXT_KINDS and not out.strip():
        return Result(skipped=True, detail="no output captured")
    if kind == "contains":
        return Result(True) if fold(value) in fold(out) else Result(detail=f"output doesn't contain {value!r}")
    if kind == "not_contains":
        return Result(True) if fold(value) not in fold(out) else Result(detail=f"output contains {value!r}")
    if kind == "regex":
        try:
            rx = re.compile(value, 0 if case else re.IGNORECASE)
        except re.error:
            return Result(skipped=True, detail="invalid pattern")
        return Result(True) if rx.search(out) else Result(detail="output doesn't match the pattern")
    if kind == "json_valid":
        try:
            json.loads(out.strip())
            return Result(True)
        except ValueError:
            return Result(detail="output isn't valid JSON")
    if kind in ("tool_called", "tool_not_called"):
        called = value in run.tools
        if called == (kind == "tool_called"):
            return Result(True)
        return Result(detail=f"{value} was called" if called else f"{value} wasn't called")
    if kind == "max_steps":
        return Result(True) if run.steps <= number else Result(detail=f"{run.steps} steps, over {_num(number)}")
    if kind == "max_cost_usd":
        if not run.cost_known:
            return Result(skipped=True, detail="some model calls have no known price")
        return Result(True) if run.cost_usd <= number else Result(detail=f"cost ${_num(run.cost_usd)}, over ${_num(number)}")
    if kind == "max_duration_ms":
        return Result(True) if run.duration_ms <= number else Result(detail=f"took {round(run.duration_ms)} ms, over {_num(number)} ms")
    if kind == "run_succeeded":
        return Result(True) if not run.failed else Result(detail="a step failed")
    return Result(skipped=True, detail="unknown check")


@dataclass
class Tally:
    name: str
    passed: int = 0
    failed: int = 0
    skipped: int = 0

    def add(self, r: Result) -> None:
        if r.skipped:
            self.skipped += 1
        elif r.passed:
            self.passed += 1
        else:
            self.failed += 1

    def rate(self) -> Optional[float]:
        n = self.passed + self.failed
        return self.passed / n if n else None


def overall(tallies: List[Tally]) -> Optional[float]:
    p = sum(t.passed for t in tallies)
    n = p + sum(t.failed for t in tallies)
    return p / n if n else None


# ---- running the agent ------------------------------------------------------------


def _load(spec: str) -> Callable:
    if ":" not in spec:
        raise SystemExit(f"bindle eval: {spec!r} must be module:function")
    mod, fn = spec.split(":", 1)
    sys.path.insert(0, ".")
    obj = importlib.import_module(mod)
    for part in fn.split("."):
        obj = getattr(obj, part)
    if not callable(obj):
        raise SystemExit(f"bindle eval: {spec} isn't callable")
    return obj


try:
    from opentelemetry.sdk.trace import SpanProcessor as _SpanProcessor
except Exception:  # noqa: BLE001
    _SpanProcessor = object  # type: ignore[misc,assignment]


class _Collector(_SpanProcessor):
    """Counts steps and tool calls per trace while the agent runs."""

    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.runs: Dict[int, Dict[str, Any]] = {}

    def on_start(self, span, parent_context=None) -> None:
        with self.lock:
            r = self.runs.setdefault(span.context.trace_id, {"steps": 0, "tools": []})
            r["steps"] += 1

    def on_end(self, span) -> None:
        attrs = span.attributes or {}
        tool = attrs.get("gen_ai.tool.name") or attrs.get("tool.name")
        if tool:
            with self.lock:
                self.runs.setdefault(span.context.trace_id, {"steps": 0, "tools": []})["tools"].append(str(tool))

    def shutdown(self) -> None:
        pass

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        return True


def _install_collector() -> Optional[_Collector]:
    try:
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider

        provider = trace.get_tracer_provider()
        if not isinstance(provider, TracerProvider):
            provider = TracerProvider()
            trace.set_tracer_provider(provider)
        c = _Collector()
        provider.add_span_processor(c)
        return c
    except Exception:  # noqa: BLE001
        return None


def _as_run(line: Dict[str, Any]) -> Run:
    return Run(output=str(line.get("output") or ""), tools=list(line.get("tools") or []), steps=int(line.get("steps") or 0),
               cost_usd=float(line.get("cost_usd") or 0), cost_known=bool(line.get("cost_known")),
               duration_ms=float(line.get("duration_ms") or 0), failed=bool(line.get("failed")))


def run_agent(agent: Callable, line: Dict[str, Any], collector: Optional[_Collector]) -> Run:
    from opentelemetry import trace

    tracer = trace.get_tracer("bindle.eval")
    start = time.monotonic()
    failed = False
    output: Any = ""
    with tracer.start_as_current_span("bindle eval") as span:
        tid = span.get_span_context().trace_id
        try:
            output = agent(line.get("input", ""))
        except Exception as exc:  # noqa: BLE001 - a crash is a failed run, not a failed eval
            failed, output = True, f"{type(exc).__name__}: {exc}"
    duration = (time.monotonic() - start) * 1000
    tools: List[str] = []
    if isinstance(output, dict):
        tools = [str(t) for t in output.get("tools") or []]
        output = output.get("output", "")
    seen = collector.runs.pop(tid, {"steps": 0, "tools": []}) if collector else {"steps": 0, "tools": []}
    return Run(output=str(output if output is not None else ""), tools=tools or seen["tools"], steps=max(seen["steps"] - 1, 0),
               cost_usd=0.0, cost_known=False, duration_ms=duration, failed=failed)


# ---- the command ----------------------------------------------------------------------


def _pct(v: Optional[float]) -> str:
    return "–" if v is None else f"{v * 100:.0f}%"


def run(dataset: str, checks: str, agent_spec: str, judge_spec: Optional[str] = None, min_pass_rate: Optional[float] = None,
        max_drop: float = 0.0, report: Optional[str] = None, out=sys.stdout) -> int:
    lines = [json.loads(x) for x in open(dataset, encoding="utf-8") if x.strip()]
    raw = json.load(open(checks, encoding="utf-8"))
    scorers = [s for s in (raw.get("scorers", raw) if isinstance(raw, dict) else raw) if s.get("enabled", True)]
    if not lines:
        raise SystemExit("bindle eval: the dataset is empty")
    if not scorers:
        raise SystemExit("bindle eval: no enabled checks in the checks file")
    agent = _load(agent_spec)
    judge = _load(judge_spec) if judge_spec else None
    collector = _install_collector()

    import bindle

    base = {s["name"]: Tally(s["name"]) for s in scorers}
    new = {s["name"]: Tally(s["name"]) for s in scorers}
    judged: Dict[str, List[float]] = {}
    failures: List[str] = []
    for i, line in enumerate(lines, 1):
        before, after = _as_run(line), run_agent(agent, line, collector)
        for s in scorers:
            if s.get("agent") and line.get("agent") and s["agent"] != line["agent"]:
                continue
            base[s["name"]].add(evaluate(s, before))
            r = evaluate(s, after)
            new[s["name"]].add(r)
            if not r.passed and not r.skipped:
                failures.append(f"line {i} ({line.get('trace_id', '')[:12]}): {s['name']}: {r.detail}")
        if judge:
            try:
                verdict = judge(line, after.output)
            except Exception as exc:  # noqa: BLE001
                failures.append(f"line {i}: judge failed: {type(exc).__name__}: {exc}")
                continue
            items = verdict.items() if isinstance(verdict, dict) else [("judge", verdict)]
            for name, v in items:
                score = 1.0 if v is True else 0.0 if v is False else float(v)
                judged.setdefault(str(name), []).append(score)
                if line.get("trace_id"):
                    bindle.score(str(name), score, passed=v if isinstance(v, bool) else None, trace_id=line["trace_id"])
    bindle.flush()

    b_rate, n_rate = overall(list(base.values())), overall(list(new.values()))
    ok = True
    reasons = []
    if min_pass_rate is not None and (n_rate is None or n_rate < min_pass_rate):
        ok = False
        reasons.append(f"pass rate {_pct(n_rate)} is below the minimum {_pct(min_pass_rate)}")
    if b_rate is not None and n_rate is not None and n_rate < b_rate - max_drop:
        ok = False
        reasons.append(f"pass rate dropped from {_pct(b_rate)} to {_pct(n_rate)}")

    md = [f"## Bindle eval: {'passed' if ok else 'failed'}", "",
          f"{len(lines)} runs from `{dataset}`, {len(scorers)} checks. Pass rate **{_pct(n_rate)}** (baseline {_pct(b_rate)}).", ""]
    if reasons:
        md += [f"- {r}" for r in reasons] + [""]
    md += ["| Check | Baseline | This change | Skipped |", "|---|---:|---:|---:|"]
    for s in scorers:
        b, n = base[s["name"]], new[s["name"]]
        md.append(f"| {s['name']} | {_pct(b.rate())} | {_pct(n.rate())} | {n.skipped} |")
    if judged:
        md += ["", "| Judge | Average |", "|---|---:|"]
        md += [f"| {k} | {sum(v) / len(v):.2f} |" for k, v in sorted(judged.items())]
    if failures:
        md += ["", "<details><summary>Failures</summary>", ""] + [f"- {f}" for f in failures[:200]] + ["", "</details>"]
    text = "\n".join(md) + "\n"
    out.write(text)
    if report:
        open(report, "w", encoding="utf-8").write(text)
    return 0 if ok else 1
