package alerts

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/sdkconfig"
)

// KindDailyBudget marks budget notifications. They are not rules: budgets
// live in the tenant's limits (see sdkconfig.Limit).
const KindDailyBudget = "daily_budget"

// BudgetTenant is a tenant with at least one daily budget.
type BudgetTenant struct {
	Tenant string
	Limits []sdkconfig.Limit
}

// BudgetStore is what budget evaluation needs from the control plane.
type BudgetStore interface {
	BudgetTenants(ctx context.Context) ([]BudgetTenant, error)
	// RecordBudgetExceeded records that agent exceeded its budget on day,
	// once; it reports whether this call recorded it (and so must notify).
	RecordBudgetExceeded(ctx context.Context, tenant, agent string, day time.Time, spent, budget float64) (bool, error)
}

// SpendSource gives priced spend per agent (search.Store).
type SpendSource interface {
	CostByAgent(ctx context.Context, tenant string, from, to time.Time) (map[string]float64, error)
}

// Budgets checks daily budgets and notifies each tenant's alert channels
// once per agent and UTC day. Pausing is not done here: the SDK reads the
// over-budget list with its config and pauses per the agent's limit.
type Budgets struct {
	Store    BudgetStore
	Channels Store // for the tenant's alert channels
	Spend    SpendSource
	Deliver  *Deliverer
	Log      interface {
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
	Now func() time.Time
}

// RunOnce checks every tenant with a budget.
func (b *Budgets) RunOnce(ctx context.Context) error {
	tenants, err := b.Store.BudgetTenants(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if b.Now != nil {
		now = b.Now().UTC()
	}
	day := now.Truncate(24 * time.Hour)
	for _, t := range tenants {
		if err := b.tenant(ctx, t, day, now); err != nil {
			b.Log.Error("budget check failed", "tenant", t.Tenant, "err", err)
		}
	}
	return nil
}

func (b *Budgets) tenant(ctx context.Context, t BudgetTenant, day, now time.Time) error {
	spend, err := b.Spend.CostByAgent(ctx, t.Tenant, day, now)
	if err != nil {
		return err
	}
	budgets := sdkconfig.BudgetedAgents(t.Limits, spend)
	agents := make([]string, 0, len(budgets))
	for a := range budgets {
		agents = append(agents, a)
	}
	sort.Strings(agents)
	for _, agent := range agents {
		budget, spent := budgets[agent], spend[agent]
		if spent < budget {
			continue
		}
		created, err := b.Store.RecordBudgetExceeded(ctx, t.Tenant, agent, day, spent, budget)
		if err != nil || !created {
			if err != nil {
				b.Log.Warn("recording budget state failed", "tenant", t.Tenant, "agent", agent, "err", err)
			}
			continue
		}
		lim, _ := sdkconfig.LimitFor(t.Limits, agent)
		b.notify(ctx, t.Tenant, agent, spent, budget, lim.PauseOverBudget, now)
	}
	return nil
}

func (b *Budgets) notify(ctx context.Context, tenant, agent string, spent, budget float64, paused bool, now time.Time) {
	if b.Channels == nil || b.Deliver == nil {
		return
	}
	channels, err := b.Channels.Channels(ctx, tenant)
	if err != nil {
		b.Log.Error("loading channels failed", "tenant", tenant, "err", err)
		return
	}
	what := "Agents keep running."
	if paused {
		what = "The agent is paused until 00:00 UTC (where its code calls bindle.check_limits())."
	}
	ev := Event{
		ID: newEventID(), TenantID: tenant, RuleID: "budget", Kind: KindDailyBudget,
		Fingerprint: "budget:" + agent + ":" + now.Format("2006-01-02"),
		Summary:     fmt.Sprintf("%s spent %s today, over its daily budget of %s. %s", orDash(agent), usd(spent), usd(budget), what),
		Details:     map[string]any{"agent": agent, "spent_usd": spent, "budget_usd": budget, "paused": paused},
		FiredAt:     now,
	}
	for _, ch := range channels {
		if !ch.Enabled {
			continue
		}
		if err := b.Deliver.Send(ctx, ch, ev, Rule{ID: "budget", Kind: KindDailyBudget, Threshold: budget}); err != nil {
			b.Log.Warn("budget notification failed", "tenant", tenant, "channel", ch.ID, "err", err)
		}
	}
}
