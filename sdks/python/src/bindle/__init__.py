"""Bindle: observability for AI agents.

    import bindle
    bindle.init(api_key="bd_live_...", agent="support-triage", version="v12")

Every step your agent takes (LLM calls, tool calls, decisions, handoffs) is
traced with OpenTelemetry and sent to Bindle. Design rules:

* One line to start, no config file. Everything has a safe default.
* Never break the agent: export is async and bounded, errors are swallowed,
  and a missing API key turns the SDK into a no-op with one warning.
* Redaction is on by default and runs before data leaves the process.
"""

from __future__ import annotations

import atexit
import contextvars
import functools
import inspect
import json
import logging
import threading
from contextlib import contextmanager
from typing import Any, Callable, Iterator, List, Optional, Sequence, Union

from opentelemetry import trace
from opentelemetry.trace import Status, StatusCode

from . import _config, _correlate, _instrument
from ._redact import Redactor

from ._cassette import Cassette, RecordedToolError, UnrecordedCall
from ._limits import LimitExceeded
from ._links import inject_mcp_meta, mcp_server_context

__all__ = [
    "init", "flush", "shutdown", "step", "trace_step", "trace_tool",
    "decision", "handoff", "score", "feedback", "outcome", "correlate", "current_trace_id", "set_session", "check_limits", "limits_status",
    "LimitExceeded", "Redactor", "context_token", "resume", "inject_mcp_meta", "mcp_server_context",
    "Cassette", "UnrecordedCall", "RecordedToolError",
]
__version__ = "0.1.0"

log = logging.getLogger("bindle")
_TRACER_NAME = "bindle"
_MAX_CAPTURE = 16 * 1024

_lock = threading.Lock()
_session_var: "contextvars.ContextVar[Optional[str]]" = contextvars.ContextVar("bindle_session", default=None)
_state: dict = {
    "provider": None, "config": None, "session": threading.local(),
    "sampler": None, "exporter": None, "poller": None, "remote": None, "guard": None,
    "capture": True,  # effective content capture (local AND remote)
}


def init(
    api_key: Optional[str] = None,
    *,
    agent: Optional[str] = None,
    version: Optional[str] = None,
    environment: Optional[str] = None,
    endpoint: Optional[str] = None,
    sample_rate: Optional[float] = None,
    redact: Optional[bool] = None,
    capture_content: Optional[bool] = None,
    instrument: Union[bool, List[str], None] = None,
    redact_patterns: Optional[dict] = None,
    debug: Optional[bool] = None,
    remote_config: Optional[bool] = None,
) -> bool:
    """Start sending traces to Bindle. Returns True if tracing is active.

    All arguments are optional and fall back to environment variables:
    BINDLE_API_KEY, BINDLE_ENDPOINT, BINDLE_AGENT, BINDLE_AGENT_VERSION,
    BINDLE_ENVIRONMENT, BINDLE_SAMPLE_RATE, BINDLE_REDACT,
    BINDLE_CAPTURE_CONTENT, BINDLE_AUTO_INSTRUMENT, BINDLE_DEBUG, BINDLE_REMOTE_CONFIG.

    Settings changed in the Bindle console (sampling, content capture, extra
    redaction patterns) are fetched in the background and act as a ceiling:
    code can only be stricter.
    """
    cfg = _config.resolve(api_key, endpoint, agent, version, environment, sample_rate,
                          redact, capture_content, instrument, debug, redact_patterns, remote_config)
    if cfg.debug:
        logging.basicConfig()
        log.setLevel(logging.DEBUG)
    if not cfg.api_key:
        log.warning("bindle: no API key (set BINDLE_API_KEY); tracing is disabled")
        return False
    with _lock:
        if _state["provider"] is not None:
            log.debug("bindle: already initialized")
            return True
        try:
            provider = _setup(cfg)
        except Exception as exc:  # noqa: BLE001 - never break the host app
            log.warning("bindle: init failed, tracing disabled: %s", exc)
            return False
        _state["provider"], _state["config"], _state["capture"] = provider, cfg, cfg.capture_content
        if cfg.remote_config:
            _start_remote(cfg)
    atexit.register(shutdown)
    return True


def _local_redactor(cfg: _config.Config, remote_patterns: Optional[dict] = None) -> Redactor:
    rules = None if cfg.redact else []
    extra = dict(cfg.extra_redact_patterns) if cfg.redact else {}
    extra.update(remote_patterns or {})  # remote patterns always add, never remove
    return Redactor(rules=rules, extra_patterns=extra)


