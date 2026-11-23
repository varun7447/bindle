package search

import (
	"context"
	"encoding/json"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/lessons"
)

// Failure groups behind lessons: per agent, per step name, per error
// pattern. Bounded so one noisy workspace stays cheap.
const (
	lessonAgents   = 50
	lessonSteps    = 50
	lessonPatterns = 10
	agentToolsMax  = 200
)

// LessonGroups returns failed steps grouped by agent, step and error
// pattern in [from, to), with distinct runs, first and last time, the most
// recent trace IDs and a few sample steps (already redacted at ingest).
func (s *Store) LessonGroups(ctx context.Context, tenant string, from, to time.Time) ([]lessons.Group, error) {
	type sample struct {
		TraceID string `json:"trace_id"`
		StepID  string `json:"step_id"`
		Type    string `json:"type"`
		Name    string `json:"name"`
		Tool    string `json:"tool_name"`
		StartNs int64  `json:"start_ns"`
		Input   string `json:"input"`
		Output  string `json:"output"`
	}
	var resp struct {
		Aggregations struct {
			Agents struct {
				Buckets []struct {
					Key   string `json:"key"`
					Steps struct {
						Buckets []struct {
							Key      string `json:"key"`
							Patterns struct {
								Buckets []struct {
									Key      string     `json:"key"`
									DocCount int        `json:"doc_count"`
									Runs     numValue   `json:"runs"`
									First    numValue   `json:"first"`
									Last     numValue   `json:"last"`
									Tool     keyBuckets `json:"tool"`
									Type     keyBuckets `json:"stype"`
									Traces   struct {
										Buckets []struct {
											Key string `json:"key"`
										} `json:"buckets"`
									} `json:"traces"`
									Samples hits[sample] `json:"samples"`
								} `json:"buckets"`
							} `json:"patterns"`
						} `json:"buckets"`
					} `json:"steps"`
				} `json:"buckets"`
			} `json:"agents"`
		} `json:"aggregations"`
	}
	latest := map[string]any{"max": map[string]any{"field": "start_ns"}}
	body := map[string]any{"size": 0, "aggs": map[string]any{"agents": map[string]any{
		"terms": map[string]any{"field": "agent_name", "size": lessonAgents, "exclude": []string{""}},
		"aggs": map[string]any{"steps": map[string]any{
			"terms": map[string]any{"field": "name", "size": lessonSteps},
			"aggs": map[string]any{"patterns": map[string]any{
				"terms": map[string]any{"field": "error_pattern", "size": lessonPatterns, "exclude": []string{""}},
				"aggs": map[string]any{
					"runs":  map[string]any{"cardinality": map[string]any{"field": "trace_id"}},
					"first": map[string]any{"min": map[string]any{"field": "start_ns"}},
					"last":  latest,
					"tool":  map[string]any{"terms": map[string]any{"field": "tool_name", "size": 1}},
					"stype": map[string]any{"terms": map[string]any{"field": "type", "size": 1}},
					"traces": map[string]any{"terms": map[string]any{"field": "trace_id", "size": lessons.MaxEvidence,
						"order": map[string]any{"latest": "desc"}}, "aggs": map[string]any{"latest": latest}},
					"samples": map[string]any{"top_hits": map[string]any{"size": lessons.PackEvidence,
						"sort":    []any{map[string]any{"start_ns": "desc"}},
						"_source": []string{"trace_id", "step_id", "type", "name", "tool_name", "start_ns", "input", "output"}}},
				},
			}},
		}},
	}}}
	clauses := []map[string]any{term("status", "failure"), rangeNs("start_ns", from, to)}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return nil, err
	}
	out := []lessons.Group{}
	for _, a := range resp.Aggregations.Agents.Buckets {
		for _, st := range a.Steps.Buckets {
			for _, p := range st.Patterns.Buckets {
				g := lessons.Group{Agent: a.Key, StepName: st.Key, StepType: p.Type.first(), Tool: p.Tool.first(), Pattern: p.Key,
					Runs: int(p.Runs.get()), Failures: p.DocCount, FirstSeen: nsAt(p.First.get()), LastSeen: nsAt(p.Last.get())}
				for _, t := range p.Traces.Buckets {
					g.Traces = append(g.Traces, t.Key)
				}
				for _, h := range p.Samples.Hits.Hits {
					x := h.Source
					g.Samples = append(g.Samples, lessons.Evidence{TraceID: x.TraceID, StepID: x.StepID, StepType: x.Type, Name: x.Name,
						Tool: x.Tool, At: time.Unix(0, x.StartNs).UTC(), Input: x.Input, Output: x.Output})
				}
				out = append(out, g)
			}
		}
	}
	return out, nil
}

func nsAt(v float64) time.Time { return time.Unix(0, int64(v)).UTC() }

// AgentToolNames returns, per agent, the tool names it called in [from, to).
func (s *Store) AgentToolNames(ctx context.Context, tenant string, from, to time.Time) (map[string][]string, error) {
	var resp struct {
		Aggregations struct {
			Agents struct {
				Buckets []struct {
					Key   string `json:"key"`
					Tools struct {
						Buckets []struct {
							Key json.RawMessage `json:"key"`
						} `json:"buckets"`
					} `json:"tools"`
				} `json:"buckets"`
			} `json:"agents"`
		} `json:"aggregations"`
	}
	body := map[string]any{"size": 0, "aggs": map[string]any{"agents": map[string]any{
		"terms": map[string]any{"field": "agent_name", "size": agentToolsMax, "exclude": []string{""}},
		"aggs": map[string]any{"tools": map[string]any{"terms": map[string]any{"field": "tool_name", "size": agentToolsMax, "exclude": []string{""}}}},
	}}}
	clauses := []map[string]any{term("type", "tool_call"), rangeNs("start_ns", from, to)}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, a := range resp.Aggregations.Agents.Buckets {
		names := []string{}
		for _, t := range a.Tools.Buckets {
			var n string
			if json.Unmarshal(t.Key, &n) == nil && n != "" {
				names = append(names, n)
			}
		}
		out[a.Key] = names
	}
	return out, nil
}
