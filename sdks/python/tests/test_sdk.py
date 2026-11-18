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


def test_capture_off_drops_genai_content():
    """Framework-native content (Agent Framework attributes, Strands events)
    must not leave the process when content capture is off."""
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    from bindle._export import RedactingSpanExporter
    from bindle._redact import Redactor

    mem = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(RedactingSpanExporter(mem, Redactor(), capture_content=False)))
    with provider.get_tracer("t").start_as_current_span("chat") as span:
        span.set_attribute("gen_ai.input.messages", '[{"content":"secret plan"}]')
        span.set_attribute("gen_ai.tool.call.arguments", '{"q":"x"}')
        span.set_attribute("gen_ai.usage.input_tokens", 40)
        span.add_event("gen_ai.user.message", {"content": "secret plan"})
        span.add_event("retry", {"n": 1})
    (s,) = mem.get_finished_spans()
    assert dict(s.attributes) == {"gen_ai.usage.input_tokens": 40, "bindle.content_dropped": "sdk"}
    assert [e.name for e in s.events] == ["retry"]


def test_resource_overlay_names_spans_from_another_provider():
    """On AgentCore, ADOT owns the tracer provider; Bindle's exporter still
    labels spans with the agent name, version and environment."""
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    from bindle._export import RedactingSpanExporter
    from bindle._redact import Redactor

    mem = InMemorySpanExporter()
    overlay = {"bindle.agent.name": "refunds", "deployment.environment.name": "prod", "service.name": "bindle-default"}
    provider = TracerProvider(resource=Resource({"service.name": "adot-service"}))
    provider.add_span_processor(SimpleSpanProcessor(RedactingSpanExporter(mem, Redactor(), resource_overlay=overlay)))
    with provider.get_tracer("t").start_as_current_span("chat"):
        pass
    (s,) = mem.get_finished_spans()
    assert s.resource.attributes["bindle.agent.name"] == "refunds"
    assert s.resource.attributes["deployment.environment.name"] == "prod"
    assert s.resource.attributes["service.name"] == "adot-service"  # the app's own value wins