def _apply_remote(settings) -> None:
    """Merge remote settings with local ones: the stricter value wins."""
    cfg = _state["config"]
    if cfg is None:
        return
    rate = min(cfg.sample_rate, settings.sample_rate)
    capture = cfg.capture_content and settings.capture_content
    sampler, exporter = _state["sampler"], _state["exporter"]
    if sampler is not None:
        sampler.set_rate(rate)
    if exporter is not None:
        exporter.update(capture, _local_redactor(cfg, settings.redact_patterns),
                        "sdk" if not cfg.capture_content else "workspace")
    _state["capture"], _state["remote"] = capture, settings
    guard = _state["guard"]
    if guard is not None:
        guard.update(settings.limits, settings.over_budget, settings.stopped_runs)
    log.debug("bindle: remote config v%s applied (sample_rate=%s, capture_content=%s, patterns=%d)",
              settings.version, rate, capture, len(settings.redact_patterns))


def _start_remote(cfg: _config.Config) -> None:
    try:
        from ._remote import Poller

        poller = Poller(cfg.config_url, cfg.api_key, _apply_remote, user_agent=f"bindle-sdk-python/{__version__}")
        poller.start()
        _state["poller"] = poller
    except Exception as exc:  # noqa: BLE001
        log.debug("bindle: remote config disabled: %s", exc)


def _setup(cfg: _config.Config):
    from opentelemetry.exporter.otlp.proto.http import Compression
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor
    from opentelemetry.sdk.trace.sampling import ParentBased

    from ._export import RedactingSpanExporter
    from ._sampling import DynamicRatioSampler

    attrs = {"bindle.sdk.version": __version__, "telemetry.sdk.language": "python"}
    if cfg.agent:
        attrs["service.name"] = cfg.agent
        attrs["bindle.agent.name"] = cfg.agent
    if cfg.version:
        attrs["bindle.agent.version"] = cfg.version
    if cfg.environment:
        attrs["deployment.environment.name"] = cfg.environment

    exporter = OTLPSpanExporter(
        endpoint=cfg.traces_url,
        headers={"authorization": f"Bearer {cfg.api_key}"},
        timeout=5,
        compression=Compression.Gzip,
    )
    # Always wrapped, so remote settings can tighten capture and redaction later.
    exporter = RedactingSpanExporter(exporter, _local_redactor(cfg), cfg.capture_content, resource_overlay=attrs)
    _state["exporter"] = exporter

    # Bounded queue: when full, spans are dropped instead of blocking the agent.
    processor = BatchSpanProcessor(
        exporter, max_queue_size=4096, schedule_delay_millis=1000,
        max_export_batch_size=512, export_timeout_millis=5000,
    )

    current = trace.get_tracer_provider()
    if isinstance(current, TracerProvider):
        # The app (or a framework) already configured OpenTelemetry: join it.
        current.add_span_processor(processor)
        provider = current
    else:
        # The app's own provider keeps its sampler; ingest still enforces
        # the console's sample rate for it.
        sampler = DynamicRatioSampler(cfg.sample_rate)
        _state["sampler"] = sampler
        provider = TracerProvider(resource=Resource.create(attrs), sampler=ParentBased(sampler))
        provider.add_span_processor(processor)
        trace.set_tracer_provider(provider)

    # Counts steps and tokens per run for bindle.check_limits().
    from ._limits import Guard, RunCounter

    counter = RunCounter()
    provider.add_span_processor(counter)
    provider.add_span_processor(_SessionTagger())
    _state["guard"] = Guard(cfg.agent or "", counter)

    enabled = _instrument.instrument(provider, cfg.instrument)
    log.debug("bindle: initialized (endpoint=%s, frameworks=%s)", cfg.endpoint, enabled)
    return provider


def flush(timeout_millis: int = 5000) -> bool:
    """Send buffered spans now. Call at the end of serverless handlers."""
    provider = _state["provider"]
    if provider is None:
        return True
    try:
        return bool(provider.force_flush(timeout_millis))
    except Exception:  # noqa: BLE001
        return False


def shutdown() -> None:
    poller = _state["poller"]
    if poller is not None:
        poller.stop()
        _state["poller"] = None
    provider = _state["provider"]
    if provider is None:
        return
    try:
        provider.shutdown()
    except Exception:  # noqa: BLE001
        pass
    _state["provider"] = None
    _state.update(sampler=None, exporter=None, remote=None, capture=True, guard=None)


