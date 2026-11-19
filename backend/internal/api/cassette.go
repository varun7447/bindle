package api

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/query"
)

// CassetteVersion is the format of GET /api/traces/{id}/cassette. The SDKs
// (bindle.Cassette) read it to re-run an agent on a recorded run's input
// with every tool call answered from the recording.
const CassetteVersion = 1

// maxCassetteTools caps the tool calls in one cassette.
const maxCassetteTools = 500

// CassetteCall is one recorded tool call, in the order the run made them.
type CassetteCall struct {
	StepID    string `json:"step_id"`
	Tool      string `json:"tool"`
	Arguments string `json:"arguments"` // as captured (usually JSON text)
	Result    string `json:"result"`
	Status    string `json:"status"` // success, failure or unset
	Error     string `json:"error,omitempty"`
}

// Cassette is a recorded run for replay with recorded tool outputs.
type Cassette struct {
	Format       string         `json:"format"` // "bindle.cassette"
	Version      int            `json:"version"`
	TraceID      string         `json:"trace_id"`
	Agent        string         `json:"agent"`
	AgentVersion string         `json:"agent_version"`
	RecordedAt   string         `json:"recorded_at"`
	Input        string         `json:"input"`
	Output       string         `json:"output"`
	ToolCalls    []CassetteCall `json:"tool_calls"`
	// Incomplete lists why the cassette may not cover the whole run:
	// "steps_truncated", "tool_calls_capped", "content_missing",
	// "content_truncated".
	Incomplete []string `json:"incomplete"`
}

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
	tenant := tenantFrom(r.Context())
	maxSteps := s.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 5000
	}
	tr, err := s.Store.GetTrace(r.Context(), tenant, id, maxSteps)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if tr == nil {
		writeError(w, http.StatusNotFound, "not_found", "trace not found")
		return
	}
	c := Cassette{Format: "bindle.cassette", Version: CassetteVersion, TraceID: id, Agent: tr.Run.Agent,
		AgentVersion: tr.Run.AgentVersion, RecordedAt: tr.Run.Started.Format(time.RFC3339Nano),
		ToolCalls: []CassetteCall{}, Incomplete: []string{}}
	if tr.Truncated {
		c.Incomplete = append(c.Incomplete, "steps_truncated")
	}
	if runs, err := s.Store.RunsForEval(r.Context(), tenant, []string{id}); err != nil {
		s.storeError(w, r, err)
		return
	} else if len(runs) > 0 {
		c.Input, c.Output = runs[0].Input, runs[0].Output
	}

	var tools []query.StepSummary
	for _, st := range tr.Steps { // GetTrace returns steps in start order
		if st.Type == "tool_call" {
			tools = append(tools, st)
		}
	}
	if len(tools) > maxCassetteTools {
		tools = tools[:maxCassetteTools]
		c.Incomplete = append(c.Incomplete, "tool_calls_capped")
	}
	calls := make([]CassetteCall, len(tools))
	details := make([]*query.StepDetail, len(tools))
	errs := make([]error, len(tools))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range tools {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			details[i], errs[i] = s.Store.GetStep(r.Context(), tenant, id, tools[i].StepID)
		}(i)
	}
	wg.Wait()
	missing, partial := false, false
	for i, st := range tools {
		if errs[i] != nil {
			s.storeError(w, r, errs[i])
			return
		}
		call := CassetteCall{StepID: st.StepID, Tool: st.ToolName, Status: st.Status, Error: st.ErrorMessage}
		if call.Tool == "" {
			call.Tool = st.Name
		}
		if d := details[i]; d != nil {
			call.Arguments, call.Result = d.Input, d.Output
			if len(d.TruncatedFields) > 0 {
				partial = true
			}
		}
		if call.Arguments == "" && call.Result == "" {
			missing = true
		}
		calls[i] = call
	}
	c.ToolCalls = calls
	if missing {
		c.Incomplete = append(c.Incomplete, "content_missing")
	}
	if partial {
		c.Incomplete = append(c.Incomplete, "content_truncated")
	}
	w.Header().Set("Content-Disposition", `attachment; filename="run-`+id+`.cassette.json"`)
	writeJSON(w, http.StatusOK, c)
}
