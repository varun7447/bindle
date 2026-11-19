package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/evals"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

// EvalStore is the control-plane side of evals (control.Store).
type EvalStore interface {
	ListScorers(ctx context.Context, tenant string) ([]evals.Scorer, error)
	CreateScorer(ctx context.Context, tenant, userID string, s evals.Scorer) (evals.Scorer, error)
	UpdateScorer(ctx context.Context, tenant, id string, s evals.Scorer) (evals.Scorer, error)
	DeleteScorer(ctx context.Context, tenant, id string) error
	CreateDataset(ctx context.Context, tenant, userID, name, description string) (control.Dataset, error)
	ListDatasets(ctx context.Context, tenant string) ([]control.Dataset, error)
	GetDataset(ctx context.Context, tenant, id string) (control.Dataset, []evals.Run, error)
	AddDatasetItems(ctx context.Context, tenant, id string, runs []evals.Run) (int, error)
	RemoveDatasetItem(ctx context.Context, tenant, id, traceID string) error
	DeleteDataset(ctx context.Context, tenant, id string) error
	RecordEvalRun(ctx context.Context, tenant, userID, datasetID string, items int, tallies []evals.Tally, failures []control.Failure) (control.EvalRun, error)
	ListEvalRuns(ctx context.Context, tenant, datasetID string) ([]control.EvalRun, error)
}

var (
	scorerIDRe  = regexp.MustCompile(`^sc_[0-9a-f]{20}$`)
	datasetIDRe = regexp.MustCompile(`^ds_[0-9a-f]{20}$`)
)

const (
	maxEvalBodyBytes = 16 << 10
	maxAddPerRequest = 100
	previewChars     = 600
	checkRunsPerVer  = 100
	checkVersions    = 5
)

func (s *Server) evalRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/scorers", s.listScorers)
	api.HandleFunc("POST /api/scorers", s.createScorer)
	api.HandleFunc("PUT /api/scorers/{id}", s.updateScorer)
	api.HandleFunc("DELETE /api/scorers/{id}", s.deleteScorer)
	api.HandleFunc("GET /api/datasets", s.listDatasets)
	api.HandleFunc("POST /api/datasets", s.createDataset)
	api.HandleFunc("GET /api/datasets/{id}", s.getDataset)
	api.HandleFunc("DELETE /api/datasets/{id}", s.deleteDataset)
	api.HandleFunc("POST /api/datasets/{id}/items", s.addDatasetItems)
	api.HandleFunc("POST /api/datasets/{id}/items/from-problems", s.addProblemItems)
	api.HandleFunc("GET /api/problems/suggestions", s.suggestChecks)
	api.HandleFunc("DELETE /api/datasets/{id}/items/{trace}", s.removeDatasetItem)
	api.HandleFunc("POST /api/datasets/{id}/evaluate", s.evaluateDataset)
	api.HandleFunc("GET /api/datasets/{id}/evals", s.listEvalRuns)
	api.HandleFunc("GET /api/datasets/{id}/export", s.exportDataset)
	api.HandleFunc("GET /api/agents/checks", s.agentChecks)
}

func (s *Server) evalsOn(w http.ResponseWriter) bool {
	if s.Evals == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "evals are not configured")
		return false
	}
	return true
}

func (s *Server) evalError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case evals.IsInvalid(err):
		badRequest(w, err.Error())
	case errors.Is(err, control.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, control.ErrDuplicate):
		writeError(w, http.StatusConflict, "conflict", "that name is already used")
	default:
		s.storeError(w, r, err)
	}
}

// ---- checks ----------------------------------------------------------------

type scorerBody struct {
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	Agent         string  `json:"agent"`
	Value         string  `json:"value"`
	Number        float64 `json:"number"`
	CaseSensitive bool    `json:"case_sensitive"`
	Enabled       *bool   `json:"enabled"`
}

func (b scorerBody) scorer() evals.Scorer {
	enabled := b.Enabled == nil || *b.Enabled
	return evals.Scorer{Name: strings.TrimSpace(b.Name), Kind: b.Kind, Agent: strings.TrimSpace(b.Agent), Value: b.Value, Number: b.Number, CaseSensitive: b.CaseSensitive, Enabled: enabled}
}

func (s *Server) listScorers(w http.ResponseWriter, r *http.Request) {
	if !s.evalsOn(w) {
		return
	}
	out, err := s.Evals.ListScorers(r.Context(), tenantFrom(r.Context()))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scorers": out})
}

func (s *Server) scorerWrite(w http.ResponseWriter, r *http.Request) (evals.Scorer, bool) {
	if !s.evalsOn(w) {
		return evals.Scorer{}, false
	}
	if !principalFrom(r.Context()).CanManageKeys() {
		writeError(w, http.StatusForbidden, "forbidden", "only owners and admins can change checks")
		return evals.Scorer{}, false
	}
	if r.Method == http.MethodDelete {
		return evals.Scorer{}, true
	}
	var b scorerBody
	if !decodeBodyLimit(w, r, &b, maxEvalBodyBytes) {
		return evals.Scorer{}, false
	}
	return b.scorer(), true
}

