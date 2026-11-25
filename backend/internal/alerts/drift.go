package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/passport"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

// KindPassportDrift alerts when an agent goes outside its passport.
// threshold: drifting steps of one kind for one agent in the window. Rate
// limit and budget drift (which aren't per step) always count.
const KindPassportDrift = "passport_drift"

// DriftDetector is the optional part of the Detector behind passport drift
// (search.Store implements it).
type DriftDetector interface {
	PassportDrift(ctx context.Context, tenant string, from, to time.Time) ([]query.DriftSignal, error)
	ToolCalls(ctx context.Context, tenant, agent, pattern string, from, to time.Time) (uint64, string, error)
}

// PassportSource gives a tenant's active passports. Implemented by
// *control.PassportReader.
type PassportSource interface {
	Passports(ctx context.Context, tenant string) (passport.Set, error)
}

var driftNames = map[string]string{
	passport.ToolNotAllowed:      "calls to tools outside the passport",
	passport.DataClassNotAllowed: "data classes the passport doesn't allow",
	passport.EgressBlocked:       "blocked data sent to an outbound tool",
	passport.ReleaseNotAllowed:   "release actions without release rights",
}

// detectDrift implements the passport_drift rule.
func (e *Evaluator) detectDrift(ctx context.Context, tenant string, r Rule, now, from time.Time, limit int, ev eventFn) ([]Event, error) {
	dd, ok := e.Detect.(DriftDetector)
	if !ok || e.Passports == nil {
		return nil, fmt.Errorf("passport drift is not available")
	}
	window := time.Duration(r.WindowMinutes) * time.Minute
	slot := now.Truncate(window).Unix()
	sigs, err := dd.PassportDrift(ctx, tenant, from, now)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, s := range sigs {
		if len(out) >= limit {
			break
		}
		if float64(s.Steps) < r.Threshold {
			continue
		}
		trace := ""
		if len(s.Examples) > 0 {
			trace = s.Examples[0].TraceID
		}
		summary := fmt.Sprintf("%s: %d step%s with %s in the last %d minutes (%s)", orDash(s.Agent), s.Steps, plural(s.Steps),
			driftNames[s.Kind], r.WindowMinutes, topDetails(s.Reasons))
		out = append(out, ev(fmt.Sprintf("drift:%s:%s:%d", s.Kind, s.Agent, slot), summary, trace,
			map[string]any{"kind": s.Kind, "agent": s.Agent, "steps": s.Steps, "runs": s.Runs, "reasons": s.Reasons}))
	}
	set, err := e.Passports.Passports(ctx, tenant)
	if err != nil {
		return nil, err
	}
	counter := driftCounter{dd, e.Detect}
	for _, agent := range set.Agents() {
		findings, err := passport.Stateful(ctx, counter, tenant, set[agent], from, now)
		if err != nil {
			return nil, err
		}
		for _, f := range findings {
			if len(out) >= limit {
				break
			}
			out = append(out, ev(fmt.Sprintf("drift:%s:%s", f.Reason(), f.Slot(now)), f.Summary, f.TraceID,
				map[string]any{"kind": f.Kind, "agent": f.Agent, "detail": f.Detail, "value": f.Value, "limit": f.Limit}))
		}
	}
	return out, nil
}

func topDetails(reasons map[string]uint64) string {
	type kv struct {
		k string
		n uint64
	}
	var list []kv
	for k, n := range reasons {
		list = append(list, kv{k, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].k < list[j].k
	})
	s := ""
	for i, x := range list {
		if i == 3 {
			s += ", …"
			break
		}
		if i > 0 {
			s += ", "
		}
		_, detail, _ := strings.Cut(x.k, ":")
		s += fmt.Sprintf("%s ×%d", detail, x.n)
	}
	return s
}

type driftCounter struct {
	DriftDetector
	d Detector
}

func (c driftCounter) CostlyRuns(ctx context.Context, tenant string, from, to time.Time, minCostUSD float64, limit int) ([]query.Run, error) {
	return c.d.CostlyRuns(ctx, tenant, from, to, minCostUSD, limit)
}

func (c driftCounter) CostByAgent(ctx context.Context, tenant string, from, to time.Time) (map[string]float64, error) {
	return c.d.CostByAgent(ctx, tenant, from, to)
}

// DriftJournal writes rate-limit and budget drift to each agent's ledger
// (when the workspace has the ledger feature), once per window, run or
// day. Per-step drift is written by the pipeline with the step itself.
type DriftJournal struct {
	Tenants interface {
		Tenants(ctx context.Context) ([]string, error)
	}
	Passports PassportSource
	Counter   passport.Counter
	// Record writes findings to the agent's ledger (ledger.Writer.RecordFindings).
	Record   func(ctx context.Context, tenant, agent string, findings []passport.Finding, now time.Time) error
	Features interface {
		Enabled(ctx context.Context, tenant, feature string) bool
	}
	Lookback time.Duration // per-run budget: runs started this long ago; default 10m
	Log      *slog.Logger
	Now      func() time.Time
}

// RunOnce checks every tenant with passports.
func (j *DriftJournal) RunOnce(ctx context.Context) error {
	tenants, err := j.Tenants.Tenants(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if j.Now != nil {
		now = j.Now().UTC()
	}
	lookback := j.Lookback
	if lookback <= 0 {
		lookback = 10 * time.Minute
	}
	for _, t := range tenants {
		if j.Features == nil || !j.Features.Enabled(ctx, t, "ledger") {
			continue
		}
		set, err := j.Passports.Passports(ctx, t)
		if err != nil {
			j.Log.Warn("passport drift journal: passports unavailable", "tenant", t, "err", err)
			continue
		}
		for _, agent := range set.Agents() {
			findings, err := passport.Stateful(ctx, j.Counter, t, set[agent], now.Add(-lookback), now)
			if err != nil {
				j.Log.Warn("passport drift journal: check failed", "tenant", t, "agent", agent, "err", err)
				continue
			}
			if err := j.Record(ctx, t, agent, findings, now); err != nil {
				j.Log.Warn("passport drift journal: ledger append failed", "tenant", t, "agent", agent, "err", err)
			}
		}
	}
	return nil
}
