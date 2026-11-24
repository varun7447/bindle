package search

import (
	"context"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/outcomes"
	"github.com/bindle-ai/bindle/backend/internal/skillship"
)

// SkillLesson is the failure a skill was written for, for measuring how
// often it recurs (zero: not measured).
type SkillLesson struct {
	StepName, Tool, Pattern string
}

// SkillRunFacts returns, for each run of an agent started in [from, to)
// (most recent first, at most maxGradedRuns), what measuring a shipped
// release needs: whether the run loaded that release ("<path>@<release>"),
// loaded another release of the same skill, was successful (graded like
// outcome grading: no failed step, no failed outcome, no reversal, with
// extra holding outcomes matched later by correlation), whether the
// lesson's failure happened in it, and its tokens and duration.
func (s *Store) SkillRunFacts(ctx context.Context, tenant, agent, path string, release int, lesson SkillLesson,
	from, to time.Time, extra map[string][]outcomes.OutcomeFact, settings outcomes.Settings) ([]skillship.RunFact, error) {
	var tr struct {
		Hits struct {
			Hits []struct {
				Source traceDoc `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	body := map[string]any{"size": maxGradedRuns,
		"_source": []string{"trace_id", "failed_steps", "root_status", "input_tokens", "output_tokens", "root_duration_ms"},
		"sort":    []any{map[string]any{"first_start_ns": "desc"}, map[string]any{"trace_id": "desc"}}}
	if err := s.search(ctx, tenant, s.Names.Traces(), []map[string]any{term("agent_name", agent), rangeNs("first_start_ns", from, to)}, body, &tr); err != nil {
		return nil, err
	}
	if len(tr.Hits.Hits) == 0 {
		return []skillship.RunFact{}, nil
	}
	steps := rangeNs("start_ns", from, to.Add(time.Hour))
	ref := skillship.Ref(path, release)
	loaded, err := s.traceSet(ctx, tenant, []map[string]any{steps, term("agent_name", agent), term("skills_used", ref)})
	if err != nil {
		return nil, err
	}
	anyRelease, err := s.traceSet(ctx, tenant, []map[string]any{steps, term("agent_name", agent),
		{"prefix": map[string]any{"skills_used": path + "@"}}})
	if err != nil {
		return nil, err
	}
	recurred := map[string]bool{}
	if lesson.Pattern != "" {
		where := []map[string]any{steps, term("agent_name", agent), term("status", "failure"), term("error_pattern", lesson.Pattern)}
		if lesson.Tool != "" {
			where = append(where, term("tool_name", lesson.Tool))
		} else {
			where = append(where, term("name", lesson.StepName))
		}
		if recurred, err = s.traceSet(ctx, tenant, where); err != nil {
			return nil, err
		}
	}
	inRun, err := s.outcomeSteps(ctx, tenant, rangeNs("start_ns", from, to.Add(scoreLag)))
	if err != nil {
		return nil, err
	}
	out := make([]skillship.RunFact, 0, len(tr.Hits.Hits))
	for _, h := range tr.Hits.Hits {
		d := h.Source
		success := d.FailedSteps == 0 && d.RootStatus != "failure"
		for _, o := range append(append([]outcomes.OutcomeFact{}, inRun[d.TraceID]...), extra[d.TraceID]...) {
			if settings.IsReversal(o.Name) || !o.Success {
				success = false
			}
		}
		out = append(out, skillship.RunFact{TraceID: d.TraceID, Loaded: loaded[d.TraceID],
			Other: anyRelease[d.TraceID] && !loaded[d.TraceID], Success: success, Recurred: recurred[d.TraceID],
			Tokens: d.InputTokens + d.OutputTokens, Ms: d.RootDurationMs})
	}
	return out, nil
}

// traceSet returns the trace IDs with at least one step matching the
// clauses (at most maxJoinedRuns).
func (s *Store) traceSet(ctx context.Context, tenant string, clauses []map[string]any) (map[string]bool, error) {
	var resp struct {
		Aggregations struct {
			Traces struct {
				Buckets []struct {
					Key string `json:"key"`
				} `json:"buckets"`
			} `json:"traces"`
		} `json:"aggregations"`
	}
	body := map[string]any{"size": 0, "aggs": map[string]any{"traces": map[string]any{
		"terms": map[string]any{"field": "trace_id", "size": maxJoinedRuns}}}}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(resp.Aggregations.Traces.Buckets))
	for _, b := range resp.Aggregations.Traces.Buckets {
		out[b.Key] = true
	}
	return out, nil
}
