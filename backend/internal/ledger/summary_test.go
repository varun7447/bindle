package ledger

import (
	"strings"
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/model"
)

func TestSummary(t *testing.T) {
	es := []Entry{
		{Seq: 7, Kind: KindStep, TraceID: "r1", StepType: model.StepLLM, StepName: "plan", Status: model.StatusSuccess, CostUSD: "0.0025", CostKnown: true, Hash: []byte{1}},
		{Seq: 8, Kind: KindStep, TraceID: "r1", StepType: model.StepTool, StepName: "refund_payment", ToolName: "refund_payment", Status: model.StatusFailure, ErrorClass: "payments API timed out after <n>ms", CostUSD: "0"},
		{Seq: 9, Kind: KindStep, TraceID: "r2", StepType: model.StepTool, StepName: "refund_payment", ToolName: "refund_payment", Status: model.StatusFailure, ErrorClass: "payments API timed out after <n>ms", CostUSD: "0", SecurityFlags: []string{"prompt_injection"}},
		{Seq: 10, Kind: KindStep, TraceID: "r2", StepType: model.StepLLM, StepName: "respond", Status: model.StatusSuccess, CostUSD: "0", CostKnown: false, Hash: []byte{2}},
	}
	s := Summary("refund-agent", "2026-11-23", es, Result{OK: true, Entries: 4})
	for _, want := range []string{
		"# Ledger summary: refund-agent, 2026-11-23 (UTC)",
		"2 runs, 4 steps: 2 LLM calls, 2 tool calls.",
		"Most-used tools: refund_payment (2).",
		"2 steps failed:",
		`- refund_payment (tool call): "payments API timed out after <n>ms", 2 times`,
		"At least $0.0025 across 1 priced step; 1 model call had no known price, so this is a lower bound.",
		"1 step flagged for review: prompt_injection (1).",
		"Ledger entries 7 to 10 (4 entries)",
		"Last entry hash: `02`",
		"intact (4 entries verified)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary is missing %q:\n%s", want, s)
		}
	}
	if Summary("refund-agent", "2026-11-23", es, Result{OK: true, Entries: 4}) != s {
		t.Fatal("summary is not deterministic")
	}
	broken := Summary("", "2026-11-23", es[:1], Result{FirstBroken: &Broken{Seq: 7, Reason: "its hash doesn't match its contents"}})
	if !strings.Contains(broken, "Unnamed agent") || !strings.Contains(broken, "broken at entry 7") || !strings.Contains(broken, "Nothing: no step failed") {
		t.Fatalf("broken summary:\n%s", broken)
	}
	unknown := Summary("a", "2026-11-23", es[3:], Result{OK: true, Entries: 1})
	if !strings.Contains(unknown, "**Cost.** Unknown: 1 model call had no known price.") {
		t.Fatalf("unknown cost:\n%s", unknown)
	}
	if empty := Summary("a", "2026-11-23", nil, Result{OK: true}); !strings.Contains(empty, "No ledger entries") {
		t.Fatalf("empty: %s", empty)
	}
}
