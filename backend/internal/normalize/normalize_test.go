package normalize

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bindle-ai/bindle/backend/internal/cost"
	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/redact"
)

func newN() *Normalizer {
	return &Normalizer{
		Prices:   cost.New(map[string]cost.Price{"test-model": {InputPerMTok: 1, OutputPerMTok: 2}}),
		Redactor: redact.Default(),
	}
}

func TestClassifyConventions(t *testing.T) {
	cases := []struct {
		name  string
		attrs map[string]any
		want  string
	}{
		{"bindle explicit", map[string]any{"bindle.step.type": "decision"}, model.StepDecision},
		{"openinference llm", map[string]any{"openinference.span.kind": "LLM"}, model.StepLLM},
		{"openinference tool", map[string]any{"openinference.span.kind": "TOOL"}, model.StepTool},
		{"openinference retriever", map[string]any{"openinference.span.kind": "RETRIEVER"}, model.StepRetrieval},
		{"genai chat", map[string]any{"gen_ai.operation.name": "chat"}, model.StepLLM},
		{"genai tool", map[string]any{"gen_ai.operation.name": "execute_tool"}, model.StepTool},
		{"genai agent", map[string]any{"gen_ai.operation.name": "invoke_agent"}, model.StepAgent},
		{"handoff", map[string]any{"bindle.handoff.to": "human"}, model.StepHandoff},
		{"model heuristic", map[string]any{"gen_ai.request.model": "x"}, model.StepLLM},
		{"unknown", map[string]any{"foo": "bar"}, model.StepOther},
	}
	n := newN()
	for _, c := range cases {
		st := n.Normalize("t1", model.Span{Attributes: c.attrs})
		if st.Type != c.want {
			t.Errorf("%s: got %s want %s", c.name, st.Type, c.want)
		}
	}
}

func TestRootChainIsTheAgentRun(t *testing.T) {
	n := newN()
	chain := map[string]any{"openinference.span.kind": "CHAIN"}
	if st := n.Normalize("t1", model.Span{SpanID: "a", Attributes: chain}); st.Type != model.StepAgent {
		t.Errorf("root chain: %s", st.Type)
	}
	if st := n.Normalize("t1", model.Span{SpanID: "b", ParentSpanID: "a", Attributes: chain}); st.Type != model.StepOther {
		t.Errorf("nested chain: %s", st.Type)
	}
}

