package cost

import "testing"

func TestLookupPrefersExactThenLongestPrefix(t *testing.T) {
	tb := New(map[string]Price{
		"acme/model-a":      {InputPerMTok: 9},
		"model-a":           {InputPerMTok: 1},
		"model-a-2026":      {InputPerMTok: 2},
		"otherco/model-a-2": {InputPerMTok: 7},
	})
	if p, _ := tb.Lookup("acme", "model-a"); p.InputPerMTok != 9 {
		t.Fatalf("provider exact: %v", p)
	}
	if p, _ := tb.Lookup("x", "model-a-2026-05-01"); p.InputPerMTok != 2 {
		t.Fatalf("longest prefix: %v", p)
	}
	if _, ok := tb.Lookup("x", "unknown"); ok {
		t.Fatal("unknown model should not match")
	}
}

func TestComputeWithCache(t *testing.T) {
	tb := New(map[string]Price{"m": {InputPerMTok: 10, CachedInputPerMTok: 1, OutputPerMTok: 20}})
	c, ok := tb.Compute("", "m", 1_000_000, 1_000_000, 500_000)
	if !ok || c != 5+0.5+20 {
		t.Fatalf("got %v", c)
	}
}