def check_limits() -> None:
    """Raise ``bindle.LimitExceeded`` if this agent is paused in the Bindle
    console, over its daily budget (when its limit pauses it), or the current
    run has more steps or tokens than its limit allows.

    Call it where stopping is safe: at the top of each loop iteration, before
    a tool call or before an LLM call. Does nothing if Bindle isn't
    initialized or no limits are set.
    """
    guard = _state["guard"]
    if guard is None:
        return
    try:
        guard.check()
    except LimitExceeded:
        raise
    except Exception as exc:  # noqa: BLE001 - a bug here must not stop the agent
        log.debug("bindle: limit check failed: %s", exc)


def limits_status() -> dict:
    """The limits that apply to this agent and the current run's usage."""
    guard = _state["guard"]
    if guard is None:
        return {}
    try:
        return guard.status()
    except Exception:  # noqa: BLE001
        return {}


def set_session(session_id: Optional[str]) -> None:
    """Tag every span started from here on (in this thread or async task),
    including framework spans, with a session/conversation ID. Runs that
    share it show as one conversation under Sessions."""
    _state["session"].value = session_id
    _session_var.set(session_id or None)


def _current_session() -> Optional[str]:
    return _session_var.get() or getattr(_state["session"], "value", None)


try:
    from opentelemetry.sdk.trace import SpanProcessor as _SpanProcessor
except Exception:  # noqa: BLE001
    _SpanProcessor = object  # type: ignore[misc,assignment]


class _SessionTagger(_SpanProcessor):
    """Span processor: adds bindle.session.id to spans started in a session.
    Subclasses SpanProcessor so hooks newer SDKs call (e.g. _on_ending)
    always exist."""

    def on_start(self, span, parent_context=None) -> None:
        try:
            sid = _current_session()
            if sid and not (span.attributes or {}).get("bindle.session.id"):
                span.set_attribute("bindle.session.id", str(sid)[:256])
        except Exception:  # noqa: BLE001 - never break the host app
            pass

    def on_end(self, span) -> None:
        pass

    def shutdown(self) -> None:
        pass

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        return True


# ---------------------------------------------------------------------------
# Manual instrumentation for custom agent code that no framework covers.
# ---------------------------------------------------------------------------

def _preview(value: Any) -> str:
    try:
        text = value if isinstance(value, str) else json.dumps(value, default=repr)
    except Exception:  # noqa: BLE001
        text = repr(value)
    return text[:_MAX_CAPTURE]


def _capture() -> bool:
    return bool(_state["capture"])


@contextmanager
def step(name: str, type: str = "other", **attributes: Any) -> Iterator[trace.Span]:
    """Trace a block of code as one agent step.

        with bindle.step("plan", type="decision") as s:
            ...
    """
    tracer = trace.get_tracer(_TRACER_NAME, __version__)
    with tracer.start_as_current_span(name) as span:
        span.set_attribute("bindle.step.type", type)
        session = _current_session()
        if session:
            span.set_attribute("bindle.session.id", session)
        for k, v in attributes.items():
            if v is not None:
                span.set_attribute(k if "." in k else f"bindle.{k}", v if isinstance(v, (str, bool, int, float)) else _preview(v))
        try:
            yield span
        except Exception as exc:
            span.record_exception(exc)
            span.set_status(Status(StatusCode.ERROR, str(exc)))
            raise


def _wrap(fn: Callable, span_name: str, type: str, attrs: dict) -> Callable:
    def _record_input(span, args, kwargs):
        if _capture():
            span.set_attribute("input.value", _preview({"args": args, "kwargs": kwargs}))

    def _record_output(span, result):
        if _capture():
            span.set_attribute("output.value", _preview(result))

    if inspect.iscoroutinefunction(fn):
        @functools.wraps(fn)
        async def async_wrapper(*args, **kwargs):
            with step(span_name, type=type, **attrs) as span:
                _record_input(span, args, kwargs)
                result = await fn(*args, **kwargs)
                _record_output(span, result)
                return result
        return async_wrapper

    @functools.wraps(fn)
    def wrapper(*args, **kwargs):
        with step(span_name, type=type, **attrs) as span:
            _record_input(span, args, kwargs)
            result = fn(*args, **kwargs)
            _record_output(span, result)
            return result
    return wrapper


