package search

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/query"
)

const (
	maxProblemRuns = 200
	// Outcome and feedback results are joined to runs by trace. Scores can
	// arrive after the run (a user's thumbs down the next day), so they are
	// looked up for a week past the range.
	scoreLag       = 7 * 24 * time.Hour
	maxJoinedRuns  = 5000
	maxEconAgents  = 100
	maxEconVersion = 20
)

func notTerm(field string, v any) map[string]any {
	return map[string]any{"bool": map[string]any{"must_not": []any{term(field, v)}}}
}

// problemRequest builds the clauses and body for ProblemRuns. Exposed for tests.
func problemRequest(f query.ProblemFilter) (index string, clauses []map[string]any, body map[string]any, err error) {
	limit := f.Limit
	if limit <= 0 || limit > maxProblemRuns {
		limit = maxProblemRuns
	}
	if f.Kind == query.SourceFailedRuns {
		clauses = []map[string]any{rangeNs("first_start_ns", f.From, f.To), failedClause()}
		if f.Agent != "" {
			clauses = append(clauses, term("agent_name", f.Agent))
		}
		body = map[string]any{"size": limit, "_source": []string{"trace_id"},
			"sort": []any{map[string]any{"first_start_ns": "desc"}, map[string]any{"trace_id": "desc"}}}
		return "traces", clauses, body, nil
	}
	clauses = []map[string]any{rangeNs("start_ns", f.From, f.To)}
	switch f.Kind {
	case query.SourceFailure:
		clauses = append(clauses, term("status", "failure"))
		if f.StepName != "" {
			clauses = append(clauses, term("name", f.StepName))
		}
		if f.Pattern != "" {
			clauses = append(clauses, term("error_pattern", f.Pattern))
		}
		if f.Agent != "" {
			clauses = append(clauses, term("agent_name", f.Agent))
		}
	case query.SourceNegativeFeedback:
		clauses = append(clauses, term("score_source", "user"), term("score_pass", "fail"))
	case query.SourceFailedScore:
		clauses = append(clauses, term("type", "eval"), term("score_pass", "fail"), notTerm("score_source", "user"))
		if f.Score != "" {
			clauses = append(clauses, term("score_name", f.Score))
		}
	default:
		return "", nil, nil, fmt.Errorf("search: unknown problem source %q", f.Kind)
	}
	body = map[string]any{"size": 0, "aggs": map[string]any{"traces": map[string]any{
		"terms": map[string]any{"field": "trace_id", "size": limit, "order": map[string]any{"latest": "desc"}},
		"aggs":  map[string]any{"latest": map[string]any{"max": map[string]any{"field": "start_ns"}}},
	}}}
	return "steps", clauses, body, nil
}

// ProblemRuns implements query.Store. Feedback and score results have no
// agent of their own when sent after the run, so the caller filters those by
// the run's agent.
func (s *Store) ProblemRuns(ctx context.Context, tenant string, f query.ProblemFilter) ([]string, error) {
	index, clauses, body, err := problemRequest(f)
	if err != nil {
		return nil, err
	}
	out := []string{}
	if index == "traces" {
		var resp hits[traceDoc]
		if err := s.search(ctx, tenant, s.Names.Traces(), clauses, body, &resp); err != nil {
			return nil, err
		}
		for _, h := range resp.Hits.Hits {
			out = append(out, h.Source.TraceID)
		}
		return out, nil
	}
	var resp struct {
		Aggregations struct {
			Traces keyList `json:"traces"`
		} `json:"aggregations"`
	}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return nil, err
	}
	for _, b := range resp.Aggregations.Traces.Buckets {
		out = append(out, b.Key)
	}
	return out, nil
}

type econBucket struct {
	Key      string   `json:"key"`
	DocCount uint64   `json:"doc_count"`
	Cost     numValue `json:"cost"`
	Failed   struct {
		DocCount uint64   `json:"doc_count"`
		Cost     numValue `json:"cost"`
	} `json:"failed"`
	Unknown struct {
		DocCount uint64 `json:"doc_count"`
	} `json:"unknown"`
}

