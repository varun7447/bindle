package api

import (
	"context"
	"net/http"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/sdkconfig"
)

// RunStopStore records runs stopped from the console. Implemented by
// *control.Store.
type RunStopStore interface {
	StopRun(ctx context.Context, tenant, userID, traceID string) (control.RunStop, error)
	GetRunStop(ctx context.Context, tenant, traceID string) (control.RunStop, bool, error)
}

type runStopJSON struct {
	Stopped   bool   `json:"stopped"`
	StoppedAt string `json:"stopped_at,omitempty"`
	// PollSeconds is how often the SDK refreshes its config, so how long a
	// stop can take to reach a running agent.
	PollSeconds int `json:"poll_seconds"`
}

func stopJSON(st control.RunStop, ok bool) runStopJSON {
	out := runStopJSON{Stopped: ok, PollSeconds: sdkconfig.PollSeconds}
	if ok {
		out.StoppedAt = st.StoppedAt.Format("2006-01-02T15:04:05Z")
	}
	return out
}

// GET /api/traces/{id}/stop: whether the run was stopped from the console.
func (s *Server) getRunStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !traceIDRe.MatchString(id) {
		badRequest(w, "trace id must be 32 lowercase hex characters")
		return
	}
	if s.Stops == nil {
		writeJSON(w, http.StatusOK, runStopJSON{PollSeconds: sdkconfig.PollSeconds})
		return
	}
	st, ok, err := s.Stops.GetRunStop(r.Context(), tenantFrom(r.Context()), id)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stopJSON(st, ok))
}

// POST /api/traces/{id}/stop: ask the agent to stop this run. It takes
// effect where the agent calls bindle.check_limits(), after its next config
// poll; Bindle never stops an agent anywhere else.
func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !traceIDRe.MatchString(id) {
		badRequest(w, "trace id must be 32 lowercase hex characters")
		return
	}
	if s.Stops == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "remote config is not configured")
		return
	}
	p := principalFrom(r.Context())
	if !p.CanManageKeys() {
		writeError(w, http.StatusForbidden, "forbidden", "only owners and admins can stop runs")
		return
	}
	st, err := s.Stops.StopRun(r.Context(), p.TenantID, p.UserID, id)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stopJSON(st, true))
}