def trace_step(_fn: Optional[Callable] = None, *, type: str = "other", name: Optional[str] = None):
    """Decorator: trace a function (sync or async) as an agent step,
    capturing its arguments as input and its return value as output."""

    def decorate(fn: Callable) -> Callable:
        return _wrap(fn, name or fn.__qualname__, type, {})

    return decorate(_fn) if _fn is not None else decorate


def trace_tool(_fn: Optional[Callable] = None, *, name: Optional[str] = None):
    """Decorator for tools: a tool_call step tagged with the tool's name."""

    def decorate(fn: Callable) -> Callable:
        tool_name = name or fn.__name__
        return _wrap(fn, tool_name, "tool_call", {"bindle.tool.name": tool_name})

    return decorate(_fn) if _fn is not None else decorate


def decision(name: str, options: Sequence[str], chosen: str, rationale: Optional[str] = None) -> None:
    """Record a routing/planning decision as its own step."""
    with step(name, type="decision") as span:
        span.set_attribute("bindle.decision.options", list(options))
        span.set_attribute("bindle.decision.chosen", chosen)
        if rationale and _capture():
            span.set_attribute("bindle.reasoning", rationale[:_MAX_CAPTURE])


def score(name: str, value: Optional[float] = None, *, passed: Optional[bool] = None,
          comment: Optional[str] = None, trace_id: Union[str, int, None] = None, _source: Optional[str] = None) -> bool:
    """Record a score for a run: a number, pass/fail, or both.

    Inside a run, the score joins the current trace. From elsewhere (a CI
    job or an LLM judge grading an exported dataset), pass the run's
    ``trace_id`` (32 hex characters, as in the export) and the score is
    attached to that run. Bindle shows scores per agent version.

    Returns True if the score was recorded. Never raises.
    """
    try:
        import math
        import random

        from opentelemetry.trace import NonRecordingSpan, SpanContext, TraceFlags, set_span_in_context

        if _state["provider"] is None or not isinstance(name, str) or not name.strip() or len(name) > 80:
            return False
        attrs: dict = {"bindle.step.type": "eval", "bindle.score.name": name.strip()}
        if value is not None:
            v = float(value)
            if math.isnan(v) or math.isinf(v):
                return False
            attrs["bindle.score.value"] = v
        if passed is not None:
            attrs["bindle.score.passed"] = bool(passed)
        if value is None and passed is None:
            return False
        if comment and _capture():
            attrs["output.value"] = str(comment)[:_MAX_CAPTURE]
        if _source:
            attrs["bindle.score.source"] = _source
        ctx = None
        if trace_id is not None:
            tid = int(trace_id, 16) if isinstance(trace_id, str) else int(trace_id)
            if not 0 < tid < 1 << 128:
                return False
            parent = SpanContext(trace_id=tid, span_id=random.getrandbits(64) or 1, is_remote=True,
                                 trace_flags=TraceFlags(TraceFlags.SAMPLED))
            ctx = set_span_in_context(NonRecordingSpan(parent))
            attrs["bindle.score.external"] = True
        elif not trace.get_current_span().get_span_context().is_valid:
            log.debug("bindle: score() outside a run needs trace_id")
            return False
        tracer = _state["provider"].get_tracer(_TRACER_NAME, __version__)
        with tracer.start_as_current_span(f"score {name.strip()}", context=ctx, attributes=attrs):
            pass
        return True
    except Exception as exc:  # noqa: BLE001 - never break the host app
        log.debug("bindle: score failed: %s", exc)
        return False


def feedback(*, trace_id: Union[str, int, None] = None, thumbs_up: Optional[bool] = None,
             rating: Optional[float] = None, comment: Optional[str] = None) -> bool:
    """Record a user's feedback on a run: thumbs up/down, a rating, or both,
    with an optional comment (redacted like everything else).

    Typically called from your app's feedback handler with the trace ID the
    run returned (see ``current_trace_id()``). Feedback shows on the run, its
    session, and Home ("unhappy sessions"). Returns True if recorded.
    """
    value = rating if rating is not None else (None if thumbs_up is None else (1.0 if thumbs_up else 0.0))
    return score("feedback", value, passed=thumbs_up, comment=comment, trace_id=trace_id, _source="user")


