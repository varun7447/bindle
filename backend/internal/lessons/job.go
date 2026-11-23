package lessons

import (
	"context"
	"log/slog"
	"time"
)

// Tenant is a workspace to compute lessons for.
type Tenant struct {
	ID      string
	MinRuns int
	Capture bool
}

// Job recomputes lessons for every workspace with the learning loop on.
// The alerts worker runs it hourly.
type Job struct {
	Tenants func(ctx context.Context) ([]Tenant, error)
	Groups  func(ctx context.Context, tenant string, from, to time.Time) ([]Group, error)
	Tools   func(ctx context.Context, tenant string, from, to time.Time) (map[string][]string, error)
	Replace func(ctx context.Context, tenant string, ls []Lesson, tools map[string][]string) (int, error)
	Log     *slog.Logger
	Now     func() time.Time
}

// RunOnce computes and stores every workspace's lessons. A failing
// workspace is logged and skipped; the others still run.
func (j *Job) RunOnce(ctx context.Context) (workspaces, stored int) {
	ts, err := j.Tenants(ctx)
	if err != nil {
		j.Log.Warn("lessons: tenants", "err", err)
		return 0, 0
	}
	now := time.Now()
	if j.Now != nil {
		now = j.Now()
	}
	from := now.Add(-Window)
	for _, t := range ts {
		groups, err := j.Groups(ctx, t.ID, from, now)
		if err != nil {
			j.Log.Warn("lessons: failure groups", "tenant", t.ID, "err", err)
			continue
		}
		tools, err := j.Tools(ctx, t.ID, from, now)
		if err != nil {
			j.Log.Warn("lessons: agent tools", "tenant", t.ID, "err", err)
			continue
		}
		n, err := j.Replace(ctx, t.ID, Build(groups, t.MinRuns, t.Capture), tools)
		if err != nil {
			j.Log.Warn("lessons: store", "tenant", t.ID, "err", err)
			continue
		}
		workspaces++
		stored += n
	}
	return workspaces, stored
}

// Run runs the job now and then every interval until ctx ends.
func (j *Job) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		ws, n := j.RunOnce(rctx)
		cancel()
		if ws > 0 {
			j.Log.Info("lessons recomputed", "workspaces", ws, "lessons", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
