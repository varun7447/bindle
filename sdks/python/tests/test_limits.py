import json

import pytest
from opentelemetry.sdk.trace import TracerProvider

import bindle
from bindle import _remote
from bindle._limits import Guard, Limit, LimitExceeded, RunCounter, limit_for, parse_limits


def make(agent="refunds"):
    counter = RunCounter()
    provider = TracerProvider()
    provider.add_span_processor(counter)
    return provider.get_tracer("t"), Guard(agent, counter)


def test_parse_limits_is_tolerant():
    got = parse_limits([
        {"agent": "*", "max_steps_per_run": 10},
        {"agent": "refunds", "paused": True, "daily_budget_usd": 5, "pause_over_budget": True},
        {"agent": ""}, {"max_steps_per_run": 3}, "junk", {"agent": "x", "max_steps_per_run": "nope"},
        {"agent": "neg", "max_tokens_per_run": -5},
    ])
    assert [l.agent for l in got] == ["*", "refunds", "neg"]
    assert got[1].paused and got[1].pause_over_budget and got[1].daily_budget_usd == 5
    assert got[2].max_tokens_per_run == 0
    assert parse_limits(None) == ()


def test_limit_for_prefers_own_row():
    ls = (Limit("*", max_steps_per_run=50), Limit("refunds", max_steps_per_run=10))
    assert limit_for(ls, "refunds").max_steps_per_run == 10
    assert limit_for(ls, "other").max_steps_per_run == 50
    assert limit_for(ls[1:], "other") is None


def test_no_limits_never_raises():
    tracer, guard = make()
    with tracer.start_as_current_span("run"):
        for _ in range(100):
            with tracer.start_as_current_span("step"):
                pass
        guard.check()


def test_paused_and_over_budget():
    _, guard = make()
    guard.update((Limit("refunds", paused=True),), ())
    with pytest.raises(LimitExceeded) as e:
        guard.check()
    assert e.value.reason == "paused"

    guard.update((Limit("refunds", daily_budget_usd=5),), ("refunds",))
    guard.check()  # over budget, but this limit only notifies

    guard.update((Limit("*", daily_budget_usd=5, pause_over_budget=True),), ("refunds",))
    with pytest.raises(LimitExceeded) as e:
        guard.check()
    assert e.value.reason == "over_budget"

    guard.update((Limit("*", daily_budget_usd=5, pause_over_budget=True),), ("someone-else",))
    guard.check()


def test_steps_per_run_counts_spans_in_the_current_trace():
    tracer, guard = make()
    guard.update((Limit("*", max_steps_per_run=5),), ())
    with tracer.start_as_current_span("run"):
        for _ in range(4):
            with tracer.start_as_current_span("tool"):
                pass
        guard.check()  # 5 steps: at the limit
        with tracer.start_as_current_span("tool"):
            with pytest.raises(LimitExceeded) as e:
                guard.check()
    assert e.value.reason == "max_steps_per_run" and e.value.value == 6 and e.value.limit == 5
    with tracer.start_as_current_span("next run"):
        guard.check()  # a new trace starts from zero


def test_tokens_per_run_counts_llm_spans_only():
    tracer, guard = make()
    guard.update((Limit("*", max_tokens_per_run=100),), ())
    with tracer.start_as_current_span("agent", attributes={"gen_ai.operation.name": "invoke_agent",
                                                           "gen_ai.usage.input_tokens": 500}):
        with tracer.start_as_current_span("chat", attributes={"gen_ai.operation.name": "chat",
                                                              "gen_ai.usage.input_tokens": 60, "gen_ai.usage.output_tokens": 30}):
            pass
        guard.check()  # 90 tokens; the agent span's repeated total isn't counted
        with tracer.start_as_current_span("llm", attributes={"openinference.span.kind": "LLM", "llm.token_count.prompt": 20}):
            pass
        with pytest.raises(LimitExceeded) as e:
            guard.check()
    assert e.value.reason == "max_tokens_per_run" and e.value.value == 110


def test_counter_is_bounded():
    counter = RunCounter(max_traces=3)
    provider = TracerProvider()
    provider.add_span_processor(counter)
    tracer = provider.get_tracer("t")
    ids = []
    for _ in range(5):
        with tracer.start_as_current_span("run") as s:
            ids.append(s.get_span_context().trace_id)
    assert len(counter._runs) == 3
    assert counter.usage(ids[0]) == (0, 0) and counter.usage(ids[-1]) == (1, 0)


