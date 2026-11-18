package normalize

import (
	"strings"
	"testing"

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
