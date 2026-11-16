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
    def __init__(self, delegate: SpanExporter, redactor: Redactor, capture_content: bool = True):
        self._delegate = delegate
        self._redactor = redactor
        self._capture_content = capture_content

    _CONTENT_KEYS = ("input.value", "output.value", "gen_ai.prompt", "gen_ai.completion",
                     "bindle.input", "bindle.output", "bindle.reasoning")

    def _clean(self, span: ReadableSpan) -> ReadableSpan:
        attrs = dict(span.attributes or {})
        if not self._capture_content:
            for k in self._CONTENT_KEYS:
                attrs.pop(k, None)
        attrs = self._redactor.apply_attributes(attrs)
        events = [
            Event(e.name, self._redactor.apply_attributes(dict(e.attributes or {})), e.timestamp)
            for e in span.events
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
        try:
            cleaned = [self._clean(s) for s in spans]
        except Exception:  # noqa: BLE001 - never send unredacted data on error
            return SpanExportResult.FAILURE
        return self._delegate.export(cleaned)

    def shutdown(self) -> None:
        self._delegate.shutdown()

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        flush = getattr(self._delegate, "force_flush", None)
        return flush(timeout_millis) if flush else True
