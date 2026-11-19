package alerts

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/sdkconfig"
)

type fakeBudgetStore struct {
	tenants  []BudgetTenant
	recorded map[string]bool
}

func (f *fakeBudgetStore) BudgetTenants(context.Context) ([]BudgetTenant, error) {
	return f.tenants, nil
}
func (f *fakeBudgetStore) RecordBudgetExceeded(_ context.Context, tenant, agent string, day time.Time, _, _ float64) (bool, error) {
	k := tenant + "/" + agent + "/" + day.Format("2006-01-02")
	if f.recorded[k] {
		return false, nil
	}
	f.recorded[k] = true
	return true, nil
}

type fakeSpend map[string]float64

func (f fakeSpend) CostByAgent(context.Context, string, time.Time, time.Time) (map[string]float64, error) {
	return f, nil
}

func TestBudgets(t *testing.T) {
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, time.UTC)
	st := &fakeBudgetStore{recorded: map[string]bool{}, tenants: []BudgetTenant{{Tenant: "t1", Limits: []sdkconfig.Limit{
		{Agent: "*", DailyBudgetUSD: 1},
		{Agent: "big", DailyBudgetUSD: 10, PauseOverBudget: true},
	}}}}
	b := &Budgets{Store: st, Spend: fakeSpend{"small": 1.5, "big": 9.99, "tiny": 0.2}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	if err := b.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !st.recorded["t1/small/2026-11-17"] || len(st.recorded) != 1 {
		t.Errorf("recorded %v, want only small (big is under its own budget)", st.recorded)
	}
	b.Spend = fakeSpend{"small": 3, "big": 10}
	_ = b.RunOnce(context.Background())
	if !st.recorded["t1/big/2026-11-17"] || len(st.recorded) != 2 {
		t.Errorf("recorded %v", st.recorded)
	}
}
