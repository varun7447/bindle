package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/outcomes"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

// Outcome-graded quality (feature "outcome_grading"): workspace settings,
// signed webhook endpoints, the events reported after runs, and grading per
// agent version. Every route answers 404 unless the feature is on.

// OutcomeAdmin is the control-plane side (control.Store).
type OutcomeAdmin interface {
	GetOutcomeSettings(ctx context.Context, tenant string) (outcomes.Settings, error)
	PutOutcomeSettings(ctx context.Context, tenant, userID string, s outcomes.Settings) (outcomes.Settings, error)
	CreateOutcomeEndpoint(ctx context.Context, tenant, userID, name string) (control.OutcomeEndpoint, string, error)
	ListOutcomeEndpoints(ctx context.Context, tenant string) ([]control.OutcomeEndpoint, error)
	RevokeOutcomeEndpoint(ctx context.Context, tenant, id string) error
	ListOutcomeEvents(ctx context.Context, tenant, status string, limit int) ([]outcomes.Event, error)
	MatchedOutcomes(ctx context.Context, tenant string, since time.Time) (map[string][]outcomes.OutcomeFact, error)
}

// OutcomeGrader is the search side (search.Store), checked at runtime.
type OutcomeGrader interface {
	GradingFacts(ctx context.Context, tenant string, from, to time.Time) ([]outcomes.RunFacts, map[string][]outcomes.OutcomeFact, bool, error)
	EconomicsGraded(ctx context.Context, tenant string, from, to time.Time, extra map[string][]outcomes.OutcomeFact, s outcomes.Settings) ([]query.Economics, error)
}

var endpointIDParam = regexp.MustCompile(`^oep_[0-9a-f]{16}$`)

// outcomesOn checks the feature and that both stores are configured.
func (s *Server) outcomesOn(w http.ResponseWriter, r *http.Request) bool {
	if s.Outcomes == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return false
	}
	return s.featureOn(w, r, outcomes.Feature)
}

// outcomesEnabled is the silent check used by existing endpoints (economics,
// problem runs) to switch to outcome grading.
func (s *Server) outcomesEnabled(r *http.Request) (OutcomeGrader, bool) {
	return s.outcomesEnabledFor(r.Context(), tenantFrom(r.Context()))
}

func (s *Server) outcomesEnabledFor(ctx context.Context, tenant string) (OutcomeGrader, bool) {
	g, ok := s.Store.(OutcomeGrader)
	if !ok || s.Outcomes == nil || s.Features == nil {
		return nil, false
	}
	got, err := s.Features.List(ctx, tenant)
	if err != nil {
		return nil, false
	}
	for _, f := range got {
		if f == outcomes.Feature {
			return g, true
		}
	}
	return nil, false
}

func (s *Server) manageOutcomes(w http.ResponseWriter, r *http.Request) bool {
	if !principalFrom(r.Context()).CanManageKeys() {
		writeError(w, http.StatusForbidden, "forbidden", "only owners and admins can change outcome settings")
		return false
	}
	return true
}

func (s *Server) outcomeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, control.ErrInvalidSettings):
		badRequest(w, err.Error())
	case errors.Is(err, control.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	default:
		s.storeError(w, r, err)
	}
}

