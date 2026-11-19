package evals

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := []Scorer{
		{Name: "mentions refund", Kind: KindContains, Value: "refund"},
		{Name: "order id", Kind: KindRegex, Value: `ord_\d+`},
		{Name: "json", Kind: KindJSONValid},
		{Name: "cheap", Kind: KindMaxCost, Number: 0.05, Agent: "refunds"},
		{Name: "ok", Kind: KindSucceeded},
	}
	for _, s := range ok {
		if err := Validate(s); err != nil {
			t.Errorf("%s: %v", s.Name, err)
		}
	}
	bad := []Scorer{
		{Name: "", Kind: KindJSONValid},
		{Name: " x", Kind: KindJSONValid},
		{Name: "x", Kind: "llm_judge"},
		{Name: "x", Kind: KindContains},
		{Name: "x", Kind: KindRegex, Value: "(a+)+"},
		{Name: "x", Kind: KindMaxSteps},
		{Name: "x", Kind: KindMaxSteps, Number: -1},
		{Name: "x", Kind: KindContains, Value: strings.Repeat("a", 501)},
	}
	for _, s := range bad {
		if Validate(s) == nil {
			t.Errorf("accepted %+v", s)
		}
	}
}

func TestEvaluate(t *testing.T) {
	r := Run{Output: `Your refund for ORD_42 was issued.`, Tools: []string{"lookup_order"}, Steps: 7, CostUSD: 0.02, CostKnown: true, DurationMs: 900}
	cases := []struct {
		s    Scorer
		pass bool
		skip bool
	}{
		{Scorer{Kind: KindContains, Value: "REFUND"}, true, false},
		{Scorer{Kind: KindContains, Value: "REFUND", CaseSensitive: true}, false, false},
		{Scorer{Kind: KindNotContains, Value: "sorry"}, true, false},
		{Scorer{Kind: KindRegex, Value: `ord_\d+`}, true, false},
		{Scorer{Kind: KindRegex, Value: `ord_\d+`, CaseSensitive: true}, false, false},
		{Scorer{Kind: KindJSONValid}, false, false},
		{Scorer{Kind: KindToolCalled, Value: "lookup_order"}, true, false},
		{Scorer{Kind: KindToolNotCalled, Value: "lookup_order"}, false, false},
		{Scorer{Kind: KindMaxSteps, Number: 5}, false, false},
		{Scorer{Kind: KindMaxCost, Number: 0.05}, true, false},
		{Scorer{Kind: KindMaxDuration, Number: 500}, false, false},
		{Scorer{Kind: KindSucceeded}, true, false},
	}
	for i, c := range cases {
		got := Evaluate(c.s, r)
		if got.Pass != c.pass || got.Skipped != c.skip {
			t.Errorf("case %d %s: %+v", i, c.s.Kind, got)
		}
		if !got.Pass && !got.Skipped && got.Detail == "" {
			t.Errorf("case %d: failures explain themselves", i)
		}
	}
	if !Evaluate(Scorer{Kind: KindJSONValid}, Run{Output: ` {"a": 1} `}).Pass {
		t.Error("valid JSON with spaces")
	}
	if got := Evaluate(Scorer{Kind: KindContains, Value: "x"}, Run{}); !got.Skipped {
		t.Error("no output: skipped, not failed")
	}
	if got := Evaluate(Scorer{Kind: KindMaxCost, Number: 1}, Run{CostKnown: false}); !got.Skipped {
		t.Error("unpriced run: skipped")
	}
}

func TestTally(t *testing.T) {
	var ta Tally
	if ta.PassRate() != -1 {
		t.Fatal("empty tally")
	}
	for _, r := range []Result{{Pass: true}, {Pass: true}, {}, {Skipped: true}} {
		ta.Add(r)
	}
	if ta.Passed != 2 || ta.Failed != 1 || ta.Skipped != 1 || ta.PassRate() < 0.66 || ta.PassRate() > 0.67 {
		t.Errorf("%+v %v", ta, ta.PassRate())
	}
	if !(Scorer{}).Applies("x") || (Scorer{Agent: "a"}).Applies("b") {
		t.Error("Applies")
	}
}

// TestSharedVectors keeps the Go checks and the Python SDK's local checks
// (bindle eval) in agreement: both run testdata/check_vectors.json.
func TestSharedVectors(t *testing.T) {
	b, err := os.ReadFile("testdata/check_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Run   map[string]any `json:"run"`
		Cases []struct {
			Scorer  Scorer         `json:"scorer"`
			Run     map[string]any `json:"run"`
			Pass    bool           `json:"pass"`
			Skipped bool           `json:"skipped"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for i, c := range v.Cases {
		merged := map[string]any{}
		for k, x := range v.Run {
			merged[k] = x
		}
		for k, x := range c.Run {
			merged[k] = x
		}
		mb, _ := json.Marshal(merged)
		var r Run
		if err := json.Unmarshal(mb, &r); err != nil {
			t.Fatal(err)
		}
		got := Evaluate(c.Scorer, r)
		if got.Pass != c.Pass || got.Skipped != c.Skipped {
			t.Errorf("vector %d (%s): got %+v", i, c.Scorer.Kind, got)
		}
	}
}
