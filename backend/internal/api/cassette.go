package api

import (
	"net/http"
	"strings"

	"github.com/bindle-ai/bindle/backend/internal/cassette"
)

// Cassette is the format of GET /api/traces/{id}/cassette (package cassette).
type Cassette = cassette.Cassette

// GET /api/traces/{id}/cassette: the run's input, final output and every
// tool call with its arguments and recorded result, for bindle.Cassette.
// Redaction already happened at ingest, so the cassette holds only what
// Bindle stored.
func (s *Server) getCassette(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	if !traceIDRe.MatchString(id) {
		badRequest(w, "trace id must be 32 hex characters")
		return
	}
	c, err := cassette.Build(r.Context(), s.Store, tenantFrom(r.Context()), id, s.MaxSteps)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "not_found", "trace not found")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="run-`+id+`.cassette.json"`)
	writeJSON(w, http.StatusOK, c)
}
