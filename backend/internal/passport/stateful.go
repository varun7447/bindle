package passport

import (
	"context"
	"fmt"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/query"
)

// Counter answers the questions rate and budget drift need. Implemented by
// *search.Store (and query.Store for the run and spend parts).
type Counter interface {
	// ToolCalls counts an agent's tool steps whose tool name matches
	// pattern (* wildcards) in [from, to), with the latest run as a sample.
	ToolCalls(ctx context.Context, tenant, agent, pattern string, from, to time.Time) (uint64, string, error)
	CostlyRuns(ctx context.Context, tenant string, from, to time.Time, minCostUSD float64, limit int) ([]query.Run, error)
	CostByAgent(ctx context.Context, tenant string, from, to time.Time) (map[string]float64, error)
}

// Finding is one drift that depends on more than one step: a tool over its
// rate limit, a run over budget, or a day over budget.
type Finding struct {
	Kind    string  `json:"kind"`
	Agent   string  `json:"agent"`
	Detail  string  `json:"detail"`             // tool pattern, trace ID or UTC day
	Value   float64 `json:"value"`              // calls, or USD
	Limit   float64 `json:"limit"`              // the passport's limit
	Window  string  `json:"window,omitempty"`   // rate limits only
	TraceID string  `json:"trace_id,omitempty"` // a run to open
	Summary string  `json:"summary"`
}

// Reason is the finding as a drift reason ("kind:detail").
func (f Finding) Reason() string { return f.Kind + ":" + f.Detail }

// Slot makes a finding count once per period: once per rate window for rate
// limits; once ever for run and day budgets, whose detail is already the
// trace ID or the UTC day.
func (f Finding) Slot(now time.Time) string {
	if f.Kind == RateOverLimit {
		d, err := parseWindow(f.Window)
		if err != nil || d <= 0 {
			d = time.Minute
		}
		return fmt.Sprint(now.Truncate(d).Unix())
	}
	return "once"
}

const maxBudgetRuns = 20

// Stateful checks rate limits (each in the window ending at now), the
// per-run budget (runs started in [runsFrom, now)) and the daily budget
// (priced spend since 00:00 UTC). Unknown prices never count toward a
// budget: spend is what Bindle could price.
func Stateful(ctx context.Context, c Counter, tenant string, p *Passport, runsFrom, now time.Time) ([]Finding, error) {
	if p == nil {
		return nil, nil
	}
	var out []Finding
	for _, l := range p.Limits {
		d := l.Duration()
		n, trace, err := c.ToolCalls(ctx, tenant, p.Agent, l.Tool, now.Add(-d), now)
		if err != nil {
			return nil, err
		}
		if int64(n) > l.Max {
			out = append(out, Finding{Kind: RateOverLimit, Agent: p.Agent, Detail: l.Tool, Value: float64(n), Limit: float64(l.Max),
				Window: l.Window, TraceID: trace,
				Summary: fmt.Sprintf("%s called %s %d times in the last %s (passport limit %d)", p.Agent, l.Tool, n, l.Window, l.Max)})
		}
	}
	if b := p.Budget; b != nil && b.PerRunUSD > 0 {
		runs, err := c.CostlyRuns(ctx, tenant, runsFrom, now, b.PerRunUSD, 200)
		if err != nil {
			return nil, err
		}
		count := 0
		for _, r := range runs {
			if r.Agent != p.Agent || r.CostUSD <= b.PerRunUSD {
				continue
			}
			if count++; count > maxBudgetRuns {
				break
			}
			out = append(out, Finding{Kind: RunOverBudget, Agent: p.Agent, Detail: r.TraceID, Value: r.CostUSD, Limit: b.PerRunUSD, TraceID: r.TraceID,
				Summary: fmt.Sprintf("A run of %s cost %s (passport per-run budget %s)", p.Agent, usd(r.CostUSD), usd(b.PerRunUSD))})
		}
	}
	if b := p.Budget; b != nil && b.PerDayUSD > 0 {
		day := now.UTC().Truncate(24 * time.Hour)
		spend, err := c.CostByAgent(ctx, tenant, day, now)
		if err != nil {
			return nil, err
		}
		if v := spend[p.Agent]; v > b.PerDayUSD {
			out = append(out, Finding{Kind: DayOverBudget, Agent: p.Agent, Detail: day.Format("2006-01-02"), Value: v, Limit: b.PerDayUSD,
				Summary: fmt.Sprintf("%s has spent %s today (passport daily budget %s)", p.Agent, usd(v), usd(b.PerDayUSD))})
		}
	}
	return out, nil
}

func usd(v float64) string {
	if v != 0 && v < 0.01 && v > -0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}
