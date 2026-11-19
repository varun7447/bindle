"""Agent limits set in the Bindle console: a kill switch, per-run caps on
steps and tokens, and daily budgets.

Bindle never stops an agent behind its back. Limits take effect where the
agent's code calls ``bindle.check_limits()`` (for example at the top of each
loop iteration or tool call), which raises ``bindle.LimitExceeded`` when the
agent is paused, over today's budget (if its limit pauses it), or when the
current run has used more steps or tokens than allowed.

Run usage is counted in-process by a span processor: every span started in
a trace is one step, and token usage is read from LLM spans as they end.
"""

from __future__ import annotations

import threading
from collections import OrderedDict
from dataclasses import dataclass
from typing import Iterable, Optional, Tuple

from opentelemetry import trace

try:
    from opentelemetry.sdk.trace import SpanProcessor
except Exception:  # noqa: BLE001 - the SDK is a hard dependency; this keeps imports safe
    SpanProcessor = object  # type: ignore[misc,assignment]

ANY_AGENT = "*"
_MAX_TRACES = 2048

_LLM_OPS = {"chat", "text_completion", "generate_content", "embeddings"}
_NOT_LLM_OPS = {"invoke_agent", "create_agent", "execute_tool", "invoke_workflow"}
_NOT_LLM_KINDS = {"AGENT", "CHAIN", "TOOL", "RETRIEVER", "RERANKER", "GUARDRAIL", "EVALUATOR"}


class LimitExceeded(Exception):
    """Raised by ``bindle.check_limits()``. ``reason`` is one of "paused",
    "over_budget", "max_steps_per_run" or "max_tokens_per_run"."""

    def __init__(self, reason: str, agent: str, message: str, limit: float = 0, value: float = 0):
        super().__init__(message)
        self.reason, self.agent, self.limit, self.value = reason, agent, limit, value


@dataclass(frozen=True)
class Limit:
    agent: str
    paused: bool = False
    max_steps_per_run: int = 0
    max_tokens_per_run: int = 0
    daily_budget_usd: float = 0.0
    pause_over_budget: bool = False


def parse_limits(raw) -> Tuple[Limit, ...]:
    """Tolerant parse of the config's "limits" list; bad rows are skipped."""
    out = []
    for d in raw if isinstance(raw, list) else []:
        try:
            agent = d.get("agent")
            if not isinstance(agent, str) or not agent:
                continue
            out.append(Limit(
                agent=agent,
                paused=d.get("paused") is True,
                max_steps_per_run=max(int(d.get("max_steps_per_run") or 0), 0),
                max_tokens_per_run=max(int(d.get("max_tokens_per_run") or 0), 0),
                daily_budget_usd=max(float(d.get("daily_budget_usd") or 0), 0.0),
                pause_over_budget=d.get("pause_over_budget") is True,
            ))
        except Exception:  # noqa: BLE001
            continue
    return tuple(out)


def limit_for(limits: Iterable[Limit], agent: str) -> Optional[Limit]:
    """The agent's own limit, else the "*" limit, else None."""
    default = None
    for lim in limits:
        if lim.agent == agent and agent != ANY_AGENT:
            return lim
        if lim.agent == ANY_AGENT:
            default = lim
    return default


def _is_llm(attrs) -> bool:
    op = str(attrs.get("gen_ai.operation.name", "")).lower()
    kind = str(attrs.get("openinference.span.kind", "")).upper()
    if op in _LLM_OPS or kind in ("LLM", "EMBEDDING"):
        return True
    if op in _NOT_LLM_OPS or kind in _NOT_LLM_KINDS:
        return False
    return bool(attrs.get("gen_ai.request.model") or attrs.get("llm.model_name"))


