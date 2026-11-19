package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/bindle-ai/bindle/backend/internal/evals"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

// Problem runs (failure groups, negative feedback, failed scores) can be
// copied into a dataset in one step, with checks that would have caught
// them proposed for the user to confirm.

const (
	maxFromProblems = 50  // runs added per request
	problemLookup   = 200 // candidates looked up (before the agent filter)
	baselineRuns    = 100 // good runs compared against for suggestions
)

var problemKinds = map[string]bool{
	query.SourceFailure: true, query.SourceNegativeFeedback: true, query.SourceFailedScore: true, query.SourceFailedRuns: true,
}

// problemBody is a problem-run source, from a query string or a JSON body.
type problemBody struct {
	Kind     string `json:"kind"`
	StepName string `json:"step_name"`
	Pattern  string `json:"pattern"`
	Agent    string `json:"agent"`
	Score    string `json:"score"`
	Window   string `json:"window"`
}

func problemFromQuery(r *http.Request) problemBody {
	q := r.URL.Query()
	return problemBody{Kind: q.Get("kind"), StepName: q.Get("step_name"), Pattern: q.Get("pattern"),
		Agent: q.Get("agent"), Score: q.Get("score"), Window: q.Get("window")}
}

func cleanFilter(v string) (string, bool) {
	if len(v) > maxFilterLen || strings.IndexFunc(v, unicode.IsControl) >= 0 {
		return "", false
	}
	return v, true
}

// filter validates b into a ProblemFilter for the window ending at now.
func (b problemBody) filter(now time.Time) (query.ProblemFilter, string) {
	if !problemKinds[b.Kind] {
		return query.ProblemFilter{}, "kind must be failure, negative_feedback, failed_score or failed_runs"
	}
	if b.Window == "" {
		b.Window = "7d"
	}
	win, ok := overviewWindows[b.Window]
	if !ok {
		return query.ProblemFilter{}, "window must be 24h, 7d or 30d"
	}
	f := query.ProblemFilter{Kind: b.Kind, From: now.Add(-win.window), To: now, Limit: problemLookup}
	for _, p := range []struct {
		dst *string
		v   string
	}{{&f.StepName, b.StepName}, {&f.Pattern, b.Pattern}, {&f.Agent, b.Agent}, {&f.Score, b.Score}} {
		v, ok := cleanFilter(p.v)
		if !ok {
			return query.ProblemFilter{}, "filters must be at most 256 characters on one line"
		}
		*p.dst = v
	}
	if f.Kind == query.SourceFailure && f.StepName == "" && f.Pattern == "" {
		return query.ProblemFilter{}, "a failure source needs step_name or pattern"
	}
	return f, ""
}

// problemLabel says where items came from, as stored on each dataset item.
func problemLabel(f query.ProblemFilter) string {
	switch f.Kind {
	case query.SourceFailure:
		parts := []string{}
		for _, p := range []string{f.StepName, f.Pattern} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		return "failure: " + strings.Join(parts, ": ")
	case query.SourceNegativeFeedback:
		return "negative feedback"
	case query.SourceFailedScore:
		if f.Score != "" {
			return "failed score: " + f.Score
		}
		return "failed score"
	}
	if f.Agent != "" {
		return "failed runs of " + f.Agent
	}
	return "failed runs"
}

// problemRuns loads the problem runs for f, most recent first, filtered to
// f.Agent (feedback and scores sent later carry no agent of their own).
func (s *Server) problemRuns(ctx context.Context, tenant string, f query.ProblemFilter) ([]evals.Run, error) {
	ids, err := s.Store.ProblemRuns(ctx, tenant, f)
	if err != nil {
		return nil, err
	}
	runs, err := s.Store.RunsForEval(ctx, tenant, ids)
	if err != nil {
		return nil, err
	}
	out := make([]evals.Run, 0, min(len(runs), maxFromProblems))
	label := problemLabel(f)
	for _, r := range runs {
		if f.Agent != "" && r.Agent != f.Agent {
			continue
		}
		r.Source = label
		out = append(out, r)
		if len(out) == maxFromProblems {
			break
		}
	}
	return out, nil
}