def test_remote_parse_carries_limits():
    body = json.dumps({"version": 4, "sample_rate": 1, "capture_content": True,
                       "limits": [{"agent": "refunds", "paused": True}], "over_budget": ["refunds", 7]}).encode()
    s = _remote.parse(body)
    assert s.limits[0].paused and s.over_budget == ("refunds",)
    old = _remote.parse(json.dumps({"version": 1, "sample_rate": 1, "capture_content": True}).encode())
    assert old.limits == () and old.over_budget == ()


def test_public_api_without_init_is_a_no_op():
    bindle.shutdown()
    bindle.check_limits()
    assert bindle.limits_status() == {}


def test_score_in_run_and_by_trace_id():
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    mem = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(mem))
    bindle.shutdown()
    assert bindle.score("helpful", 1.0) is False  # not initialized
    bindle._state["provider"] = provider
    try:
        tracer = provider.get_tracer("t")
        assert bindle.score("helpful", 0.9) is False  # outside a run, no trace_id
        with tracer.start_as_current_span("run") as run:
            assert bindle.score("helpful", 0.9, passed=True, comment="clear answer")
            run_tid = run.get_span_context().trace_id
        assert bindle.score("judge", passed=False, trace_id=format(run_tid, "032x"))
        assert bindle.score("bad", float("nan"), trace_id=run_tid) is False
        assert bindle.score("", 1, trace_id=run_tid) is False
        assert bindle.score("nothing", trace_id=run_tid) is False
        spans = {s.name: s for s in mem.get_finished_spans()}
        inrun, ext = spans["score helpful"], spans["score judge"]
        assert inrun.context.trace_id == run_tid and inrun.attributes["bindle.score.value"] == 0.9
        assert inrun.attributes["bindle.step.type"] == "eval" and inrun.attributes["bindle.score.passed"] is True
        assert "bindle.score.external" not in inrun.attributes
        assert ext.context.trace_id == run_tid and ext.attributes["bindle.score.external"] is True
        assert ext.attributes["bindle.score.passed"] is False
    finally:
        bindle._state["provider"] = None


def test_feedback_and_current_trace_id():
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    mem = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(mem))
    bindle._state["provider"] = provider
    try:
        assert bindle.current_trace_id() is None
        with provider.get_tracer("t").start_as_current_span("run"):
            tid = bindle.current_trace_id()
        assert tid and len(tid) == 32
        assert bindle.feedback(trace_id=tid, thumbs_up=False, comment="wrong order")
        assert bindle.feedback(trace_id=tid, rating=4)
        assert bindle.feedback(trace_id=tid) is False
        fb = [s for s in mem.get_finished_spans() if s.name == "score feedback"]
        assert len(fb) == 2
        assert fb[0].attributes["bindle.score.source"] == "user" and fb[0].attributes["bindle.score.passed"] is False
        assert fb[0].attributes["bindle.score.value"] == 0.0 and fb[1].attributes["bindle.score.value"] == 4.0
        assert format(fb[0].context.trace_id, "032x") == tid
    finally:
        bindle._state["provider"] = None


def test_session_tags_every_span():
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    mem = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(bindle._SessionTagger())
    provider.add_span_processor(SimpleSpanProcessor(mem))
    tracer = provider.get_tracer("framework")
    bindle.set_session("conv-1")
    try:
        with tracer.start_as_current_span("framework span"):
            pass
    finally:
        bindle.set_session(None)
    with tracer.start_as_current_span("after"):
        pass
    spans = {s.name: s for s in mem.get_finished_spans()}
    assert spans["framework span"].attributes["bindle.session.id"] == "conv-1"
    assert "bindle.session.id" not in spans["after"].attributes


def test_processors_are_real_span_processors():
    """Newer OpenTelemetry SDKs call hooks such as _on_ending on every
    processor; a plain object would raise inside the host app."""
    from opentelemetry.sdk.trace import SpanProcessor

    from bindle._eval import _Collector

    for p in (bindle._SessionTagger(), _Collector(), RunCounter()):
        assert isinstance(p, SpanProcessor)
