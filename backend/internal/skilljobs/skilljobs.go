// Package skilljobs are the alerts worker's learning-loop jobs for phase 2:
// recording each lesson's evidence runs as cassettes (so bindle skills
// prove can replay them in the customer's environment), and measuring each
// shipped skill release, rolling a clearly worse one back.
package skilljobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/cassette"
	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/ledger"
	"github.com/bindle-ai/bindle/backend/internal/outcomes"
	"github.com/bindle-ai/bindle/backend/internal/search"
	"github.com/bindle-ai/bindle/backend/internal/skills"
	"github.com/bindle-ai/bindle/backend/internal/skillship"
)

// ---- cassettes --------------------------------------------------------------------

// Cassettes fills in missing cassettes for a workspace's lessons.
type Cassettes struct {
	Missing func(ctx context.Context, tenant string, n int) ([]control.LessonTrace, error)
	Reader  cassette.Reader
	Put     func(ctx context.Context, tenant, lesson, trace string, cassette []byte) error
	Log     *slog.Logger
}

// Fill records up to skillship.MaxCassettes evidence runs per lesson. A run
// no longer stored is skipped; a too-large one is stored with tool results
// clipped and marked "content_truncated".
func (c *Cassettes) Fill(ctx context.Context, tenant string) int {
	missing, err := c.Missing(ctx, tenant, skillship.MaxCassettes)
	if err != nil {
		c.Log.Warn("skills: cassettes to build", "tenant", tenant, "err", err)
		return 0
	}
	n := 0
	for _, m := range missing {
		cs, err := cassette.Build(ctx, c.Reader, tenant, m.TraceID, 0)
		if err != nil {
			c.Log.Warn("skills: build cassette", "tenant", tenant, "trace", m.TraceID, "err", err)
			continue
		}
		if cs == nil {
			continue // past retention
		}
		b, ok := fit(cs)
		if !ok {
			continue
		}
		if err := c.Put(ctx, tenant, m.Lesson, m.TraceID, b); err != nil {
			c.Log.Warn("skills: store cassette", "tenant", tenant, "err", err)
			continue
		}
		n++
	}
	return n
}

