import json
from pathlib import Path

import pytest

import bindle
from bindle import _links

VECTORS = json.loads((Path(__file__).parent / "link_token_vectors.json").read_text())


@pytest.mark.parametrize("case", VECTORS["valid"])
def test_parse_valid_tokens(case):
    p = _links.parse_token(case["token"])
    assert p is not None
    assert format(p.span_context.trace_id, "032x") == case["trace_id"]
    assert format(p.span_context.span_id, "016x") == case["span_id"]
    assert p.span_context.trace_flags.sampled == case["sampled"]
    assert p.agent == case["agent"] and p.session == case["session"]
    assert p.span_context.trace_state.to_header() == case["tracestate"]
    # Round trip: the same context makes an equivalent token.
    again = _links.parse_token(_links.make_token(p.span_context, p.agent, p.session))
    assert again == p


@pytest.mark.parametrize("token", VECTORS["invalid"] + [None, 42])
def test_parse_invalid_tokens(token):
    assert _links.parse_token(token) is None


@pytest.fixture
def mem(monkeypatch):
    pytest.importorskip("opentelemetry.sdk")
    from opentelemetry import trace
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    monkeypatch.setattr(trace, "_TRACER_PROVIDER", trace._TRACER_PROVIDER)
    monkeypatch.setattr(trace._TRACER_PROVIDER_SET_ONCE, "_done", trace._TRACER_PROVIDER_SET_ONCE._done)
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    provider.add_span_processor(bindle._SessionTagger())
    trace.set_tracer_provider(provider)
    return exporter


def test_context_token_outside_a_run_is_none():
    assert bindle.context_token() is None


def test_resume_links_a_new_run(mem):
    bindle.set_session("conv-7")
    try:
        with bindle.step("ask for approval"):
            token = bindle.context_token()
    finally:
        bindle.set_session(None)
    assert token and token.startswith("b1.00-")

    with bindle.resume(token, name="after approval") as span:
        with bindle.step("refund", type="tool_call"):
            pass
        assert span.is_recording()

    by_name = {s.name: s for s in mem.get_finished_spans()}
    original, resumed, child = by_name["ask for approval"], by_name["after approval"], by_name["refund"]
    assert resumed.context.trace_id != original.context.trace_id  # a new run
    assert resumed.parent is None
    (link,) = resumed.links
    assert link.context.trace_id == original.context.trace_id and link.context.span_id == original.context.span_id
    assert resumed.attributes["bindle.link.kind"] == "resumed"
    assert resumed.attributes["bindle.link.trace_id"] == format(original.context.trace_id, "032x")
    # The session travels with the token and tags the resumed run's spans.
    assert resumed.attributes["bindle.session.id"] == "conv-7"
    assert child.attributes["bindle.session.id"] == "conv-7"
    assert child.context.trace_id == resumed.context.trace_id
    assert bindle._current_session() is None


def test_resume_same_trace_and_kinds(mem):
    with bindle.step("caller"):
        token = bindle.context_token()
    with bindle.resume(token, name="worker", kind="handoff", same_trace=True):
        pass
    with bindle.resume(token, name="odd", kind="bogus"):
        pass
    by_name = {s.name: s for s in mem.get_finished_spans()}
    worker = by_name["worker"]
    assert worker.context.trace_id == by_name["caller"].context.trace_id
    assert worker.parent.span_id == by_name["caller"].context.span_id
    assert worker.attributes["bindle.link.kind"] == "handoff"
    assert by_name["odd"].attributes["bindle.link.kind"] == "resumed"


def test_resume_with_bad_token_never_raises(mem):
    with bindle.resume("not-a-token", name="unlinked"):
        pass
    with bindle.resume(None, name="none"):
        pass
    spans = {s.name: s for s in mem.get_finished_spans()}
    assert spans["unlinked"].links == () or list(spans["unlinked"].links) == []
    assert "bindle.link.trace_id" not in spans["none"].attributes


def test_resume_records_errors(mem):
    with pytest.raises(ValueError):
        with bindle.resume(None, name="fails"):
            raise ValueError("boom")
    (span,) = mem.get_finished_spans()
    assert span.status.status_code.name == "ERROR"


def test_mcp_meta_round_trip(mem):
    assert bindle.inject_mcp_meta({"name": "lookup"}) == {"name": "lookup"}  # outside a run
    with bindle.step("client call") as client:
        params = bindle.inject_mcp_meta({"name": "lookup", "_meta": {"progressToken": 3}})
    assert params["_meta"]["progressToken"] == 3
    assert params["_meta"]["traceparent"].split("-")[1] == format(client.get_span_context().trace_id, "032x")

    # Server side: spans continue the caller's trace.
    with bindle.mcp_server_context(params):
        with bindle.step("server tool"):
            pass
    with bindle.mcp_server_context({"_meta": {"traceparent": "garbage"}}):
        with bindle.step("no context"):
            pass
    spans = {s.name: s for s in mem.get_finished_spans()}
    server = spans["server tool"]
    assert server.context.trace_id == spans["client call"].context.trace_id
    assert server.parent.span_id == spans["client call"].context.span_id
    assert spans["no context"].parent is None