def _tokens(attrs) -> int:
    total = 0
    for k in ("gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens",
              "llm.token_count.prompt", "llm.token_count.completion"):
        try:
            total += int(attrs.get(k) or 0)
        except (TypeError, ValueError):
            pass
    if total == 0:
        for k in ("gen_ai.usage.total_tokens", "llm.token_count.total"):
            try:
                total = int(attrs.get(k) or 0)
            except (TypeError, ValueError):
                total = 0
            if total:
                break
    return total


class RunCounter(SpanProcessor):
    """Counts steps and tokens per trace (bounded: oldest traces drop off)."""

    def __init__(self, max_traces: int = _MAX_TRACES):
        self._lock = threading.Lock()
        self._runs: "OrderedDict[int, list]" = OrderedDict()
        self._max = max_traces

    def _entry(self, trace_id: int) -> list:
        e = self._runs.get(trace_id)
        if e is None:
            e = self._runs[trace_id] = [0, 0]
            if len(self._runs) > self._max:
                self._runs.popitem(last=False)
        else:
            self._runs.move_to_end(trace_id)
        return e

    def on_start(self, span, parent_context=None) -> None:
        try:
            with self._lock:
                self._entry(span.context.trace_id)[0] += 1
        except Exception:  # noqa: BLE001 - never break the host app
            pass

    def on_end(self, span) -> None:
        try:
            attrs = span.attributes or {}
            if _is_llm(attrs):
                n = _tokens(attrs)
                if n:
                    with self._lock:
                        self._entry(span.context.trace_id)[1] += n
        except Exception:  # noqa: BLE001
            pass

    def usage(self, trace_id: int) -> Tuple[int, int]:
        with self._lock:
            e = self._runs.get(trace_id)
            return (e[0], e[1]) if e else (0, 0)

    def shutdown(self) -> None:
        pass

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        return True


class Guard:
    """Holds the current limits and checks them for the current run."""

    def __init__(self, agent: str, counter: RunCounter):
        self.agent = agent or ""
        self.counter = counter
        self._limits: Tuple[Limit, ...] = ()
        self._over_budget: frozenset = frozenset()

    def update(self, limits: Tuple[Limit, ...], over_budget: Iterable[str]) -> None:
        self._limits, self._over_budget = tuple(limits), frozenset(over_budget)

    def status(self) -> dict:
        lim = limit_for(self._limits, self.agent)
        span = trace.get_current_span()
        ctx = span.get_span_context() if span is not None else None
        steps, tokens = self.counter.usage(ctx.trace_id) if ctx is not None and ctx.is_valid else (0, 0)
        return {
            "agent": self.agent,
            "paused": bool(lim and lim.paused),
            "over_budget": self.agent in self._over_budget,
            "pause_over_budget": bool(lim and lim.pause_over_budget),
            "max_steps_per_run": lim.max_steps_per_run if lim else 0,
            "max_tokens_per_run": lim.max_tokens_per_run if lim else 0,
            "run_steps": steps,
            "run_tokens": tokens,
        }

    def check(self) -> None:
        s = self.status()
        who = self.agent or "this agent"
        if s["paused"]:
            raise LimitExceeded("paused", self.agent, f"{who} is paused in Bindle")
        if s["over_budget"] and s["pause_over_budget"]:
            raise LimitExceeded("over_budget", self.agent, f"{who} is over its daily budget in Bindle (paused until 00:00 UTC)")
        if s["max_steps_per_run"] and s["run_steps"] > s["max_steps_per_run"]:
            raise LimitExceeded("max_steps_per_run", self.agent,
                                f"run used {s['run_steps']} steps, over the limit of {s['max_steps_per_run']}",
                                s["max_steps_per_run"], s["run_steps"])
        if s["max_tokens_per_run"] and s["run_tokens"] > s["max_tokens_per_run"]:
            raise LimitExceeded("max_tokens_per_run", self.agent,
                                f"run used {s['run_tokens']} tokens, over the limit of {s['max_tokens_per_run']}",
                                s["max_tokens_per_run"], s["run_tokens"])
