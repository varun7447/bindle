package normalize

import (
	"strings"
	"testing"

	"github.com/bindle-ai/bindle/services/internal/cost"
	"github.com/bindle-ai/bindle/services/internal/model"
	"github.com/bindle-ai/bindle/services/internal/redact"
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

func TestLLMFieldsAndCost(t *testing.T) {
	n := newN()
	st := n.Normalize("acme", model.Span{
		TraceID: "t", SpanID: "s", StartUnixNano: 1_000_000, EndUnixNano: 3_000_000,
		ScopeName: "openinference.instrumentation.openai_agents",
		Attributes: map[string]any{
			"gen_ai.operation.name":      "chat",
			"gen_ai.system":              "anthropic",
			"gen_ai.request.model":       "test-model-2026",
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