// Span shapes recorded from the frameworks themselves (stub models).
func TestAgentFrameworkShapes(t *testing.T) {
	n := newN()
	// Microsoft Agent Framework: content in gen_ai.* message attributes.
	chat := n.Normalize("t", model.Span{SpanID: "c", ParentSpanID: "a", ScopeName: "agent_framework", Attributes: map[string]any{
		"gen_ai.operation.name":      "chat",
		"gen_ai.response.model":      "stub-model",
		"gen_ai.usage.input_tokens":  int64(40),
		"gen_ai.system_instructions": "Help with refunds.",
		"gen_ai.input.messages":      `[{"role":"user","parts":[{"type":"text","content":"I'm bob@example.com"}]}]`,
		"gen_ai.output.messages":     `[{"role":"assistant","parts":[{"type":"text","content":"On its way"}]}]`,
	}})
	if chat.Type != model.StepLLM || chat.Framework != "Microsoft Agent Framework" || chat.InputTokens != 40 {
		t.Fatalf("chat: %+v", chat)
	}
	if !strings.HasPrefix(chat.Input, "system: Help with refunds.") || !strings.Contains(chat.Input, "[REDACTED:EMAIL]") || !strings.Contains(chat.Output, "On its way") {
		t.Fatalf("content: %q / %q", chat.Input, chat.Output)
	}
	if _, ok := chat.Attributes["gen_ai.input.messages"]; ok {
		t.Fatal("content left in attributes")
	}
	tool := n.Normalize("t", model.Span{SpanID: "x", ParentSpanID: "a", ScopeName: "agent_framework", Attributes: map[string]any{
		"gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": "lookup_order",
		"gen_ai.tool.call.arguments": `{"email":"bob@example.com"}`, "gen_ai.tool.call.result": "refund issued",
	}})
	if tool.Type != model.StepTool || tool.Input != `{"email":"[REDACTED:EMAIL]"}` || tool.Output != "refund issued" {
		t.Fatalf("tool: %+v", tool)
	}
	// Agent spans repeat their children's usage: not counted twice.
	agent := n.Normalize("t", model.Span{SpanID: "a", ScopeName: "agent_framework", Attributes: map[string]any{
		"gen_ai.operation.name": "invoke_agent", "gen_ai.agent.name": "refund_agent", "gen_ai.usage.input_tokens": int64(80),
	}, ResourceAttrs: map[string]any{"bindle.agent.name": "maf-demo"}})
	if agent.Type != model.StepAgent || agent.InputTokens != 0 || agent.Attributes["gen_ai.usage.input_tokens"] != "80" || agent.AgentName != "maf-demo" {
		t.Fatalf("agent: %+v", agent)
	}

	// Strands: content in GenAI span events.
	sc := n.Normalize("t", model.Span{SpanID: "s", ParentSpanID: "a", ScopeName: "strands.telemetry.tracer", Attributes: map[string]any{"gen_ai.operation.name": "chat"},
		Events: []model.Event{
			{Name: "gen_ai.user.message", Attributes: map[string]any{"content": `[{"text":"refund for bob@example.com"}]`}},
			{Name: "gen_ai.choice", Attributes: map[string]any{"message": `[{"text":"On its way"}]`}},
		}})
	if !strings.HasPrefix(sc.Input, `[{"role":"user","content":[`) || sc.Framework != "Strands" || !strings.Contains(sc.Input, "[REDACTED:EMAIL]") || !strings.Contains(sc.Output, "On its way") {
		t.Fatalf("strands: %+v", sc)
	}

	// Google ADK: the run's root is invoke_workflow.
	wf := n.Normalize("t", model.Span{SpanID: "w", ScopeName: "gcp.vertex.agent", Attributes: map[string]any{"gen_ai.operation.name": "invoke_workflow"}})
	if wf.Type != model.StepAgent || wf.Framework != "Google ADK" {
		t.Fatalf("adk: %+v", wf)
	}

	// Foundry Agent Service (azure-ai-agents): content in gen_ai.event.content.
	fa := n.Normalize("t", model.Span{SpanID: "f", ParentSpanID: "a", ScopeName: "azure.core.tracing.ext.opentelemetry_span",
		Attributes: map[string]any{"gen_ai.system": "az.ai.agents", "gen_ai.operation.name": "process_thread_run"},
		Events:     []model.Event{{Name: "gen_ai.user.message", Attributes: map[string]any{"gen_ai.event.content": `{"content":"hi"}`}}}})
	if fa.Framework != "Microsoft Foundry Agent Service" || fa.Input != `[{"role":"user","content":{"content":"hi"}}]` {
		t.Fatalf("foundry: %+v", fa)
	}
}

func TestLLMFieldsAndCost(t *testing.T) {
	n := newN()
	st := n.Normalize("acme", model.Span{
		TraceID: "t", SpanID: "s", StartUnixNano: 1_000_000, EndUnixNano: 3_000_000,
		ScopeName: "openinference.instrumentation.openai_agents",
		Attributes: map[string]any{
			"gen_ai.operation.name":      "chat",
			"gen_ai.system":              "anthropic",
			"gen_ai.request.model":       "test-model-2026-01-15",
			"gen_ai.usage.input_tokens":  int64(1_000_000),
			"gen_ai.usage.output_tokens": int64(500_000),
			"input.value":                "email me at jane@example.com",
		},
	})
	if st.TenantID != "acme" || st.Framework != "OpenAI Agents SDK" {
		t.Fatalf("tenant/framework: %+v", st)
	}
	if st.DurationMs != 2 {
		t.Fatalf("duration %v", st.DurationMs)
	}
	if !st.CostKnown || st.CostUSD != 2.0 { // 1M*1/1M + 0.5M*2/1M
		t.Fatalf("cost %v known=%v", st.CostUSD, st.CostKnown)
	}
	if strings.Contains(st.Input, "jane@example.com") || !strings.Contains(st.Input, "[REDACTED:EMAIL]") {
		t.Fatalf("input not redacted: %q", st.Input)
	}
	if _, ok := st.Attributes["input.value"]; ok {
		t.Fatal("payload should not be duplicated into attributes")
	}
}

func TestUnknownModelCostIsNotGuessed(t *testing.T) {
	st := newN().Normalize("t", model.Span{Attributes: map[string]any{
		"gen_ai.operation.name": "chat", "gen_ai.request.model": "mystery", "gen_ai.usage.input_tokens": int64(10),
	}})
	if st.CostKnown || st.CostUSD != 0 {
		t.Fatalf("expected unknown cost, got %v", st.CostUSD)
	}
}

