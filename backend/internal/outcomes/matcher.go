package outcomes

import (
	"context"
	"log/slog"
	"time"
)

// Finder finds the runs (trace IDs) whose steps carry any of the stored
// correlations, in [from, to). Implemented by search.Store (read-only).
type Finder interface {
	RunsByCorrelation(ctx context.Context, tenant string, correlations []string, from, to time.Time) ([]string, error)
}

// MatchStore is the matcher's view of the control plane.
type MatchStore interface {
	DueOutcomeTenants(ctx context.Context) ([]string, error)
	DueOutcomeEvents(ctx context.Context, tenant string, limit int) ([]Event, error)
	SaveOutcomeAttempt(ctx context.Context, tenant string, ev Event) error
	PruneOutcomeEvents(ctx context.Context) (int64, error)
}

// Matcher links pending events to runs. It runs in the alerts worker.
type Matcher struct {
	Store  MatchStore
	Find   Finder
	Log    *slog.Logger
	Now    func() time.Time
	Budget int // events per tenant per pass; default 200
}

func (m *Matcher) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Attempt decides one event's next state given the runs found for it.
func Attempt(ev Event, runs []string, now time.Time) Event {
	ev.Attempts++
	switch {
	case len(runs) > 0:
		ev.Status = StatusMatched
		ev.MatchedTraces = runs
		t := now
		ev.MatchedAt = &t
	case !now.Before(ev.ExpiresAt):
		ev.Status = StatusUnmatched
	default:
		next := now.Add(Backoff(ev.Attempts - 1))
		if next.After(ev.ExpiresAt) {
			next = ev.ExpiresAt // one last try when the window ends
		}
		ev.NextAttemptAt = next
	}
	return ev
}

// RunOnce makes one matching attempt for every due event.
func (m *Matcher) RunOnce(ctx context.Context) (matched, unmatched int, err error) {
	tenants, err := m.Store.DueOutcomeTenants(ctx)
	if err != nil {
		return 0, 0, err
	}
	budget := m.Budget
	if budget <= 0 {
		budget = 200
	}
	for _, t := range tenants {
		events, err := m.Store.DueOutcomeEvents(ctx, t, budget)
		if err != nil {
			m.warn("outcome events lookup failed", t, err)
			continue
		}
		for _, ev := range events {
			now := m.now()
			runs, err := m.Find.RunsByCorrelation(ctx, t, ev.Correlations, now.Add(-MatchLookback), now.Add(time.Hour))
			if err != nil {
				m.warn("outcome matching failed", t, err)
				break // search is down: leave the rest due, try again next pass
			}
			next := Attempt(ev, runs, now)
			if err := m.Store.SaveOutcomeAttempt(ctx, t, next); err != nil {
				m.warn("outcome attempt not saved", t, err)
				continue
			}
			switch next.Status {
			case StatusMatched:
				matched++
			case StatusUnmatched:
				unmatched++
			}
		}
	}
	return matched, unmatched, nil
}

// Run matches every interval until ctx ends, and prunes old events daily.
func (m *Matcher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		pctx, cancel := context.WithTimeout(ctx, interval*4)
		matched, unmatched, err := m.RunOnce(pctx)
		if err != nil {
			m.warn("outcome matcher failed", "", err)
		} else if matched+unmatched > 0 && m.Log != nil {
			m.Log.Info("outcome events", "matched", matched, "unmatched", unmatched)
		}
		if m.now().Sub(lastPrune) > 24*time.Hour {
			if n, err := m.Store.PruneOutcomeEvents(pctx); err != nil {
				m.warn("outcome events prune failed", "", err)
			} else {
				lastPrune = m.now()
				if n > 0 && m.Log != nil {
					m.Log.Info("outcome events pruned", "events", n)
				}
			}
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Matcher) warn(msg, tenant string, err error) {
	if m.Log != nil {
		m.Log.Warn(msg, "tenant", tenant, "err", err)
	}
}
