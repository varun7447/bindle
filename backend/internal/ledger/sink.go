package ledger

import (
	"context"
	"log/slog"

	"github.com/bindle-ai/bindle/backend/internal/model"
)

// Feature is the per-workspace flag that turns the ledger on.
const Feature = "ledger"

// StepWriter is the store the ledger wraps (search.Writer, pipeline sinks).
type StepWriter interface {
	Write(ctx context.Context, steps []model.Step) error
}

// FeatureChecker reports whether a tenant has a feature turned on.
type FeatureChecker interface {
	Enabled(ctx context.Context, tenant, feature string) bool
}

// Sink writes steps to Inner, then appends them to the ledger for tenants
// that have the ledger turned on. A ledger failure fails the whole write, so
// the batch is retried (Inner's writes are idempotent, and so are the
// ledger's): the ledger never silently misses a step that was stored.
type Sink struct {
	Inner    StepWriter
	Ledger   *Writer
	Features FeatureChecker
	Log      *slog.Logger
}

// Write implements pipeline.Sink and stream.Writer.
func (s *Sink) Write(ctx context.Context, steps []model.Step) error {
	if err := s.Inner.Write(ctx, steps); err != nil {
		return err
	}
	type group struct{ tenant, agent string }
	groups := map[group][]Entry{}
	var order []group
	enabled := map[string]bool{}
	for _, st := range steps {
		on, ok := enabled[st.TenantID]
		if !ok {
			on = st.TenantID != "" && s.Features != nil && s.Features.Enabled(ctx, st.TenantID, Feature)
			enabled[st.TenantID] = on
		}
		if !on {
			continue
		}
		g := group{st.TenantID, st.AgentName}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], FromStep(st))
		for _, reason := range st.PassportDrift {
			groups[g] = append(groups[g], DriftFromStep(st, reason))
		}
	}
	for _, g := range order {
		if _, err := s.Ledger.Append(ctx, g.tenant, g.agent, groups[g]); err != nil {
			if s.Log != nil {
				s.Log.Warn("ledger append failed; the batch will be retried", "tenant", g.tenant, "entries", len(groups[g]), "err", err)
			}
			return err
		}
	}
	return nil
}
