"""Connecting runs across processes, queues, people and MCP servers.

``context_token()`` captures where a run is (W3C trace context plus the
agent and session) as one short string. Put it in a queue message, a job
row or a human-approval request; later, anywhere, ``resume(token)`` starts a
new run that links back to it, so Bindle shows "continued from" on the new
run and "continued in" on the old one.

For MCP, ``inject_mcp_meta()`` adds the trace context to a request's
``params._meta`` (the ``traceparent``/``tracestate`` keys of the
OpenTelemetry MCP conventions) and ``mcp_server_context()`` continues it in
the server, so the server's spans join the caller's trace.

Token format (shared with the TypeScript SDK):
``b1.<traceparent>[.<base64url JSON {"ts": tracestate, "a": agent, "s": session}>]``.
"""

from __future__ import annotations

import base64
import json
import logging
import re
from contextlib import contextmanager
from typing import Any, Iterator, Mapping, NamedTuple, Optional

from opentelemetry import context as otel_context
from opentelemetry import trace
from opentelemetry.trace import Link, NonRecordingSpan, SpanContext, Status, StatusCode, TraceFlags, TraceState

log = logging.getLogger("bindle")

KINDS = ("resumed", "handoff", "called")
MAX_TOKEN = 2048
_MAX_FIELD = 256
_TRACEPARENT = re.compile(r"^00-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$")


class Parsed(NamedTuple):
    span_context: SpanContext
    agent: Optional[str]
    session: Optional[str]


def _b64(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).decode("ascii").rstrip("=")


def _unb64(text: str) -> bytes:
    return base64.urlsafe_b64decode(text + "=" * (-len(text) % 4))


def make_token(span_context: SpanContext, agent: Optional[str] = None, session: Optional[str] = None) -> Optional[str]:
    if not span_context.is_valid:
        return None
    tp = "00-%032x-%016x-%02x" % (span_context.trace_id, span_context.span_id, int(span_context.trace_flags))
    extra: dict = {}
    ts = span_context.trace_state.to_header() if span_context.trace_state else ""
    if ts:
        extra["ts"] = ts[:512]
    if agent:
        extra["a"] = str(agent)[:_MAX_FIELD]
    if session:
        extra["s"] = str(session)[:_MAX_FIELD]
    token = "b1." + tp
    if extra:
        token += "." + _b64(json.dumps(extra, separators=(",", ":")).encode("utf-8"))
    return token


def parse_token(token: Any) -> Optional[Parsed]:
    """The token's trace context, agent and session; None if malformed."""
    if not isinstance(token, str) or len(token) > MAX_TOKEN:
        return None
    parts = token.strip().split(".")
    if len(parts) not in (2, 3) or parts[0] != "b1":
        return None
    m = _TRACEPARENT.match(parts[1])
    if not m:
        return None
    trace_id, span_id, flags = int(m.group(1), 16), int(m.group(2), 16), int(m.group(3), 16)
    if trace_id == 0 or span_id == 0:
        return None
    extra: dict = {}
    if len(parts) == 3:
        try:
            extra = json.loads(_unb64(parts[2]).decode("utf-8"))
        except Exception:  # noqa: BLE001
            return None
        if not isinstance(extra, dict):
            return None
    state = TraceState()
    ts = extra.get("ts")
    if isinstance(ts, str) and ts:
        try:
            state = TraceState.from_header([ts])
        except Exception:  # noqa: BLE001
            state = TraceState()

    def field(key: str) -> Optional[str]:
        v = extra.get(key)
        return v[:_MAX_FIELD] if isinstance(v, str) and v else None

    sc = SpanContext(trace_id=trace_id, span_id=span_id, is_remote=True,
                     trace_flags=TraceFlags(flags & 0xFF), trace_state=state)
    return Parsed(sc, field("a"), field("s"))


@contextmanager
def resume_span(tracer, token: Any, name: str, kind: str, same_trace: bool,
                session_var, current_session) -> Iterator[trace.Span]:
    parsed = parse_token(token)
    if token is not None and parsed is None:
        log.debug("bindle: resume() got an invalid context token; starting an unlinked run")
    kind = kind if kind in KINDS else "resumed"
    attrs: dict = {"bindle.step.type": "agent", "bindle.link.kind": kind}
    links = []
    ctx = otel_context.Context()  # empty: the new span is the root of a new run
    if parsed is not None:
        sc = parsed.span_context
        attrs["bindle.link.trace_id"] = format(sc.trace_id, "032x")
        if parsed.agent:
            attrs["bindle.link.from_agent"] = parsed.agent
        links.append(Link(sc, {"bindle.link.kind": kind}))
        if same_trace:
            ctx = trace.set_span_in_context(NonRecordingSpan(sc), ctx)
    reset = None
    if parsed is not None and parsed.session and not current_session():
        reset = session_var.set(parsed.session)
    try:
        with tracer.start_as_current_span(name, context=ctx, links=links, attributes=attrs,
                                          record_exception=False, set_status_on_exception=False) as span:
            if reset is not None:
                span.set_attribute("bindle.session.id", parsed.session)
            try:
                yield span
            except Exception as exc:
                span.record_exception(exc)
                span.set_status(Status(StatusCode.ERROR, str(exc)))
                raise
    finally:
        if reset is not None:
            session_var.reset(reset)


def inject_mcp_meta(params: Optional[Mapping[str, Any]] = None) -> dict:
    """A copy of an MCP request's ``params`` with the current trace context
    in ``_meta`` (``traceparent``, and ``tracestate`` when set). Existing
    ``_meta`` keys are kept. Outside a run, params are returned unchanged."""
    out = dict(params or {})
    try:
        sc = trace.get_current_span().get_span_context()
        if not sc.is_valid:
            return out
        meta = dict(out.get("_meta") or {})
        meta["traceparent"] = "00-%032x-%016x-%02x" % (sc.trace_id, sc.span_id, int(sc.trace_flags))
        ts = sc.trace_state.to_header() if sc.trace_state else ""
        if ts:
            meta["tracestate"] = ts
        out["_meta"] = meta
    except Exception as exc:  # noqa: BLE001 - never break the host app
        log.debug("bindle: inject_mcp_meta failed: %s", exc)
    return out


def extract_mcp_context(params: Any) -> Optional[SpanContext]:
    meta = params.get("_meta") if isinstance(params, Mapping) else None
    if not isinstance(meta, Mapping):
        return None
    tp = meta.get("traceparent")
    m = _TRACEPARENT.match(tp) if isinstance(tp, str) else None
    if not m or int(m.group(1), 16) == 0 or int(m.group(2), 16) == 0:
        return None
    state = TraceState()
    ts = meta.get("tracestate")
    if isinstance(ts, str) and ts:
        try:
            state = TraceState.from_header([ts[:512]])
        except Exception:  # noqa: BLE001
            pass
    return SpanContext(trace_id=int(m.group(1), 16), span_id=int(m.group(2), 16), is_remote=True,
                       trace_flags=TraceFlags(int(m.group(3), 16)), trace_state=state)


@contextmanager
def mcp_server_context(params: Any) -> Iterator[None]:
    """In an MCP server's request handler: spans started inside continue the
    caller's trace, if its request carried one in ``params._meta``."""
    sc = extract_mcp_context(params)
    if sc is None:
        yield
        return
    token = otel_context.attach(trace.set_span_in_context(NonRecordingSpan(sc)))
    try:
        yield
    finally:
        otel_context.detach(token)