// GET /api/outcomes/settings
func (s *Server) getOutcomeSettings(w http.ResponseWriter, r *http.Request) {
	if !s.outcomesOn(w, r) {
		return
	}
	st, err := s.Outcomes.GetOutcomeSettings(r.Context(), tenantFrom(r.Context()))
	if err != nil {
		s.outcomeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// PUT /api/outcomes/settings {reversal_outcomes, pending_hours}
func (s *Server) putOutcomeSettings(w http.ResponseWriter, r *http.Request) {
	if !s.outcomesOn(w, r) || !s.manageOutcomes(w, r) {
		return
	}
	var in outcomes.Settings
	if !decodeBody(w, r, &in) {
		return
	}
	p := principalFrom(r.Context())
	out, err := s.Outcomes.PutOutcomeSettings(r.Context(), p.TenantID, p.UserID, in)
	if err != nil {
		s.outcomeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/outcomes/endpoints
func (s *Server) listOutcomeEndpoints(w http.ResponseWriter, r *http.Request) {
	if !s.outcomesOn(w, r) {
		return
	}
	eps, err := s.Outcomes.ListOutcomeEndpoints(r.Context(), tenantFrom(r.Context()))
	if err != nil {
		s.outcomeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": eps})
}

// POST /api/outcomes/endpoints {name}: the signing secret is in the
// response, once.
func (s *Server) createOutcomeEndpoint(w http.ResponseWriter, r *http.Request) {
	if !s.outcomesOn(w, r) || !s.manageOutcomes(w, r) {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	p := principalFrom(r.Context())
	ep, secret, err := s.Outcomes.CreateOutcomeEndpoint(r.Context(), p.TenantID, p.UserID, in.Name)
	if err != nil {
		s.outcomeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"endpoint": ep, "secret": secret,
		"path": "/v1/outcome-events/webhook/" + ep.ID, "signature_header": outcomes.SignatureHeader})
}

// POST /api/outcomes/endpoints/{id}/revoke
func (s *Server) revokeOutcomeEndpoint(w http.ResponseWriter, r *http.Request) {
	if !s.outcomesOn(w, r) || !s.manageOutcomes(w, r) {
		return
	}
	id := r.PathValue("id")
	if !endpointIDParam.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	if err := s.Outcomes.RevokeOutcomeEndpoint(r.Context(), tenantFrom(r.Context()), id); err != nil {
		s.outcomeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

// GET /api/outcomes/events?status=pending|matched|unmatched&limit=
func (s *Server) listOutcomeEvents(w http.ResponseWriter, r *http.Request) {
	if !s.outcomesOn(w, r) {
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", outcomes.StatusPending, outcomes.StatusMatched, outcomes.StatusUnmatched:
	default:
		badRequest(w, "status must be pending, matched or unmatched")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			badRequest(w, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	evs, err := s.Outcomes.ListOutcomeEvents(r.Context(), tenantFrom(r.Context()), status, limit)
	if err != nil {
		s.outcomeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}

// grades computes outcome-graded quality for the window.
func (s *Server) grades(ctx context.Context, g OutcomeGrader, tenant string, from, to time.Time) ([]outcomes.Grade, bool, error) {
	settings, err := s.Outcomes.GetOutcomeSettings(ctx, tenant)
	if err != nil {
		return nil, false, err
	}
	runs, inRun, truncated, err := g.GradingFacts(ctx, tenant, from, to)
	if err != nil {
		return nil, false, err
	}
	matched, err := s.Outcomes.MatchedOutcomes(ctx, tenant, from)
	if err != nil {
		return nil, false, err
	}
	for t, f := range matched {
		inRun[t] = append(inRun[t], f...)
	}
	return outcomes.GradeRuns(runs, inRun, settings), truncated, nil
}

// GET /api/agents/grading?window=: per agent version, the outcome-graded
// success rate, reversals and runs that passed evals but were reversed.
func (s *Server) agentGrading(w http.ResponseWriter, r *http.Request) {
	if !s.outcomesOn(w, r) {
		return
	}
	g, ok := s.Store.(OutcomeGrader)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	name, from, to, ok := s.window(w, r, "7d")
	if !ok {
		return
	}
	grades, truncated, err := s.grades(r.Context(), g, tenantFrom(r.Context()), from, to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if agent := r.URL.Query().Get("agent"); agent != "" {
		kept := grades[:0]
		for _, gr := range grades {
			if gr.Agent == agent {
				kept = append(kept, gr)
			}
		}
		grades = kept
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": name, "grades": grades, "truncated": truncated})
}

// economicsGraded is economics with matched events and reversals.
func (s *Server) economicsGraded(ctx context.Context, g OutcomeGrader, tenant string, from, to time.Time) ([]query.Economics, error) {
	settings, err := s.Outcomes.GetOutcomeSettings(ctx, tenant)
	if err != nil {
		return nil, err
	}
	matched, err := s.Outcomes.MatchedOutcomes(ctx, tenant, from)
	if err != nil {
		return nil, err
	}
	return g.EconomicsGraded(ctx, tenant, from, to, matched, settings)
}

// reversedRuns are the problem runs of source "reversed": runs that passed
// their evals but were later reversed, most recent first.
func (s *Server) reversedRuns(ctx context.Context, g OutcomeGrader, tenant string, f query.ProblemFilter) ([]string, error) {
	grades, _, err := s.grades(ctx, g, tenant, f.From, f.To)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, gr := range grades {
		if f.Agent != "" && gr.Agent != f.Agent {
			continue
		}
		for _, rr := range gr.ReversedRuns {
			ids = append(ids, rr.TraceID)
		}
	}
	if len(ids) > problemLookup {
		ids = ids[:problemLookup]
	}
	return ids, nil
}

// errReversedOff: source "reversed" without outcome grading.
var errReversedOff = errors.New("reversed runs need outcome grading, which is off for this workspace")

// problemError answers a problem-runs lookup failure.
func (s *Server) problemError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errReversedOff) {
		badRequest(w, err.Error())
		return
	}
	s.storeError(w, r, err)
}

// reversedLabel is the dataset item source for reversed runs.
const reversedLabel = "reversed"
