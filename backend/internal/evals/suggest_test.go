package evals

import "testing"

func TestLiteralFragment(t *testing.T) {
	cases := map[string]string{
		"timeout after <n>s on order <n>":        "timeout after",
		"<str> not found":                        "not found",
		"<str> <n>":                              "",
		"connection refused by upstream <id>":    "connection refused by upstream",
		"":                                       "",
		`invalid argument "<str>": expected <n>`: "invalid argument",
		"rate limit exceeded, retry in <n> seconds": "rate limit exceeded, retry in",
	}
	for in, want := range cases {
		if got := literalFragment(in); got != want {
			t.Errorf("literalFragment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSuggestOnlyWhatCatches(t *testing.T) {
	good := []Run{}
	for i := 0; i < 6; i++ {
		good = append(good, Run{Output: `{"ok":true}`, Tools: []string{"search"}, Steps: 6})
	}
	problem := []Run{{Output: `{"ok":false}`, Tools: []string{"search"}, Steps: 6}}
	// Nothing about the problem run differs: nothing is suggested.
	if got := Suggest(SuggestInput{Problem: problem, Baseline: good, ErrorPattern: "x"}); len(got) != 0 {
		t.Errorf("suggested %+v", got)
	}
	problem = []Run{{Output: "not json", Steps: 30, Failed: true}}
	got := Suggest(SuggestInput{Problem: problem, Baseline: good, Agent: "a"})
	kinds := map[string]Suggestion{}
	for _, s := range got {
		kinds[s.Scorer.Kind] = s
		if s.Scorer.Agent != "a" || !s.Scorer.Enabled || Validate(s.Scorer) != nil || s.Reason == "" {
			t.Errorf("bad suggestion %+v", s)
		}
	}
	for _, k := range []string{KindSucceeded, KindToolCalled, KindMaxSteps, KindJSONValid} {
		if kinds[k].Catches != 1 {
			t.Errorf("%s: %+v", k, kinds[k])
		}
	}
	if kinds[KindMaxSteps].Scorer.Number != 12 {
		t.Errorf("step budget %v", kinds[KindMaxSteps].Scorer.Number)
	}
	// Too few good runs to know what normal looks like.
	if got := Suggest(SuggestInput{Problem: problem, Baseline: good[:2]}); len(got) != 1 || got[0].Scorer.Kind != KindSucceeded {
		t.Errorf("small baseline: %+v", got)
	}
}
