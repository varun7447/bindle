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
import functools
import inspect
import json
import logging
import threading
from contextlib import contextmanager
from typing import Any, Callable, Iterator, List, Optional, Sequence, Union

from opentelemetry import trace
from opentelemetry.trace import Status, StatusCode

from . import _config, _instrument
from ._redact import Redactor

__all__ = [
    "init", "flush", "shutdown", "step", "trace_step", "trace_tool",
    "decision", "handoff", "set_session", "Redactor",
]
__version__ = "0.1.0"

log = logging.getLogger("bindle")
_TRACER_NAME = "bindle"
_MAX_CAPTURE = 16 * 1024

_lock = threading.Lock()
_state: dict = {
    "provider": None, "config": None, "session": threading.local(),
    "sampler": None, "exporter": None, "poller": None, "remote": None,
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
        exporter.update(capture, _local_redactor(cfg, settings.redact_patterns))
    _state["capture"], _state["remote"] = capture, settings
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
    exporter = RedactingSpanExporter(exporter, _local_redactor(cfg), cfg.capture_content)
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
    _state.update(sampler=None, exporter=None, remote=None, capture=True)


def set_session(session_id: Optional[str]) -> None:
    """Tag subsequent steps on this thread with a session/conversation ID."""
    _state["session"].value = session_id


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
        session = getattr(_state["session"], "value", None)
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


def handoff(to_agent: str, reason: Optional[str] = None, from_agent: Optional[str] = None) -> None:
    """Record a handoff to another agent or a human."""
    with step(f"handoff -> {to_agent}", type="handoff") as span:
        span.set_attribute("bindle.handoff.to", to_agent)
        if from_agent:
            span.set_attribute("bindle.handoff.from", from_agent)
        if reason:
            span.set_attribute("bindle.handoff.reason", reason)
