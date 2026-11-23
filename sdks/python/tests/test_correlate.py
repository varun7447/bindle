import json
from pathlib import Path

from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

import bindle
from bindle import _correlate

VECTORS = json.loads((Path(__file__).parent / "correlation_vectors.json").read_text(encoding="utf-8"))


def test_shared_vectors():
    assert VECTORS["max_keys"] == _correlate.MAX_KEYS
    assert VECTORS["max_value_bytes"] == _correlate.MAX_VALUE_BYTES
    for d in VECTORS["digests"]:
        assert _correlate.digest(d["key"], d["value"]) == d["digest"]
    for k in VECTORS["valid_keys"]:
        assert _correlate.valid_key(k)
    for k in VECTORS["invalid_keys"]:
        assert not _correlate.valid_key(k)
    for v in VECTORS["invalid_values"]:
        assert not _correlate.valid_value(v)


def _tracer():
    mem = InMemorySpanExporter()
    tp = TracerProvider()
    tp.add_span_processor(SimpleSpanProcessor(mem))
    return tp.get_tracer("t"), mem


def test_correlate_sends_only_a_digest():
    tracer, mem = _tracer()
    with tracer.start_as_current_span("run"):
        assert bindle.correlate("ticket_id", "T-123") is True
        assert bindle.correlate("order", 981) is True  # numbers as text
    attrs = dict(mem.get_finished_spans()[0].attributes)
    assert attrs["bindle.correlation.ticket_id"] == _correlate.digest("ticket_id", "T-123")
    assert attrs["bindle.correlation.order"] == _correlate.digest("order", "981")
    assert "T-123" not in json.dumps(attrs)


def test_correlate_never_raises_and_rejects_bad_input():
    assert bindle.correlate("ticket_id", "T-1") is False  # outside a run
    tracer, mem = _tracer()
    with tracer.start_as_current_span("run"):
        assert bindle.correlate("Ticket", "x") is False
        assert bindle.correlate("ticket_id", "") is False
        assert bindle.correlate("ticket_id", "x" * 257) is False
        assert bindle.correlate("ticket_id", None) is False  # type: ignore[arg-type]
        assert bindle.correlate("flag", True) is False  # type: ignore[arg-type]
        assert bindle.correlate(None, "x") is False  # type: ignore[arg-type]
    assert not any(k.startswith("bindle.correlation.") for k in mem.get_finished_spans()[0].attributes)


def test_a_run_keeps_at_most_eight_keys():
    tracer, mem = _tracer()
    with tracer.start_as_current_span("run"):
        results = [bindle.correlate(f"k{i}", "v") for i in range(10)]
        assert bindle.correlate("k0", "again") is True  # an existing key can be set again
    assert results == [True] * 8 + [False] * 2
    keys = [k for k in mem.get_finished_spans()[0].attributes if k.startswith("bindle.correlation.")]
    assert len(keys) == 8