// econAgg is the per agent and version totals of the traces index.
type econAgg struct {
	Aggregations struct {
		Agents struct {
			Buckets []struct {
				Key      string `json:"key"`
				Versions struct {
					Buckets []econBucket `json:"buckets"`
				} `json:"versions"`
			} `json:"buckets"`
		} `json:"agents"`
	} `json:"aggregations"`
}

func econTotalsBody() map[string]any {
	return map[string]any{"size": 0, "aggs": map[string]any{"agents": map[string]any{
		"terms": map[string]any{"field": "agent_name", "size": maxEconAgents},
		"aggs": map[string]any{"versions": map[string]any{
			"terms": map[string]any{"field": "agent_version", "size": maxEconVersion},
			"aggs": map[string]any{
				"cost":    map[string]any{"sum": map[string]any{"field": "cost_usd"}},
				"failed":  map[string]any{"filter": failedClause(), "aggs": map[string]any{"cost": map[string]any{"sum": map[string]any{"field": "cost_usd"}}}},
				"unknown": map[string]any{"filter": map[string]any{"range": map[string]any{"cost_unknown_steps": map[string]any{"gt": 0}}}},
			},
		}},
	}}}
}

// passFail is how many pass and fail results a run has for one source.
type passFail struct{ pass, fail uint64 }

// scoreResults returns pass/fail counts per trace for score steps matching
// clauses (at most maxJoinedRuns traces).
func (s *Store) scoreResults(ctx context.Context, tenant string, clauses []map[string]any) (map[string]passFail, error) {
	var resp struct {
		Aggregations struct {
			Traces struct {
				Buckets []struct {
					Key  string   `json:"key"`
					Pass keyCount `json:"pass"`
				} `json:"buckets"`
			} `json:"traces"`
		} `json:"aggregations"`
	}
	body := map[string]any{"size": 0, "aggs": map[string]any{"traces": map[string]any{
		"terms": map[string]any{"field": "trace_id", "size": maxJoinedRuns},
		"aggs":  map[string]any{"pass": map[string]any{"terms": map[string]any{"field": "score_pass", "size": 2}}},
	}}}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return nil, err
	}
	out := make(map[string]passFail, len(resp.Aggregations.Traces.Buckets))
	for _, b := range resp.Aggregations.Traces.Buckets {
		out[b.Key] = passFail{pass: b.Pass.count("pass"), fail: b.Pass.count("fail")}
	}
	return out, nil
}

type econKey struct{ agent, version string }

// econAdjust is what the per-run outcome and feedback results change.
type econAdjust struct {
	outcomeRuns, outcomePass, outcomeFail uint64
	outcomeFailUSD                        float64
	failedWithOutcome                     uint64
	failedWithOutcomeUSD                  float64
	dislikedOK                            uint64 // succeeded runs with negative feedback
	dislikedOKUSD                         float64
}