func (s *Server) createScorer(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.scorerWrite(w, r)
	if !ok {
		return
	}
	if err := evals.Validate(sc); err != nil {
		badRequest(w, err.Error())
		return
	}
	p := principalFrom(r.Context())
	out, err := s.Evals.CreateScorer(r.Context(), p.TenantID, p.UserID, sc)
	if err != nil {
		s.evalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) updateScorer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !scorerIDRe.MatchString(id) {
		badRequest(w, "invalid check ID")
		return
	}
	sc, ok := s.scorerWrite(w, r)
	if !ok {
		return
	}
	// The stored kind is kept; the store validates against it.
	out, err := s.Evals.UpdateScorer(r.Context(), tenantFrom(r.Context()), id, sc)
	if err != nil {
		s.evalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteScorer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !scorerIDRe.MatchString(id) {
		badRequest(w, "invalid check ID")
		return
	}
	if _, ok := s.scorerWrite(w, r); !ok {
		return
	}
	if err := s.Evals.DeleteScorer(r.Context(), tenantFrom(r.Context()), id); err != nil {
		s.evalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- datasets --------------------------------------------------------------

func (s *Server) datasetID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.evalsOn(w) {
		return "", false
	}
	id := r.PathValue("id")
	if !datasetIDRe.MatchString(id) {
		badRequest(w, "invalid dataset ID")
		return "", false
	}
	return id, true
}

func (s *Server) listDatasets(w http.ResponseWriter, r *http.Request) {
	if !s.evalsOn(w) {
		return
	}
	out, err := s.Evals.ListDatasets(r.Context(), tenantFrom(r.Context()))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"datasets": out})
}

