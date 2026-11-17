"""A head sampler whose rate can change at runtime (remote config)."""

from __future__ import annotations

import threading
from typing import Optional, Sequence

from opentelemetry.context import Context
from opentelemetry.sdk.trace.sampling import Sampler, SamplingResult, TraceIdRatioBased
from opentelemetry.trace import Link, SpanKind
from opentelemetry.util.types import Attributes


class DynamicRatioSampler(Sampler):
    """TraceIdRatioBased with a swappable rate. Uses the same rule as
    Bindle's ingest, so both keep or drop the same traces."""

    def __init__(self, rate: float):
        self._lock = threading.Lock()
        self._inner = TraceIdRatioBased(rate)

    @property
    def rate(self) -> float:
        return self._inner.rate

    def set_rate(self, rate: float) -> None:
        rate = min(max(rate, 0.0), 1.0)
        if rate != self._inner.rate:
            with self._lock:
                self._inner = TraceIdRatioBased(rate)

    def should_sample(self, parent_context: Optional[Context], trace_id: int, name: str,
                      kind: Optional[SpanKind] = None, attributes: Attributes = None,
                      links: Optional[Sequence[Link]] = None, trace_state=None) -> SamplingResult:
        return self._inner.should_sample(parent_context, trace_id, name, kind, attributes, links, trace_state)

    def get_description(self) -> str:
        return f"BindleDynamicRatio{{{self._inner.rate}}}"
