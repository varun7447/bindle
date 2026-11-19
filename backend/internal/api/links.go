package api

import (
	"net/http"
	"strings"

	"github.com/bindle-ai/bindle/backend/internal/query"
)

// LinkJSON is one linked run in GET /api/traces/{id}/links.
type LinkJSON struct {
	query.TraceLink
	Run *RunJSON `json:"run"` // null when the other run isn't stored
}

// traceLinks answers GET /api/traces/{id}/links: runs this run continues,
// takes over from or serves (outgoing), and runs that continue it, take
// over from it or serve it (incoming). Links come from OpenTelemetry span
// links, set by bindle.resume(), the MCP helpers or any OTel SDK.
func (s *Server) traceLinks(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	if !traceIDRe.MatchString(id) {
		badRequest(w, "trace id must be 32 hex characters")
		return
	}
	ls, ok := s.Store.(query.LinkStore)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"links": []LinkJSON{}})
		return
	}
	links, err := ls.TraceLinks(r.Context(), tenantFrom(r.Context()), id, 50)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	out := make([]LinkJSON, 0, len(links))
	for _, l := range links {
		j := LinkJSON{TraceLink: l}
		if l.Run != nil {
			rj := runJSON(*l.Run)
			j.Run = &rj
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out})
}