// fit encodes a cassette within skillship.MaxCassetteBytes, clipping
// recorded arguments and results when it has to.
func fit(cs *cassette.Cassette) ([]byte, bool) {
	b, err := json.Marshal(cs)
	if err != nil {
		return nil, false
	}
	if len(b) <= skillship.MaxCassetteBytes {
		return b, true
	}
	for _, limit := range []int{16 << 10, 4 << 10, 1 << 10} {
		for i := range cs.ToolCalls {
			cs.ToolCalls[i].Arguments = clip(cs.ToolCalls[i].Arguments, limit)
			cs.ToolCalls[i].Result = clip(cs.ToolCalls[i].Result, limit)
		}
		cs.Input, cs.Output = clip(cs.Input, limit*4), clip(cs.Output, limit*4)
		if !contains(cs.Incomplete, "content_truncated") {
			cs.Incomplete = append(cs.Incomplete, "content_truncated")
		}
		if b, err = json.Marshal(cs); err == nil && len(b) <= skillship.MaxCassetteBytes {
			return b, true
		}
	}
	return nil, false
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ---- measuring and rolling back ----------------------------------------------------

// Shipping is what the measurement job reads and writes in the control plane.
type Shipping interface {
	Releases(ctx context.Context) ([]control.ShippingRelease, error)
	Lessons(ctx context.Context, tenant string) (map[string]control.LessonBrief, error)
	PutMeasurement(ctx context.Context, tenant string, m skillship.Measurement) error
	Rollback(ctx context.Context, tenant, agent, path string, release int, reason string) (bool, error)
	GoLive(ctx context.Context, tenant, agent, path, sha string) (int, error)
}

// RunFacts reads runs from OpenSearch (search.Store.SkillRunFacts).
type RunFacts func(ctx context.Context, tenant, agent, path string, release int, lesson search.SkillLesson,
	from, to time.Time, extra map[string][]outcomes.OutcomeFact, settings outcomes.Settings) ([]skillship.RunFact, error)

// Measure measures every shipped release and acts on the result.
type Measure struct {
	Store    Shipping
	Facts    RunFacts
	Matched  func(ctx context.Context, tenant string, since time.Time) (map[string][]outcomes.OutcomeFact, error)
	Settings func(ctx context.Context, tenant string) (outcomes.Settings, error)
	// Ledger, when set and on for the tenant, records rollbacks and go-lives.
	Ledger   *ledger.Writer
	LedgerOn func(ctx context.Context, tenant string) bool
	Log      *slog.Logger
	Now      func() time.Time
}

// Result counts what one pass did.
type Result struct{ Measured, RolledBack, Live int }

// RunOnce measures each release agents are getting: runs that loaded it
// against the agent's runs without the skill (from a week before it
// shipped), graded like outcome grading. A clearly worse release rolls
// back (skillship's rule), which serves the previous release again, opens
// a Home issue and writes a ledger entry. An approved skill that agents
// started loading is marked live.
func (j *Measure) RunOnce(ctx context.Context) Result {
	var res Result
	rs, err := j.Store.Releases(ctx)
	if err != nil {
		j.Log.Warn("skills: releases", "err", err)
		return res
	}
	now := time.Now()
	if j.Now != nil {
		now = j.Now()
	}
	type tenantData struct {
		lessons  map[string]control.LessonBrief
		matched  map[string][]outcomes.OutcomeFact
		settings outcomes.Settings
		ok       bool
	}
	cache := map[string]*tenantData{}
	for _, r := range rs {
		td := cache[r.Tenant]
		if td == nil {
			td = &tenantData{}
			cache[r.Tenant] = td
			var err error
			if td.lessons, err = j.Store.Lessons(ctx, r.Tenant); err != nil {
				j.Log.Warn("skills: lessons", "tenant", r.Tenant, "err", err)
				continue
			}
			from := now.Add(-skillship.MeasureWindow - skillship.MeasureLookback)
			if j.Matched != nil {
				if td.matched, err = j.Matched(ctx, r.Tenant, from); err != nil {
					j.Log.Warn("skills: matched outcomes", "tenant", r.Tenant, "err", err)
					continue
				}
			}
			td.settings = outcomes.DefaultSettings()
			if j.Settings != nil {
				if td.settings, err = j.Settings(ctx, r.Tenant); err != nil {
					j.Log.Warn("skills: outcome settings", "tenant", r.Tenant, "err", err)
					continue
				}
			}
			td.ok = true
		}
		if !td.ok {
			continue
		}
		var lesson search.SkillLesson
		if sk, err := skills.Parse(r.Body); err == nil {
			if l, ok := td.lessons[strings.TrimSpace(sk.Metadata["bindle-lesson"])]; ok {
				lesson = search.SkillLesson{StepName: l.StepName, Tool: l.Tool, Pattern: l.Pattern}
			}
		}
		from := r.ReleasedAt.Add(-skillship.MeasureLookback)
		if min := now.Add(-skillship.MeasureWindow - skillship.MeasureLookback); from.Before(min) {
			from = min
		}
		facts, err := j.Facts(ctx, r.Tenant, r.Agent, r.Path, r.Release.Release, lesson, from, now, td.matched, td.settings)
		if err != nil {
			j.Log.Warn("skills: run facts", "tenant", r.Tenant, "agent", r.Agent, "path", r.Path, "err", err)
			continue
		}
		m := skillship.Measure(r.Agent, r.Path, r.Release.Release, facts, now)
		if err := j.Store.PutMeasurement(ctx, r.Tenant, m); err != nil {
			j.Log.Warn("skills: store measurement", "tenant", r.Tenant, "err", err)
			continue
		}
		res.Measured++
		if m.Decision == "rollback" {
			done, err := j.Store.Rollback(ctx, r.Tenant, r.Agent, r.Path, r.Release.Release, m.Reason)
			if err != nil {
				j.Log.Warn("skills: rollback", "tenant", r.Tenant, "err", err)
				continue
			}
			if done {
				res.RolledBack++
				j.Log.Info("skill release rolled back", "tenant", r.Tenant, "agent", r.Agent, "path", r.Path, "release", r.Release.Release, "reason", m.Reason)
				j.record(ctx, r.Tenant, r.Agent, ledger.SkillChange(r.Agent, r.Path, r.Release.Release, r.SHA256, "rollback", m.Reason, now))
			}
			continue
		}
		if m.With.Runs > 0 && r.NoteStatus == skills.StatusApproved && r.NoteSHA == r.SHA256 {
			v, err := j.Store.GoLive(ctx, r.Tenant, r.Agent, r.Path, r.SHA256)
			if err != nil {
				j.Log.Warn("skills: go live", "tenant", r.Tenant, "err", err)
				continue
			}
			if v > 0 {
				res.Live++
				j.record(ctx, r.Tenant, r.Agent, ledger.SkillChange(r.Agent, r.Path, r.Release.Release, r.SHA256, "live",
					"agents started loading it", now))
			}
		}
	}
	return res
}

func (j *Measure) record(ctx context.Context, tenant, agent string, e ledger.Entry) {
	if j.Ledger == nil || j.LedgerOn == nil || !j.LedgerOn(ctx, tenant) {
		return
	}
	if _, err := j.Ledger.Append(ctx, tenant, agent, []ledger.Entry{e}); err != nil {
		j.Log.Warn("skills: ledger", "tenant", tenant, "err", err)
	}
}

// Run measures now and then every interval until ctx ends.
func (j *Measure) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		res := j.RunOnce(rctx)
		cancel()
		if res.Measured > 0 {
			j.Log.Info("skill releases measured", "measured", res.Measured, "rolled_back", res.RolledBack, "live", res.Live)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