// soleAgent is the agent all runs share, or "".
func soleAgent(runs []evals.Run) string {
	agent := ""
	for i, r := range runs {
		if i == 0 {
			agent = r.Agent
		} else if r.Agent != agent {
			return ""
		}
	}
	return agent
}

// suggestChecks handles GET /api/problems/suggestions: how many runs a
// source matches, and checks that would catch them (not saved).
func (s *Server) suggestChecks(w http.ResponseWriter, r *http.Request) {
	if !s.evalsOn(w) {
		return
	}
	f, msg := problemFromQuery(r).filter(s.now())
	if msg != "" {
		badRequest(w, msg)
		return
	}
	ctx, tenant := r.Context(), tenantFrom(r.Context())
	runs, err := s.problemRuns(ctx, tenant, f)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	agent := f.Agent
	if agent == "" {
		agent = soleAgent(runs)
	}
	in := evals.SuggestInput{Problem: runs, Agent: agent}
	if f.Kind == query.SourceFailure {
		in.ErrorPattern = f.Pattern
	}
	if agent != "" && len(runs) > 0 {
		good, err := s.Store.ListRuns(ctx, tenant, query.RunFilter{From: f.From, To: f.To, Agent: agent, Status: "ok", Limit: baselineRuns})
		if err != nil {
			s.storeError(w, r, err)
			return
		}
		problem := map[string]bool{}
		for _, p := range runs {
			problem[p.TraceID] = true
		}
		ids := make([]string, 0, len(good))
		for _, g := range good {
			if !problem[g.TraceID] {
				ids = append(ids, g.TraceID)
			}
		}
		if in.Baseline, err = s.Store.RunsForEval(ctx, tenant, ids); err != nil {
			s.storeError(w, r, err)
			return
		}
	}
	sugg := evals.Suggest(in)
	existing, err := s.Evals.ListScorers(ctx, tenant)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	names := map[string]bool{}
	for _, e := range existing {
		names[strings.ToLower(e.Name)] = true
	}
	for i := range sugg {
		for _, e := range existing {
			if evals.Same(e, sugg[i].Scorer) {
				sugg[i].Exists, sugg[i].Scorer.Name = true, e.Name
				break
			}
		}
		if sugg[i].Exists {
			continue
		}
		// Keep names unique among saved checks.
		base := sugg[i].Scorer.Name
		for n := 2; names[strings.ToLower(sugg[i].Scorer.Name)]; n++ {
			sugg[i].Scorer.Name = fmt.Sprintf("%s (%d)", base, n)
		}
		names[strings.ToLower(sugg[i].Scorer.Name)] = true
	}
	traceIDs := make([]string, 0, len(runs))
	for _, r := range runs {
		traceIDs = append(traceIDs, r.TraceID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source": problemLabel(f), "agent": agent, "matched": len(runs), "trace_ids": traceIDs, "suggestions": sugg,
	})
}

// addProblemItems handles POST /api/datasets/{id}/items/from-problems.
func (s *Server) addProblemItems(w http.ResponseWriter, r *http.Request) {
	id, ok := s.datasetID(w, r)
	if !ok {
		return
	}
	var b problemBody
	if !decodeBodyLimit(w, r, &b, maxEvalBodyBytes) {
		return
	}
	f, msg := b.filter(s.now())
	if msg != "" {
		badRequest(w, msg)
		return
	}
	ctx, tenant := r.Context(), tenantFrom(r.Context())
	runs, err := s.problemRuns(ctx, tenant, f)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	added := 0
	if len(runs) > 0 {
		if added, err = s.Evals.AddDatasetItems(ctx, tenant, id, runs); err != nil {
			s.evalError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "found": len(runs), "source": problemLabel(f)})
}

// economics handles GET /api/economics: cost per successful outcome per
// agent version.
func (s *Server) economics(w http.ResponseWriter, r *http.Request) {
	name, from, to, ok := s.window(w, r, "7d")
	if !ok {
		return
	}
	es, err := s.Store.Economics(r.Context(), tenantFrom(r.Context()), from, to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": name, "agents": es})
}