func TestFailureFromExceptionEvent(t *testing.T) {
	st := newN().Normalize("t", model.Span{Events: []model.Event{{
		Name: "exception", Attributes: map[string]any{"exception.message": "503 from crm"},
	}}})
	if st.Status != model.StatusFailure || st.ErrorMessage != "503 from crm" {
		t.Fatalf("got %s %q", st.Status, st.ErrorMessage)
	}
}

func TestPayloadTenantIsIgnored(t *testing.T) {
	st := newN().Normalize("real-tenant", model.Span{
		Attributes:    map[string]any{"tenant_id": "evil", "bindle.tenant": "evil"},
		ResourceAttrs: map[string]any{"tenant.id": "evil"},
	})
	if st.TenantID != "real-tenant" {
		t.Fatalf("tenant overridden by payload: %s", st.TenantID)
	}
}

func TestTruncateKeepsUTF8(t *testing.T) {
	s := strings.Repeat("é", 10)
	out := truncate(s, 5)
	if !strings.HasSuffix(out, "…[truncated]") || strings.ContainsRune(out[:len(out)-len("…[truncated]")], '�') {
		t.Fatalf("bad truncate: %q", out)
	}
}

func TestMaxPayloadBytesConfigurable(t *testing.T) {
	big := strings.Repeat("y", 100<<10)
	sp := model.Span{TraceID: "t", SpanID: "s", Name: "n", Attributes: map[string]any{"input.value": big}}
	if got := (&Normalizer{}).Normalize("t", sp).Input; len(got) > 33<<10 {
		t.Fatalf("default cap not applied: %d", len(got))
	}
	if got := (&Normalizer{MaxPayloadBytes: 1 << 20}).Normalize("t", sp).Input; got != big {
		t.Fatalf("raised cap not applied: %d", len(got))
	}
}

func TestIsHealthCheck(t *testing.T) {
	span := func(parent string, a map[string]any) model.Span {
		return model.Span{ParentSpanID: parent, Attributes: a}
	}
	for _, s := range []model.Span{
		span("", map[string]any{"http.method": "GET", "http.route": "/ping"}),
		span("", map[string]any{"http.request.method": "GET", "url.path": "/healthz"}),
		span("", map[string]any{"http.target": "/health?full=1"}),
	} {
		if !IsHealthCheck(s) {
			t.Errorf("not detected: %v", s.Attributes)
		}
	}
	for _, s := range []model.Span{
		span("", map[string]any{"http.method": "POST", "http.route": "/invocations"}),
		span("p", map[string]any{"http.method": "GET", "http.route": "/ping"}), // part of a larger trace
		span("", map[string]any{"http.method": "POST", "http.route": "/ping"}),
		span("", map[string]any{"gen_ai.operation.name": "chat"}),
	} {
		if IsHealthCheck(s) {
			t.Errorf("wrongly dropped: %v", s.Attributes)
		}
	}
}

func TestAgentCoreServiceSpans(t *testing.T) {
	n := newN()
	gw := map[string]any{"tool.name": "lookup_order", "gateway.id": "gw-1", "aws.operation.name": "CallTool"}
	cases := []struct {
		name  string
		kind  int32
		attrs map[string]any
		want  string
	}{
		{"AgentCore.Gateway.InvokeTool", 2, gw, model.StepTool},
		{"AgentCore.Gateway.InvokeTool", 3, gw, model.StepOther},
		{"AgentCore.Policy.AuthorizeAction", 1, map[string]any{}, model.StepGuardrail},
		{"AgentCore.Identity.GetWorkloadAccessToken", 1, map[string]any{}, model.StepOther},
	}
	for _, c := range cases {
		st := n.Normalize("t1", model.Span{Name: c.name, Kind: c.kind, ParentSpanID: "p", Attributes: c.attrs})
		if st.Type != c.want || st.Framework != "Amazon Bedrock AgentCore" {
			t.Errorf("%s kind %d: got %s/%s", c.name, c.kind, st.Type, st.Framework)
		}
	}
	// An agent's own spans aren't relabelled.
	st := n.Normalize("t1", model.Span{Name: "chat", ScopeName: "strands.telemetry.tracer", Attributes: map[string]any{"gen_ai.operation.name": "chat"}})
	if st.Framework != "Strands" || st.Type != model.StepLLM {
		t.Errorf("strands span: got %s/%s", st.Type, st.Framework)
	}
}

