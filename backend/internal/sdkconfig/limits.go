package sdkconfig

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
)

// Limit caps what one agent may do. Agent "*" applies to every agent that
// has no row of its own. Zero means "no limit".
//
// The SDK enforces Paused, MaxStepsPerRun and MaxTokensPerRun where the
// agent's code calls bindle.check_limits(). DailyBudgetUSD is evaluated by
// Bindle (alerts worker) from priced spend since 00:00 UTC; when it is
// exceeded Bindle notifies the alert channels and, with PauseOverBudget,
// reports the agent as paused until the day ends.
type Limit struct {
	Agent           string  `json:"agent"`
	Paused          bool    `json:"paused"`
	MaxStepsPerRun  int64   `json:"max_steps_per_run"`
	MaxTokensPerRun int64   `json:"max_tokens_per_run"`
	DailyBudgetUSD  float64 `json:"daily_budget_usd"`
	PauseOverBudget bool    `json:"pause_over_budget"`
	// MaxRepeatCalls stops a run that calls the same tool with the same
	// arguments more than this many times (a loop guard), checked by the SDK.
	MaxRepeatCalls int64 `json:"max_repeat_calls"`
}

const (
	MaxLimits      = 50
	maxAgentName   = 128
	maxStepsLimit  = 1_000_000
	maxTokensLimit = 1_000_000_000_000
	maxDailyBudget = 1_000_000
	maxRepeatLimit = 10_000
	AnyAgent       = "*"
)

// ValidateLimits checks limits before they are saved.
func ValidateLimits(ls []Limit) error {
	if len(ls) > MaxLimits {
		return fmt.Errorf("at most %d limits", MaxLimits)
	}
	seen := map[string]bool{}
	for i, l := range ls {
		name := l.Agent
		if name == "" || len(name) > maxAgentName || strings.TrimSpace(name) != name || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			return fmt.Errorf("limit %d: agent must be 1-%d characters without leading or trailing spaces, or * for all agents", i+1, maxAgentName)
		}
		if seen[name] {
			return fmt.Errorf("limit %d: agent %s is listed twice", i+1, name)
		}
		seen[name] = true
		if l.MaxStepsPerRun < 0 || l.MaxStepsPerRun > maxStepsLimit {
			return fmt.Errorf("limit %s: max_steps_per_run must be between 0 and %d", name, maxStepsLimit)
		}
		if l.MaxTokensPerRun < 0 || l.MaxTokensPerRun > maxTokensLimit {
			return fmt.Errorf("limit %s: max_tokens_per_run must be between 0 and %d", name, int64(maxTokensLimit))
		}
		if l.MaxRepeatCalls < 0 || l.MaxRepeatCalls > maxRepeatLimit {
			return fmt.Errorf("limit %s: max_repeat_calls must be between 0 and %d", name, maxRepeatLimit)
		}
		if math.IsNaN(l.DailyBudgetUSD) || l.DailyBudgetUSD < 0 || l.DailyBudgetUSD > maxDailyBudget {
			return fmt.Errorf("limit %s: daily_budget_usd must be between 0 and %d", name, maxDailyBudget)
		}
		if l.PauseOverBudget && l.DailyBudgetUSD == 0 {
			return errors.New("limit " + name + ": pause_over_budget needs a daily_budget_usd")
		}
	}
	return nil
}

// LimitFor returns the limit that applies to agent: its own row, else "*".
func LimitFor(ls []Limit, agent string) (Limit, bool) {
	var any *Limit
	for i := range ls {
		if ls[i].Agent == agent && agent != AnyAgent {
			return ls[i], true
		}
		if ls[i].Agent == AnyAgent {
			any = &ls[i]
		}
	}
	if any != nil {
		l := *any
		l.Agent = agent
		return l, true
	}
	return Limit{}, false
}

// BudgetedAgents lists the agents to check against a daily budget, given
// today's spend per agent: every agent with its own budget, plus every
// agent seen today when "*" has one. It returns agent -> budget.
func BudgetedAgents(ls []Limit, spend map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for _, l := range ls {
		if l.Agent != AnyAgent && l.DailyBudgetUSD > 0 {
			out[l.Agent] = l.DailyBudgetUSD
		}
	}
	for agent := range spend {
		if _, own := out[agent]; own {
			continue
		}
		if l, ok := LimitFor(ls, agent); ok && l.DailyBudgetUSD > 0 {
			out[agent] = l.DailyBudgetUSD
		}
	}
	return out
}
