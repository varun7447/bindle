"""Exporter wrapper that redacts spans before they leave the process.

OpenTelemetry spans are read-only once ended, so redaction happens by copying
each span with cleaned attributes and handing the copy to the real exporter.
"""

from __future__ import annotations

from typing import Sequence

from opentelemetry.sdk.trace import Event, ReadableSpan
from opentelemetry.sdk.trace.export import SpanExporter, SpanExportResult

from ._redact import Redactor


class RedactingSpanExporter(SpanExporter):
    # When content is dropped, spans say who turned capture off, so the
    # console can explain an empty input/output: "sdk" (this app's code or
    # environment) or "workspace" (the console's SDK settings).
    DROPPED_ATTR = "bindle.content_dropped"

    def __init__(self, delegate: SpanExporter, redactor: Redactor, capture_content: bool = True,
                 dropped_by: str = "sdk"):
        self._delegate = delegate
        # One tuple, replaced atomically by update(), read once per export.
        self._policy = (capture_content, redactor, dropped_by)

    def update(self, capture_content: bool, redactor: Redactor, dropped_by: str = "sdk") -> None:
        """Switch settings at runtime (remote config)."""
        self._policy = (capture_content, redactor, dropped_by)

    @property
    def capture_content(self) -> bool:
        return self._policy[0]

    _CONTENT_KEYS = ("input.value", "output.value", "gen_ai.prompt", "gen_ai.completion",
                     "bindle.input", "bindle.output", "bindle.reasoning",
                     # OpenTelemetry GenAI conventions (Microsoft Agent Framework, Strands, ADK)
                     "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions",
                     "gen_ai.tool.call.arguments", "gen_ai.tool.call.result")

    def _clean(self, span: ReadableSpan, capture_content: bool, redactor: Redactor, dropped_by: str = "sdk") -> ReadableSpan:
        attrs = dict(span.attributes or {})
        if not capture_content:
            for k in self._CONTENT_KEYS:
                attrs.pop(k, None)
            attrs[self.DROPPED_ATTR] = dropped_by
        attrs = redactor.apply_attributes(attrs)
        events = [
            Event(e.name, redactor.apply_attributes(dict(e.attributes or {})), e.timestamp)
            for e in span.events
            # GenAI events carry message content (gen_ai.user.message, gen_ai.choice, ...).
            if capture_content or not e.name.startswith("gen_ai.")
        ]
        return ReadableSpan(
            name=span.name,
            context=span.context,
            parent=span.parent,
            resource=span.resource,
            attributes=attrs,
            events=events,
            links=span.links,
            kind=span.kind,
            status=span.status,
            start_time=span.start_time,
            end_time=span.end_time,
            instrumentation_scope=span.instrumentation_scope,
        )

    def export(self, spans: Sequence[ReadableSpan]) -> SpanExportResult:
        capture_content, redactor, dropped_by = self._policy
        try:
            cleaned = [self._clean(s, capture_content, redactor, dropped_by) for s in spans]
        except Exception:  # noqa: BLE001 - never send unredacted data on error
            return SpanExportResult.FAILURE
        return self._delegate.export(cleaned)

    def shutdown(self) -> None:
        self._delegate.shutdown()

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        flush = getattr(self._delegate, "force_flush", None)
        return flush(timeout_millis) if flush else True