func (s *Server) createDataset(w http.ResponseWriter, r *http.Request) {
	if !s.evalsOn(w) {
		return
	}
	var b struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decodeBodyLimit(w, r, &b, maxEvalBodyBytes) {
		return
	}
	p := principalFrom(r.Context())
	out, err := s.Evals.CreateDataset(r.Context(), p.TenantID, p.UserID, b.Name, b.Description)
	if err != nil {
		s.evalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func preview(s string) string {
	if len(s) <= previewChars {
		return s
	}
	cut := previewChars
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

func (s *Server) getDataset(w http.ResponseWriter, r *http.Request) {
	id, ok := s.datasetID(w, r)
	if !ok {
		return
	}
	d, items, err := s.Evals.GetDataset(r.Context(), tenantFrom(r.Context()), id)
	if err != nil {
		s.evalError(w, r, err)
		return
	}
	for i := range items {
		items[i].Input, items[i].Output = preview(items[i].Input), preview(items[i].Output)
	}
	writeJSON(w, http.StatusOK, map[string]any{"dataset": d, "items": items})
}

func (s *Server) deleteDataset(w http.ResponseWriter, r *http.Request) {
	id, ok := s.datasetID(w, r)
	if !ok {
		return
	}
	if !principalFrom(r.Context()).CanManageKeys() {
		writeError(w, http.StatusForbidden, "forbidden", "only owners and admins can delete datasets")
		return
	}
	if err := s.Evals.DeleteDataset(r.Context(), tenantFrom(r.Context()), id); err != nil {
		s.evalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) addDatasetItems(w http.ResponseWriter, r *http.Request) {
	id, ok := s.datasetID(w, r)
	if !ok {
		return
	}
	var b struct {
		TraceIDs []string `json:"trace_ids"`
	}
	if !decodeBodyLimit(w, r, &b, maxEvalBodyBytes) {
		return
	}
	if len(b.TraceIDs) == 0 || len(b.TraceIDs) > maxAddPerRequest {
		badRequest(w, "trace_ids must list 1 to 100 runs")
		return
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(b.TraceIDs))
	for _, t := range b.TraceIDs {
		t = strings.ToLower(t)
		if !traceIDRe.MatchString(t) {
			badRequest(w, "invalid trace ID")
			return
		}
		if !seen[t] {
			seen[t] = true
			ids = append(ids, t)
		}
	}
	tenant := tenantFrom(r.Context())
	runs, err := s.Store.RunsForEval(r.Context(), tenant, ids)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	added, err := s.Evals.AddDatasetItems(r.Context(), tenant, id, runs)
	if err != nil {
		s.evalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "found": len(runs), "requested": len(ids)})
}

func (s *Server) removeDatasetItem(w http.ResponseWriter, r *http.Request) {
	id, ok := s.datasetID(w, r)
	if !ok {
		return
	}
	trace := strings.ToLower(r.PathValue("trace"))
	if !traceIDRe.MatchString(trace) {
		badRequest(w, "invalid trace ID")
		return
	}
	if err := s.Evals.RemoveDatasetItem(r.Context(), tenantFrom(r.Context()), id, trace); err != nil {
		s.evalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// grade runs every enabled check that applies to each run.
func grade(scorers []evals.Scorer, runs []evals.Run) ([]evals.Tally, []control.Failure) {
	tallies := make([]evals.Tally, 0, len(scorers))
	var failures []control.Failure
	for _, sc := range scorers {
		if !sc.Enabled {
			continue
		}
		t := evals.Tally{ScorerID: sc.ID, Name: sc.Name}
		for _, run := range runs {
			if !sc.Applies(run.Agent) {
				continue
			}
			res := evals.Evaluate(sc, run)
			t.Add(res)
			if !res.Pass && !res.Skipped {
				failures = append(failures, control.Failure{TraceID: run.TraceID, ScorerID: sc.ID, Scorer: sc.Name, Detail: res.Detail})
			}
		}
		tallies = append(tallies, t)
	}
	return tallies, failures
}

func (s *Server) evaluateDataset(w http.ResponseWriter, r *http.Request) {
	id, ok := s.datasetID(w, r)
	if !ok {
		return
	}
	p := principalFrom(r.Context())
	_, items, err := s.Evals.GetDataset(r.Context(), p.TenantID, id)
	if err != nil {
		s.evalError(w, r, err)
		return
	}
	scorers, err := s.Evals.ListScorers(r.Context(), p.TenantID)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	tallies, failures := grade(scorers, items)
	out, err := s.Evals.RecordEvalRun(r.Context(), p.TenantID, p.UserID, id, len(items), tallies, failures)
	if err != nil {
		s.evalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listEvalRuns(w http.ResponseWriter, r *http.Request) {
	id, ok := s.datasetID(w, r)
	if !ok {
		return
	}
	out, err := s.Evals.ListEvalRuns(r.Context(), tenantFrom(r.Context()), id)
	if err != nil {
		s.evalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"evals": out})
}

// exportDataset streams the dataset as JSON Lines, for model-graded evals
// run in the customer's own environment (results come back through
// bindle.score with each line's trace_id).
func (s *Server) exportDataset(w http.ResponseWriter, r *http.Request) {
	id, ok := s.datasetID(w, r)
	if !ok {
		return
	}
	d, items, err := s.Evals.GetDataset(r.Context(), tenantFrom(r.Context()), id)
	if err != nil {
		s.evalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="`+safeFilename(d.Name)+`.jsonl"`)
	enc := json.NewEncoder(w)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			return
		}
	}
}

var unsafeFile = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeFilename(name string) string {
	f := strings.Trim(unsafeFile.ReplaceAllString(name, "-"), "-.")
	if f == "" {
		return "dataset"
	}
	if len(f) > 60 {
		f = f[:60]
	}
	return f
}

// ---- checks per agent version (live traffic) -------------------------------

type versionChecks struct {
	Version     string        `json:"version"`
	RunsChecked int           `json:"runs_checked"`
	Checks      []evals.Tally `json:"checks"`
}

// agentChecks grades a sample of each version's latest runs (up to 100 per
// version, 5 most recent versions) with the enabled checks, and adds
// bindle.score() results.
func (s *Server) agentChecks(w http.ResponseWriter, r *http.Request) {
	if !s.evalsOn(w) {
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
	tenant := tenantFrom(r.Context())
	scorers, err := s.Evals.ListScorers(r.Context(), tenant)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	var applicable []evals.Scorer
	for _, sc := range scorers {
		if sc.Enabled && sc.Applies(agent) {
			applicable = append(applicable, sc)
		}
	}
	versions, err := s.Store.AgentVersions(r.Context(), tenant, agent, from, to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if len(versions) > checkVersions {
		versions = versions[:checkVersions]
	}
	out := []versionChecks{}
	if len(applicable) > 0 {
		for _, v := range versions {
			runs, err := s.Store.ListRuns(r.Context(), tenant, query.RunFilter{From: from, To: to, Agent: agent, Version: v.Version, Limit: checkRunsPerVer})
			if err != nil {
				s.storeError(w, r, err)
				return
			}
			ids := make([]string, 0, len(runs))
			for _, run := range runs {
				ids = append(ids, run.TraceID)
			}
			er, err := s.Store.RunsForEval(r.Context(), tenant, ids)
			if err != nil {
				s.storeError(w, r, err)
				return
			}
			tallies, _ := grade(applicable, er)
			out = append(out, versionChecks{Version: v.Version, RunsChecked: len(er), Checks: tallies})
		}
	}
	scores, err := s.Store.Scores(r.Context(), tenant, agent, from, to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": name, "agent": agent, "versions": out, "scores": scores})
}
