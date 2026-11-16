import asyncio

import pytest

import bindle
from bindle import _config, _instrument


def test_init_without_key_is_noop(monkeypatch):
    monkeypatch.delenv("BINDLE_API_KEY", raising=False)
    assert bindle.init() is False


def test_config_env_and_clamping(monkeypatch):
    monkeypatch.setenv("BINDLE_API_KEY", "bd_test_x")
    monkeypatch.setenv("BINDLE_SAMPLE_RATE", "7")
    monkeypatch.setenv("BINDLE_REDACT", "false")
    cfg = _config.resolve(agent="triage")
    assert cfg.api_key == "bd_test_x"
    assert cfg.sample_rate == 1.0
    assert cfg.redact is False
    assert cfg.traces_url == "https://ingest.bindle.ai/v1/traces"


def test_decorators_work_without_init():
    @bindle.trace_tool
    def lookup(x):
        return x * 2

    @bindle.trace_step(type="decision")
    async def plan(x):
        return x + 1

    assert lookup(2) == 4
    assert asyncio.run(plan(1)) == 2
    bindle.decision("route", ["a", "b"], "a", rationale="a is cheaper")
    bindle.handoff("human-escalation", reason="low eval score")


def test_exceptions_propagate():
    @bindle.trace_tool
    def boom():
        raise ValueError("nope")

    with pytest.raises(ValueError):
        boom()


def test_unknown_framework_is_skipped():
    assert _instrument.instrument(None, ["not-a-framework"]) == []


def test_sdk_end_to_end_with_otel_sdk():
    """Full pipeline check; runs only where opentelemetry-sdk is installed."""
    pytest.importorskip("opentelemetry.sdk")
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    from bindle._export import RedactingSpanExporter
    from bindle._redact import Redactor

    mem = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(RedactingSpanExporter(mem, Redactor())))
    tracer = provider.get_tracer("t")
    with tracer.start_as_current_span("s") as span:
        span.set_attribute("input.value", "email a@b.com")
    (out,) = mem.get_finished_spans()
    assert out.attributes["input.value"] == "email [REDACTED:EMAIL]"
