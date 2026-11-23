package search

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

// ---- passport drift ----------------------------------------------------------

// PassportDrift counts drifting steps in [from, to) per drift kind and agent,
// with the latest examples. Drift is recorded on steps at ingest, from each
// agent's passport at the time.
func (s *Store) PassportDrift(ctx context.Context, tenant string, from, to time.Time) ([]query.DriftSignal, error) {
	var resp struct {
		Aggregations struct {
			Agents struct {
				Buckets []struct {
					Key     string    `json:"key"`
					Reasons countAggs `json:"reasons"`
					Kinds   struct {
						Buckets []struct {
							Key      string   `json:"key"`
							DocCount uint64   `json:"doc_count"`
							Runs     valueAgg `json:"runs"`
							Last     valueAgg `json:"last"`
							Examples struct {
								Hits struct {
									Hits []struct {
										Source stepDoc `json:"_source"`
									} `json:"hits"`
								} `json:"hits"`
							} `json:"examples"`
						} `json:"buckets"`
					} `json:"kinds"`
				} `json:"buckets"`
			} `json:"agents"`
		} `json:"aggregations"`
	}
	// Kinds are the part before ":" of each reason; a script-free way to group
	// is one filter per kind.
	kinds := map[string]any{}
	for _, k := range driftKinds {
		kinds[k] = map[string]any{"prefix": map[string]any{"passport_drift": k + ":"}}
	}
	body := map[string]any{"size": 0, "aggs": map[string]any{"agents": map[string]any{
		"terms": map[string]any{"field": "agent_name", "size": 50, "missing": ""},
		"aggs": map[string]any{
			"reasons": map[string]any{"terms": map[string]any{"field": "passport_drift", "size": 100}},
			"kinds": map[string]any{
				"filters": map[string]any{"filters": kinds, "keyed": false},
				"aggs": map[string]any{
					"runs": map[string]any{"cardinality": map[string]any{"field": "trace_id"}},
					"last": map[string]any{"max": map[string]any{"field": "start_ns"}},
					"examples": map[string]any{"top_hits": map[string]any{"size": 3, "sort": []any{map[string]any{"start_ns": "desc"}},
						"_source": []string{"trace_id", "step_id", "name", "tool_name", "passport_drift"}}},
				},
			},
		},
	}}}
	clauses := []map[string]any{{"exists": map[string]any{"field": "passport_drift"}}, rangeNs("start_ns", from, to)}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return nil, err
	}
	out := []query.DriftSignal{}
	for _, a := range resp.Aggregations.Agents.Buckets {
		for i, k := range a.Kinds.Buckets {
			if k.DocCount == 0 || i >= len(driftKinds) {
				continue
			}
			kind := driftKinds[i]
			sig := query.DriftSignal{Kind: kind, Agent: a.Key, Steps: k.DocCount, Runs: uint64(k.Runs.Value),
				LastSeen: nsTime(int64(k.Last.Value)), Reasons: map[string]uint64{}, Examples: []query.SecurityExample{}}
			for _, r := range a.Reasons.Buckets {
				if strings.HasPrefix(r.Key, kind+":") {
					sig.Reasons[r.Key] = r.DocCount
				}
			}
			for _, h := range k.Examples.Hits.Hits {
				d := h.Source
				ex := query.SecurityExample{TraceID: d.TraceID, StepID: d.StepID, StepName: d.Name, ToolName: d.ToolName, Reasons: []string{}}
				for _, r := range d.PassportDrift {
					if strings.HasPrefix(r, kind+":") {
						ex.Reasons = append(ex.Reasons, r)
					}
				}
				sig.Examples = append(sig.Examples, ex)
			}
			out = append(out, sig)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Steps != out[j].Steps {
			return out[i].Steps > out[j].Steps
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Agent < out[j].Agent
	})
	return out, nil
}

// driftKinds are the per-step drift kinds, in the order of the filters
// aggregation's (unkeyed) buckets.
var driftKinds = []string{"tool_not_allowed", "data_class_not_allowed", "egress_blocked", "release_not_allowed"}

// ToolCalls counts an agent's tool steps whose tool name matches pattern
// (* is a wildcard) in [from, to), with the latest run as a sample.
func (s *Store) ToolCalls(ctx context.Context, tenant, agent, pattern string, from, to time.Time) (uint64, string, error) {
	var resp struct {
		Hits struct {
			Total struct {
				Value uint64 `json:"value"`
			} `json:"total"`
			Hits []struct {
				Source stepDoc `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	tool := map[string]any{"term": map[string]any{"tool_name": pattern}}
	if strings.Contains(pattern, "*") {
		tool = map[string]any{"wildcard": map[string]any{"tool_name": map[string]any{"value": wildcardValue(pattern)}}}
	}
	clauses := []map[string]any{
		{"term": map[string]any{"type": model.StepTool}},
		{"term": map[string]any{"agent_name": agent}},
		tool,
		rangeNs("start_ns", from, to),
	}
	body := map[string]any{"size": 1, "track_total_hits": true, "sort": []any{map[string]any{"start_ns": "desc"}},
		"_source": []string{"trace_id"}}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return 0, "", err
	}
	trace := ""
	if len(resp.Hits.Hits) > 0 {
		trace = resp.Hits.Hits[0].Source.TraceID
	}
	return resp.Hits.Total.Value, trace, nil
}

// wildcardValue escapes the wildcard query's other special characters (? and
// backslash), so only * is a wildcard, as in passports.
func wildcardValue(pattern string) string {
	r := strings.NewReplacer(`\`, `\\`, `?`, `\?`)
	return r.Replace(pattern)
}
