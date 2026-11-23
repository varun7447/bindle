package search

import (
	"context"
	"sort"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/correlate"
	"github.com/bindle-ai/bindle/backend/internal/outcomes"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

// maxCorrelatedRuns caps the runs one outcome event can be matched to.
const maxCorrelatedRuns = 100

// correlationRequest builds the RunsByCorrelation query. Exposed for tests.
func correlationRequest(correlations []string, from, to time.Time) (clauses []map[string]any, body map[string]any) {
	clauses = []map[string]any{{"terms": map[string]any{"correlations": correlations}}, rangeNs("start_ns", from, to)}
	body = map[string]any{"size": 0, "aggs": map[string]any{"traces": map[string]any{
		"terms": map[string]any{"field": "trace_id", "size": maxCorrelatedRuns, "order": map[string]any{"latest": "desc"}},
		"aggs":  map[string]any{"latest": map[string]any{"max": map[string]any{"field": "start_ns"}}},
	}}}
	return clauses, body
}

// RunsByCorrelation implements outcomes.Finder: the runs whose steps carry
// any of the stored correlations, most recent first. Correlations are
// tenant-bound hashes and the query is tenant-filtered, so a value never
// matches another workspace's runs.
func (s *Store) RunsByCorrelation(ctx context.Context, tenant string, correlations []string, from, to time.Time) ([]string, error) {
	valid := make([]string, 0, len(correlations))
	for _, c := range correlations {
		if correlate.ValidStored(c) {
			valid = append(valid, c)
		}
	}
	if len(valid) == 0 {
		return []string{}, nil
	}
	clauses, body := correlationRequest(valid, from, to)
	var resp struct {
		Aggregations struct {
			Traces keyList `json:"traces"`
		} `json:"aggregations"`
	}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Aggregations.Traces.Buckets))
	for _, b := range resp.Aggregations.Traces.Buckets {
		out = append(out, b.Key)
	}
	return out, nil
}

// maxGradedRuns caps the runs graded per request (most recent first).
const maxGradedRuns = 5000