// Economics implements query.Store.
func (s *Store) Economics(ctx context.Context, tenant string, from, to time.Time) ([]query.Economics, error) {
	var totals econAgg
	if err := s.search(ctx, tenant, s.Names.Traces(), []map[string]any{rangeNs("first_start_ns", from, to)}, econTotalsBody(), &totals); err != nil {
		return nil, err
	}
	if len(totals.Aggregations.Agents.Buckets) == 0 {
		return []query.Economics{}, nil
	}
	scoreRange := rangeNs("start_ns", from, to.Add(scoreLag))
	outcomes, err := s.scoreResults(ctx, tenant, []map[string]any{term("score_source", "outcome"), scoreRange})
	if err != nil {
		return nil, err
	}
	disliked, err := s.scoreResults(ctx, tenant, []map[string]any{term("score_source", "user"), term("score_pass", "fail"), scoreRange})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(outcomes)+len(disliked))
	for id := range outcomes {
		ids = append(ids, id)
	}
	for id := range disliked {
		if _, dup := outcomes[id]; !dup {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	adj := map[econKey]*econAdjust{}
	explicit := map[string]bool{} // agents that send outcomes
	var docs []traceDoc
	for len(ids) > 0 {
		n := min(len(ids), maxJoinedRuns)
		batch := ids[:n]
		ids = ids[n:]
		var resp hits[traceDoc]
		clauses := []map[string]any{{"terms": map[string]any{"trace_id": batch}}, rangeNs("first_start_ns", from, to)}
		body := map[string]any{"size": len(batch), "_source": []string{"trace_id", "agent_name", "agent_version", "cost_usd", "failed_steps", "root_status"}}
		if err := s.search(ctx, tenant, s.Names.Traces(), clauses, body, &resp); err != nil {
			return nil, err
		}
		for _, h := range resp.Hits.Hits {
			docs = append(docs, h.Source)
			if _, ok := outcomes[h.Source.TraceID]; ok {
				explicit[h.Source.AgentName] = true
			}
		}
	}
	for _, d := range docs {
		k := econKey{d.AgentName, d.AgentVersion}
		a := adj[k]
		if a == nil {
			a = &econAdjust{}
			adj[k] = a
		}
		failed := d.FailedSteps > 0 || d.RootStatus == "failure"
		if o, ok := outcomes[d.TraceID]; ok && explicit[d.AgentName] {
			a.outcomeRuns++
			switch {
			case o.fail > 0:
				a.outcomeFail++
				a.outcomeFailUSD += d.CostUSD
			case o.pass > 0:
				a.outcomePass++
			}
			if failed {
				a.failedWithOutcome++
				a.failedWithOutcomeUSD += d.CostUSD
			}
		}
		if _, ok := disliked[d.TraceID]; ok && !failed && !explicit[d.AgentName] {
			a.dislikedOK++
			a.dislikedOKUSD += d.CostUSD
		}
	}
	out := []query.Economics{}
	for _, ab := range totals.Aggregations.Agents.Buckets {
		for _, vb := range ab.Versions.Buckets {
			e := economics(ab.Key, explicit[ab.Key], vb, adj[econKey{ab.Key, vb.Key}])
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return out[i].Version > out[j].Version
	})
	return out, nil
}

// economics combines one version's totals with its outcome and feedback
// results. Failed runs without an outcome always count as wasted.
func economics(agent string, explicit bool, b econBucket, a *econAdjust) query.Economics {
	if a == nil {
		a = &econAdjust{}
	}
	e := query.Economics{Agent: agent, Version: b.Key, Runs: b.DocCount, CostUSD: b.Cost.get(), UnknownCostRuns: b.Unknown.DocCount}
	failedRuns, failedUSD := b.Failed.DocCount, b.Failed.Cost.get()
	if explicit {
		e.Definition = query.OutcomeExplicit
		e.OutcomeRuns = a.outcomeRuns
		e.Successes = a.outcomePass
		// Failed runs without an outcome, plus runs whose outcome failed
		// (a failed outcome on a failed run is counted once).
		e.Wasted = failedRuns - min(a.failedWithOutcome, failedRuns) + a.outcomeFail
		e.WastedUSD = max(failedUSD-a.failedWithOutcomeUSD, 0) + a.outcomeFailUSD
	} else {
		e.Definition = query.OutcomeInferred
		e.Wasted = failedRuns + a.dislikedOK
		e.WastedUSD = failedUSD + a.dislikedOKUSD
		if e.Wasted < e.Runs {
			e.Successes = e.Runs - e.Wasted
		}
	}
	if e.Wasted > e.Runs {
		e.Wasted = e.Runs
	}
	if e.WastedUSD > e.CostUSD {
		e.WastedUSD = e.CostUSD
	}
	if e.Successes > 0 {
		v := e.CostUSD / float64(e.Successes)
		e.CostPerSuccess = &v
	}
	return e
}