def outcome(name: str, success: bool = True, value: Optional[float] = None, *,
            trace_id: Union[str, int, None] = None, comment: Optional[str] = None) -> bool:
    """Record a run's business outcome: did it do the job? For example
    ``bindle.outcome("ticket_resolved")`` when a support ticket closes, or
    ``bindle.outcome("refund_issued", success=False)`` when it didn't.

    Bindle divides each agent version's spend by its successful outcomes
    (cost per resolved ticket) and counts the spend on runs that failed or
    whose outcome failed as wasted. Agents that never send an outcome fall
    back to "no step failed and no negative feedback". ``value`` is an
    optional number (an order total, minutes saved). Inside a run the
    outcome joins it; from elsewhere pass the run's ``trace_id``. Returns
    True if recorded. Never raises.
    """
    if not isinstance(success, bool):
        return False
    return score(name, value, passed=success, comment=comment, trace_id=trace_id, _source="outcome")


_run_keys = _correlate.RunKeys()


def correlate(key: str, value: Union[str, int]) -> bool:
    """Tag the current run with a business identifier, so an event reported
    later without the run's trace ID still finds it. For example
    ``bindle.correlate("ticket_id", ticket.id)``; when the ticket is reopened,
    your helpdesk reports ``{"correlation": {"ticket_id": "T-123"},
    "name": "ticket_reopened", "success": false}`` to Bindle.

    ``key``: lowercase letters, digits and ``_``, starting with a letter, at
    most 32 characters. ``value``: at most 256 bytes. A run keeps at most 8
    keys. Only a digest of the key and value is sent; Bindle never stores
    the value. Returns True if recorded. Never raises.
    """
    try:
        attr = _correlate.attribute(key, value)
        if attr is None:
            log.debug("bindle: correlate(%r) ignored: invalid key or value", key)
            return False
        span = trace.get_current_span()
        ctx = span.get_span_context()
        if not ctx.is_valid or not span.is_recording():
            return False
        if not _run_keys.admit(ctx.trace_id, key):
            log.debug("bindle: correlate(%r) ignored: a run keeps at most %d keys", key, _correlate.MAX_KEYS)
            return False
        span.set_attribute(attr[0], attr[1])
        return True
    except Exception:  # noqa: BLE001 - never break the host app
        return False


def current_trace_id() -> Optional[str]:
    """The current run's trace ID (32 hex characters), to hand to your UI so
    user feedback can be attached to the run later. None outside a run."""
    try:
        ctx = trace.get_current_span().get_span_context()
        return format(ctx.trace_id, "032x") if ctx.is_valid else None
    except Exception:  # noqa: BLE001
        return None


def context_token() -> Optional[str]:
    """A short string that identifies where the current run is (its trace
    context, agent and session). Store it with work that continues later or
    elsewhere (a queue message, a job row, a human-approval request) and
    pass it to ``bindle.resume()`` there. None outside a run."""
    try:
        from ._links import make_token

        cfg = _state["config"]
        return make_token(trace.get_current_span().get_span_context(),
                          agent=getattr(cfg, "agent", None), session=_current_session())
    except Exception:  # noqa: BLE001
        return None


@contextmanager
def resume(token: Optional[str], name: str = "resume", *, kind: str = "resumed",
           same_trace: bool = False) -> Iterator[trace.Span]:
    """Start a run that continues the one ``token`` came from
    (``bindle.context_token()``), in any process and any time later:

        with bindle.resume(job["bindle_token"], name="process refund"):
            ...

    The new run links to the original, so Bindle shows "continued from" and
    "continued in" on the two runs, and it keeps the original's session.
    ``kind`` is "resumed" (default), "handoff" (another agent takes over) or
    "called" (the run serves another run's request). With ``same_trace``
    the work joins the original run instead of starting a new one (for
    short hops; long waits read better as separate runs). An invalid or
    missing token starts an unlinked run; it never raises.
    """
    from ._links import resume_span

    tracer = trace.get_tracer(_TRACER_NAME, __version__)
    with resume_span(tracer, token, name, kind, same_trace, _session_var, _current_session) as span:
        yield span


def handoff(to_agent: str, reason: Optional[str] = None, from_agent: Optional[str] = None) -> None:
    """Record a handoff to another agent or a human."""
    with step(f"handoff -> {to_agent}", type="handoff") as span:
        span.set_attribute("bindle.handoff.to", to_agent)
        if from_agent:
            span.set_attribute("bindle.handoff.from", from_agent)
        if reason:
            span.set_attribute("bindle.handoff.reason", reason)