// GradingFacts returns what outcome grading needs for runs started in
// [from, to): each run's agent, version and failure, its check and score
// results, and the outcomes recorded inside it (bindle.outcome(), up to a
// week after the range). Truncated is true when more than maxGradedRuns
// runs started in the range.
func (s *Store) GradingFacts(ctx context.Context, tenant string, from, to time.Time) (runs []outcomes.RunFacts, inRun map[string][]outcomes.OutcomeFact, truncated bool, err error) {
	var tr struct {
		Hits struct {
			Total struct {
				Value uint64 `json:"value"`
			} `json:"total"`
			Hits []struct {
				Source traceDoc `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	body := map[string]any{"size": maxGradedRuns, "track_total_hits": maxGradedRuns + 1,
		"_source": []string{"trace_id", "agent_name", "agent_version", "failed_steps", "root_status"},
		"sort":    []any{map[string]any{"first_start_ns": "desc"}, map[string]any{"trace_id": "desc"}}}
	if err := s.search(ctx, tenant, s.Names.Traces(), []map[string]any{rangeNs("first_start_ns", from, to)}, body, &tr); err != nil {
		return nil, nil, false, err
	}
	truncated = tr.Hits.Total.Value > maxGradedRuns
	scoreRange := rangeNs("start_ns", from, to.Add(scoreLag))
	evals, err := s.scoreResults(ctx, tenant, []map[string]any{term("type", "eval"), scoreRange,
		notTerm("score_source", "user"), notTerm("score_source", "outcome")})
	if err != nil {
		return nil, nil, false, err
	}
	inRun, err = s.outcomeSteps(ctx, tenant, scoreRange)
	if err != nil {
		return nil, nil, false, err
	}
	runs = make([]outcomes.RunFacts, 0, len(tr.Hits.Hits))
	for _, h := range tr.Hits.Hits {
		d := h.Source
		e := evals[d.TraceID]
		runs = append(runs, outcomes.RunFacts{TraceID: d.TraceID, Agent: d.AgentName, Version: d.AgentVersion,
			Failed: d.FailedSteps > 0 || d.RootStatus == "failure", EvalPass: int(e.pass), EvalFail: int(e.fail)})
	}
	return runs, inRun, truncated, nil
}

// outcomeSteps returns the bindle.outcome() results per run: each outcome
// name with whether it passed (a name that failed at least once counts as
// failed).
func (s *Store) outcomeSteps(ctx context.Context, tenant string, scoreRange map[string]any) (map[string][]outcomes.OutcomeFact, error) {
	var resp struct {
		Aggregations struct {
			Traces struct {
				Buckets []struct {
					Key   string `json:"key"`
					Names struct {
						Buckets []struct {
							Key  string   `json:"key"`
							Pass keyCount `json:"pass"`
						} `json:"buckets"`
					} `json:"names"`
				} `json:"buckets"`
			} `json:"traces"`
		} `json:"aggregations"`
	}
	body := map[string]any{"size": 0, "aggs": map[string]any{"traces": map[string]any{
		"terms": map[string]any{"field": "trace_id", "size": maxJoinedRuns},
		"aggs": map[string]any{"names": map[string]any{
			"terms": map[string]any{"field": "score_name", "size": 20},
			"aggs":  map[string]any{"pass": map[string]any{"terms": map[string]any{"field": "score_pass", "size": 2}}},
		}},
	}}}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), []map[string]any{term("score_source", "outcome"), scoreRange}, body, &resp); err != nil {
		return nil, err
	}
	out := map[string][]outcomes.OutcomeFact{}
	for _, t := range resp.Aggregations.Traces.Buckets {
		for _, n := range t.Names.Buckets {
			out[t.Key] = append(out[t.Key], outcomes.OutcomeFact{Name: n.Key, Success: n.Pass.count("fail") == 0})
		}
		sort.Slice(out[t.Key], func(i, j int) bool { return out[t.Key][i].Name < out[t.Key][j].Name })
	}
	return out, nil
}

// EconomicsGraded is Economics with outcome grading on: outcomes matched to
// runs by correlation (extra, from the control plane) count like
// bindle.outcome() results, and a reversal outcome counts as a failed
// outcome whatever success it was reported with.
func (s *Store) EconomicsGraded(ctx context.Context, tenant string, from, to time.Time, extra map[string][]outcomes.OutcomeFact, settings outcomes.Settings) ([]query.Economics, error) {
	var totals econAgg
	if err := s.search(ctx, tenant, s.Names.Traces(), []map[string]any{rangeNs("first_start_ns", from, to)}, econTotalsBody(), &totals); err != nil {
		return nil, err
	}
	if len(totals.Aggregations.Agents.Buckets) == 0 {
		return []query.Economics{}, nil
	}
	inRun, err := s.outcomeSteps(ctx, tenant, rangeNs("start_ns", from, to.Add(scoreLag)))
	if err != nil {
		return nil, err
	}
	return s.economicsFrom(ctx, tenant, from, to, totals, gradedOutcomes(inRun, extra, settings))
}

// gradedOutcomes merges in-run and matched outcomes into pass/fail counts,
// with reversals as failures.
func gradedOutcomes(inRun, extra map[string][]outcomes.OutcomeFact, settings outcomes.Settings) map[string]passFail {
	out := map[string]passFail{}
	add := func(trace string, facts []outcomes.OutcomeFact) {
		pf := out[trace]
		for _, f := range facts {
			if f.Success && !settings.IsReversal(f.Name) {
				pf.pass++
			} else {
				pf.fail++
			}
		}
		out[trace] = pf
	}
	for t, f := range inRun {
		add(t, f)
	}
	for t, f := range extra {
		add(t, f)
	}
	return out
}
