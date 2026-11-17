package alerts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"
)

// Evaluator checks every tenant's enabled rules and delivers new alerts.
type Evaluator struct {
	Store      Store
	Detect     Detector
	Deliver    *Deliverer
	Log        *slog.Logger
	Now        func() time.Time
	MaxPerRule int // new events per rule per evaluation; default 20
}

// Run evaluates every interval until ctx ends.
func (e *Evaluator) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := e.RunOnce(ctx); err != nil && ctx.Err() == nil {
			e.Log.Error("alert evaluation failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce evaluates all tenants once. One tenant's failure doesn't stop
// the others.
func (e *Evaluator) RunOnce(ctx context.Context) error {
	tenants, err := e.Store.AlertingTenants(ctx)
	if err != nil {
		return err
	}
	for _, t := range tenants {
		if err := e.evaluateTenant(ctx, t); err != nil {
			e.Log.Error("tenant alert evaluation failed", "tenant", t, "err", err)
		}
	}
	return nil
}

func (e *Evaluator) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Evaluator) evaluateTenant(ctx context.Context, tenant string) error {
	rules, err := e.Store.Rules(ctx, tenant)
	if err != nil {
		return err
	}
	var channels []Channel
	loaded := false
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		events, err := e.detect(ctx, tenant, r)
		if err != nil {
			e.Log.Warn("rule check failed", "tenant", tenant, "rule", r.ID, "err", err)
			continue
		}
		for _, ev := range events {
			created, err := e.Store.RecordEvent(ctx, ev)
			if err != nil || !created {
				continue // fired before (or by another evaluator)
			}
			if !loaded {
				if channels, err = e.Store.Channels(ctx, tenant); err != nil {
					e.Log.Error("loading channels failed", "tenant", tenant, "err", err)
				}
				loaded = true
			}
			delivery := map[string]string{}
			for _, ch := range channels {
				if !ch.Enabled {
					continue
				}
				if err := e.Deliver.Send(ctx, ch, ev, r); err != nil {
					delivery[ch.ID] = "failed: " + err.Error()
					e.Log.Warn("alert delivery failed", "tenant", tenant, "channel", ch.ID, "err", err)
				} else {
					delivery[ch.ID] = "sent"
				}
			}
			if len(delivery) > 0 {
				_ = e.Store.SetDelivery(ctx, tenant, ev.ID, delivery)
			}
		}
	}
	return nil
}

func newEventID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return "evt_" + hex.EncodeToString(b)
}

// detect returns candidate events; fingerprints make repeats no-ops.
func (e *Evaluator) detect(ctx context.Context, tenant string, r Rule) ([]Event, error) {
	now := e.now()
	window := time.Duration(r.WindowMinutes) * time.Minute
	from := now.Add(-window)
	limit := e.MaxPerRule
	if limit <= 0 {
		limit = 20
	}
	ev := func(fp, summary, traceID string, details map[string]any) Event {
		return Event{ID: newEventID(), TenantID: tenant, RuleID: r.ID, Kind: r.Kind, Fingerprint: fp,
			Summary: summary, TraceID: traceID, Details: details, FiredAt: now}
	}
	switch r.Kind {
	case KindErrorRate:
		o, err := e.Detect.Overview(ctx, tenant, from, now, window)
		if err != nil {
			return nil, err
		}
		if o.Runs == 0 || int(o.Runs) < r.MinRuns {
			return nil, nil
		}
		rate := float64(o.FailedRuns) / float64(o.Runs)
		if rate < r.Threshold {
			return nil, nil
		}
		// At most once per window.
		slot := now.Truncate(window).Unix()
		return []Event{ev(fmt.Sprintf("rate:%d", slot),
			fmt.Sprintf("Run failure rate %.0f%% over the last %d minutes (%d of %d runs failed; threshold %.0f%%)",
				rate*100, r.WindowMinutes, o.FailedRuns, o.Runs, r.Threshold*100), "",
			map[string]any{"failure_rate": rate, "runs": o.Runs, "failed_runs": o.FailedRuns})}, nil
	case KindRunawayLoop:
		loops, err := e.Detect.Loops(ctx, tenant, from, now, int(r.Threshold))
		if err != nil {
			return nil, err
		}
		var out []Event
		for _, l := range loops {
			if len(out) >= limit {
				break
			}
			out = append(out, ev("loop:"+l.TraceID+":"+l.ToolName,
				fmt.Sprintf("Runaway loop: %s called %d times with the same input in one run", l.ToolName, l.Repeats), l.TraceID,
				map[string]any{"tool_name": l.ToolName, "repeats": l.Repeats}))
		}
		return out, nil
	case KindRunCost:
		runs, err := e.Detect.CostlyRuns(ctx, tenant, from, now, r.Threshold, limit)
		if err != nil {
			return nil, err
		}
		var out []Event
		for _, run := range runs {
			out = append(out, ev("cost:"+run.TraceID,
				fmt.Sprintf("Run cost $%.2f (threshold $%.2f) for %s", run.CostUSD, r.Threshold, orDash(run.Agent)), run.TraceID,
				map[string]any{"cost_usd": run.CostUSD, "agent": run.Agent, "cost_partial": run.CostUnknownSteps > 0}))
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown rule kind %q", r.Kind)
}

func orDash(s string) string {
	if s == "" {
		return "an unnamed agent"
	}
	return s
}
