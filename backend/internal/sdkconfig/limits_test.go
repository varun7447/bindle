package sdkconfig

import (
	"math"
	"strings"
	"testing"
)

func TestValidateLimits(t *testing.T) {
	ok := []Limit{{Agent: "*", MaxStepsPerRun: 100, MaxRepeatCalls: 3}, {Agent: "refunds", Paused: true, DailyBudgetUSD: 5, PauseOverBudget: true}}
	if err := ValidateLimits(ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string][]Limit{
		"empty agent":           {{Agent: ""}},
		"spaces":                {{Agent: " a"}},
		"control char":          {{Agent: "a\nb"}},
		"long":                  {{Agent: strings.Repeat("a", 129)}},
		"duplicate":             {{Agent: "a"}, {Agent: "a"}},
		"negative steps":        {{Agent: "a", MaxStepsPerRun: -1}},
		"negative repeat calls": {{Agent: "a", MaxRepeatCalls: -1}},
		"huge repeat calls":     {{Agent: "a", MaxRepeatCalls: 10_001}},
		"negative tokens":       {{Agent: "a", MaxTokensPerRun: -1}},
		"nan budget":            {{Agent: "a", DailyBudgetUSD: math.NaN()}},
		"huge budget":           {{Agent: "a", DailyBudgetUSD: 2e6}},
		"pause no budget":       {{Agent: "a", PauseOverBudget: true}},
	}
	for name, ls := range bad {
		if ValidateLimits(ls) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if ValidateLimits(make([]Limit, MaxLimits+1)) == nil {
		t.Error("too many limits accepted")
	}
}

func TestLimitFor(t *testing.T) {
	ls := []Limit{{Agent: "*", MaxStepsPerRun: 50}, {Agent: "refunds", MaxStepsPerRun: 10}}
	if l, ok := LimitFor(ls, "refunds"); !ok || l.MaxStepsPerRun != 10 {
		t.Errorf("own row: %+v", l)
	}
	if l, ok := LimitFor(ls, "other"); !ok || l.MaxStepsPerRun != 50 || l.Agent != "other" {
		t.Errorf("default row: %+v", l)
	}
	if _, ok := LimitFor(ls[1:], "other"); ok {
		t.Error("no default row")
	}
}

func TestBudgetedAgents(t *testing.T) {
	ls := []Limit{{Agent: "*", DailyBudgetUSD: 1}, {Agent: "big", DailyBudgetUSD: 10}, {Agent: "free"}}
	got := BudgetedAgents(ls, map[string]float64{"a": 0.5, "big": 3, "free": 9})
	want := map[string]float64{"a": 1, "big": 10}
	if len(got) != len(want) || got["a"] != 1 || got["big"] != 10 {
		t.Errorf("got %v want %v (free has its own row without a budget)", got, want)
	}
	if got := BudgetedAgents([]Limit{{Agent: "quiet", DailyBudgetUSD: 2}}, nil); got["quiet"] != 2 {
		t.Errorf("agents with their own budget are checked even with no spend: %v", got)
	}
}
