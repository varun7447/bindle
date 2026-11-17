package cost

import (
	"math"
	"path/filepath"
	"runtime"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestLookupExactThenVersionedPrefix(t *testing.T) {
	tb := New(map[string]Price{
		"acme/model-a":             {InputPerMTok: 9},
		"model-a":                  {InputPerMTok: 1},
		"model-a-2026":             {InputPerMTok: 2},
		"otherco/model-a-2":        {InputPerMTok: 7},
		"anthropic/claude-fable-5": {InputPerMTok: 10},
		"openai/gpt-5":             {InputPerMTok: 1.25},
	})
	if p, _ := tb.Lookup("acme", "model-a"); p.InputPerMTok != 9 {
		t.Fatalf("provider exact: %v", p)
	}
	if p, _ := tb.Lookup("x", "model-a-20260501"); p.InputPerMTok != 1 {
		t.Fatalf("dated suffix: %v", p)
	}
	for _, m := range []struct{ provider, model string }{
		{"x", "unknown"},
		{"openai", "gpt-5-mini"},                   // a different model, not a version
		{"anthropic", "claude-fable-5-3"},          // a future model must not inherit a price
		{"aws.bedrock", "claude-fable-5-20260101"}, // partner pricing differs: no match across providers
	} {
		if _, ok := tb.Lookup(m.provider, m.model); ok {
			t.Errorf("%s/%s should not match", m.provider, m.model)
		}
	}
	for _, m := range []string{"gpt-5-2025-08-07", "gpt-5@2025-08-07", "gpt-5-latest"} {
		if p, ok := tb.Lookup("openai", m); !ok || p.InputPerMTok != 1.25 {
			t.Errorf("%s should match gpt-5", m)
		}
	}
	if p, ok := tb.Lookup("anthropic", "claude-fable-5-20260101"); !ok || p.InputPerMTok != 10 {
		t.Fatal("dated Claude ID")
	}
}

func TestCompute(t *testing.T) {
	tb := New(map[string]Price{
		"m":       {InputPerMTok: 10, CachedInputPerMTok: F(1), OutputPerMTok: 20},
		"nocache": {InputPerMTok: 10, OutputPerMTok: 20},
		"tiered": {InputPerMTok: 0.1, OutputPerMTok: 0.5, CachedInputPerMTok: F(0.01),
			LongAbove: 100_000, LongInputPerMTok: F(0.5), LongOutputPerMTok: F(2.5), LongCachedInputPerMTok: F(0.05)},
		"tiered-no-long": {InputPerMTok: 1, OutputPerMTok: 1, LongAbove: 10},
	})
	if c, ok := tb.Compute("", "m", 1_000_000, 1_000_000, 500_000); !ok || !near(c, 5+0.5+20) {
		t.Fatalf("cache: %v", c)
	}
	if c, ok := tb.Compute("", "nocache", 1_000_000, 0, 0); !ok || !near(c, 10) {
		t.Fatalf("no cache used: %v", c)
	}
	if _, ok := tb.Compute("", "nocache", 1_000_000, 0, 10); ok {
		t.Fatal("cached tokens on a model without a cached price must be unknown")
	}
	if c, ok := tb.Compute("", "tiered", 100_000, 1_000_000, 0); !ok || !near(c, 0.01+0.5) {
		t.Fatalf("at the threshold uses base prices: %v", c)
	}
	if c, ok := tb.Compute("", "tiered", 200_000, 1_000_000, 100_000); !ok || !near(c, 0.05+0.005+2.5) {
		t.Fatalf("over the threshold uses long prices: %v", c)
	}
	if _, ok := tb.Compute("", "tiered-no-long", 11, 1, 0); ok {
		t.Fatal("missing long-prompt price must be unknown")
	}
}

// The shipped table loads, and spot-checks against the providers' pages.
func TestShippedTable(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	tb, err := Load(filepath.Join(filepath.Dir(here), "..", "..", "pricing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if tb.Len() < 30 {
		t.Fatalf("only %d models", tb.Len())
	}
	cases := []struct {
		provider, model string
		in, out, cached int64
		want            float64
	}{
		{"anthropic", "claude-opus-5-5", 1_000_000, 1_000_000, 0, 4 + 20},
		{"anthropic", "claude-sonnet-5-5", 1_000_000, 0, 1_000_000, 0.10},
		{"anthropic", "claude-haiku-5-5", 200_000, 0, 0, 0.2 * 0.50},  // over 100K: long prices
		{"anthropic", "claude-opus-4-5-20251101", 1_000_000, 0, 0, 5}, // dated ID
		{"openai", "gpt-5.4", 300_000, 0, 0, 0.3 * 5.00},              // over 272K: long prices
		{"openai", "gpt-4o-mini", 1_000_000, 1_000_000, 0, 0.15 + 0.60},
		{"openai", "gpt-4o-2024-08-06", 1_000_000, 0, 0, 2.50},
	}
	for _, c := range cases {
		got, ok := tb.Compute(c.provider, c.model, c.in, c.out, c.cached)
		if !ok || !near(got, c.want) {
			t.Errorf("%s/%s: got %v %v, want %v", c.provider, c.model, got, ok, c.want)
		}
	}
	if _, ok := tb.Compute("openai", "gpt-5-pro", 1000, 0, 10); ok {
		t.Error("gpt-5-pro has no published cached price")
	}
}
