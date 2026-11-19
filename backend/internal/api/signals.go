package api

import (
	"context"
	"net/http"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/query"
	"github.com/bindle-ai/bindle/backend/internal/security"
)

// SignalStore is the optional part of the trace store behind security
// signals and topics (search.Store implements it).
type SignalStore interface {
	SecuritySignals(ctx context.Context, tenant string, from, to time.Time) ([]query.SecuritySignal, error)
	Topics(ctx context.Context, tenant, agent string, from, to time.Time) (*query.Topics, error)
}

func (s *Server) signals(w http.ResponseWriter) (SignalStore, bool) {
	ss, ok := s.Store.(SignalStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "security signals and topics are not available")
	}
	return ss, ok
}

// GET /api/security?window=24h|7d|30d: flagged steps per flag and agent,
// with what each reason means.
func (s *Server) securitySignals(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.signals(w)
	if !ok {
		return
	}
	name, from, to, ok := s.window(w, r, "7d")
	if !ok {
		return
	}
	sig, err := ss.SecuritySignals(r.Context(), tenantFrom(r.Context()), from, to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": name, "signals": sig, "explanations": security.Explain})
}

// GET /api/agents/topics?agent=&window=: what users asked about in the
// window compared with the period before (agent "" = every agent).
func (s *Server) agentTopics(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.signals(w)
	if !ok {
		return
	}
	agent := r.URL.Query().Get("agent")
	if len(agent) > maxFilterLen {
		badRequest(w, "agent is too long")
		return
	}
	name, from, to, ok := s.window(w, r, "7d")
	if !ok {
		return
	}
	t, err := ss.Topics(r.Context(), tenantFrom(r.Context()), agent, from, to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": name, "agent": agent, "topics": t})
}