func TestErrorPattern(t *testing.T) {
	cases := map[string]string{
		"timeout after 30s on order 1234":                            "timeout after <n>s on order <n>",
		"timeout after 31.5s on order 99\nTraceback ...":             "timeout after <n>s on order <n>",
		`tool "lookup_order" failed: id 6a1f0c2b9d3e4f5a`:            "tool <str> failed: id <id>",
		"request 123e4567-e89b-12d3-a456-426614174000 was throttled": "request <id> was throttled",
		"   rate   limited  ":                                        "rate limited",
		"user [REDACTED:EMAIL] not found":                            "user [REDACTED:EMAIL] not found",
		"":                                                           "",
	}
	for in, want := range cases {
		if got := ErrorPattern(in); got != want {
			t.Errorf("ErrorPattern(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ErrorPattern(strings.Repeat("é", 300)); len(got) > maxErrorPattern || !utf8.ValidString(got) {
		t.Errorf("long pattern not cut cleanly: %d bytes", len(got))
	}
	st := newN().Normalize("t1", model.Span{StatusCode: 2, StatusMessage: "HTTP 503 from 10.0.0.12"})
	if st.ErrorPattern != "HTTP <n> from <n>.<n>" {
		t.Errorf("step pattern: %q", st.ErrorPattern)
	}
}

func TestScores(t *testing.T) {
	n := newN()
	st := n.Normalize("t1", model.Span{Name: "score helpful", Attributes: map[string]any{
		"bindle.step.type": "eval", "bindle.score.name": "helpful", "bindle.score.value": 0.8, "bindle.score.passed": true,
	}, ResourceAttrs: map[string]any{"bindle.agent.name": "refunds"}})
	if st.Type != model.StepEval || st.ScoreName != "helpful" || st.ScoreValue == nil || *st.ScoreValue != 0.8 || st.ScorePass != "pass" || st.AgentName != "refunds" {
		t.Errorf("in-run score: %+v", st)
	}
	ext := n.Normalize("t1", model.Span{Attributes: map[string]any{
		"bindle.score.name": "judge", "bindle.score.value": int64(3), "bindle.score.passed": false, "bindle.score.external": true,
	}, ResourceAttrs: map[string]any{"bindle.agent.name": "ci-runner", "bindle.agent.version": "ci"}})
	if ext.Type != model.StepEval || *ext.ScoreValue != 3 || ext.ScorePass != "fail" || ext.AgentName != "" || ext.AgentVersion != "" {
		t.Errorf("external score must not carry the sender's agent: %+v", ext)
	}
	if plain := n.Normalize("t1", model.Span{Attributes: map[string]any{"gen_ai.operation.name": "chat"}}); plain.ScoreName != "" {
		t.Error("non-score span")
	}
}

func TestEvaluationEvents(t *testing.T) {
	n := newN()
	span := model.Span{TraceID: strings.Repeat("a", 32), SpanID: "b7ad6b7169203331", EndUnixNano: 50, Events: []model.Event{
		{Name: "gen_ai.evaluation.result", TimeUnixNano: 40, Attributes: map[string]any{
			"gen_ai.evaluation.name": "Relevance", "gen_ai.evaluation.score.value": 4.0, "gen_ai.evaluation.score.label": "relevant",
			"gen_ai.evaluation.explanation": "Answers the question."}},
		{Name: "exception"},
	}}
	evs := EvaluationSpans(span)
	if len(evs) != 1 || evs[0].ParentSpanID != span.SpanID || evs[0].StartUnixNano != 40 {
		t.Fatalf("%+v", evs)
	}
	st := n.Normalize("t1", evs[0])
	if st.Type != model.StepEval || st.ScoreName != "Relevance" || st.ScoreValue == nil || *st.ScoreValue != 4 || st.ScorePass != "pass" ||
		st.Output != "Answers the question." {
		t.Errorf("step %+v", st)
	}
	odd := n.Normalize("t1", model.Span{Attributes: map[string]any{"bindle.step.type": "eval", "gen_ai.evaluation.name": "Tone", "gen_ai.evaluation.score.label": "friendly"}})
	if odd.ScorePass != "" || odd.ScoreName != "Tone" {
		t.Errorf("unknown labels stay unlabelled: %+v", odd)
	}
}
