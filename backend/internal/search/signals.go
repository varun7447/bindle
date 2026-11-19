package search

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/query"
)

// ---- security signals ------------------------------------------------------

// SecuritySignals counts flagged steps in [from, to) per security flag and
// agent, with the latest examples.
func (s *Store) SecuritySignals(ctx context.Context, tenant string, from, to time.Time) ([]query.SecuritySignal, error) {
	var resp struct {
		Aggregations struct {
			Flags struct {
				Buckets []struct {
					Key    string `json:"key"`
					Agents struct {
						Buckets []struct {
							Key      string        `json:"key"`
							DocCount uint64        `json:"doc_count"`
							Runs     valueAgg      `json:"runs"`
							Last     valueAgg      `json:"last"`
							Reasons  countAggs     `json:"reasons"`
							Examples hits[stepDoc] `json:"examples"`
						} `json:"buckets"`
					} `json:"agents"`
				} `json:"buckets"`
			} `json:"flags"`
		} `json:"aggregations"`
	}
	agents := map[string]any{
		"terms": map[string]any{"field": "agent_name", "size": 50, "missing": ""},
		"aggs": map[string]any{
			"runs": map[string]any{"cardinality": map[string]any{"field": "trace_id"}},
			"last": map[string]any{"max": map[string]any{"field": "start_ns"}},
			"examples": map[string]any{"top_hits": map[string]any{"size": 3, "sort": []any{map[string]any{"start_ns": "desc"}},
				"_source": []string{"trace_id", "step_id", "name", "tool_name", "security_reasons"}}},
		},
	}
	body := map[string]any{"size": 0, "aggs": map[string]any{"flags": map[string]any{
		"terms": map[string]any{"field": "security_flags", "size": 10},
		"aggs":  map[string]any{"agents": agents},
	}}}
	clauses := []map[string]any{{"exists": map[string]any{"field": "security_flags"}}, rangeNs("start_ns", from, to)}
	// Reasons are counted per flag below (a step can carry several flags).
	agents["aggs"].(map[string]any)["reasons"] = map[string]any{"terms": map[string]any{"field": "security_reasons", "size": 20}}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return nil, err
	}
	out := []query.SecuritySignal{}
	for _, f := range resp.Aggregations.Flags.Buckets {
		for _, a := range f.Agents.Buckets {
			sig := query.SecuritySignal{Flag: f.Key, Agent: a.Key, Steps: a.DocCount, Runs: uint64(a.Runs.Value),
				LastSeen: nsTime(int64(a.Last.Value)), Reasons: map[string]uint64{}, Examples: []query.SecurityExample{}}
			for _, r := range a.Reasons.Buckets {
				if strings.HasPrefix(r.Key, f.Key+":") {
					sig.Reasons[r.Key] = r.DocCount
				}
			}
			for _, h := range a.Examples.Hits.Hits {
				d := h.Source
				ex := query.SecurityExample{TraceID: d.TraceID, StepID: d.StepID, StepName: d.Name, ToolName: d.ToolName, Reasons: []string{}}
				for _, r := range d.SecurityReasons {
					if strings.HasPrefix(r, f.Key+":") {
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
		if out[i].Flag != out[j].Flag {
			return out[i].Flag < out[j].Flag
		}
		return out[i].Agent < out[j].Agent
	})
	return out, nil
}

// valueAgg is a metric aggregation's result; null (no documents) reads as 0.
type valueAgg struct {
	Value float64 `json:"value"`
}

type countAggs struct {
	Buckets []struct {
		Key      string `json:"key"`
		DocCount uint64 `json:"doc_count"`
	} `json:"buckets"`
}

// ---- topics (what users ask about) -----------------------------------------

// Topic search limits.
const (
	topicCandidates  = 60   // candidate terms proposed by significant_text
	topicMinRuns     = 3    // a topic needs this many runs in the range
	topicSampleShard = 5000 // runs sampled per shard when proposing terms
	topicsPerList    = 10
	// A rising topic is mentioned in at least risingLift times the share
	// of runs it was mentioned in before.
	risingLift = 1.5
)

// Topics compares what users asked an agent (all agents when agent is "")
// in [from, to) with the same-length period before it: terms mentioned in
// a clearly larger share of runs now, terms not mentioned before, and
// input volume and length. Counts are runs, however many steps a run has.
//
// significant_text only proposes candidate terms: its background counts
// are per shard (the earlier period usually sits in other daily indices)
// and per step, not per run. A second request counts each candidate's runs
// exactly in both periods, and that decides.
func (s *Store) Topics(ctx context.Context, tenant, agent string, from, to time.Time) (*query.Topics, error) {
	prevFrom := from.Add(-to.Sub(from))
	scope := []map[string]any{{"exists": map[string]any{"field": "user_input"}}}
	if agent != "" {
		scope = append(scope, term("agent_name", agent))
	}
	both := append(append([]map[string]any{}, scope...), rangeNs("start_ns", prevFrom, to))
	runs := func() map[string]any { return map[string]any{"cardinality": map[string]any{"field": "trace_id"}} }

	var resp struct {
		Aggregations struct {
			Cur struct {
				Runs   valueAgg `json:"runs"`
				Chars  valueAgg `json:"chars"`
				Sample struct {
					Terms struct {
						Buckets []struct {
							Key string `json:"key"`
						} `json:"buckets"`
					} `json:"terms"`
				} `json:"sample"`
			} `json:"cur"`
			Prev struct {
				Runs  valueAgg `json:"runs"`
				Chars valueAgg `json:"chars"`
			} `json:"prev"`
		} `json:"aggregations"`
	}
	stats := func() map[string]any {
		return map[string]any{"runs": runs(), "chars": map[string]any{"avg": map[string]any{"field": "user_input_bytes"}}}
	}
	cur := stats()
	cur["sample"] = map[string]any{
		"diversified_sampler": map[string]any{"field": "trace_id", "shard_size": topicSampleShard, "max_docs_per_value": 1},
		"aggs": map[string]any{"terms": map[string]any{"significant_text": map[string]any{
			"field": "user_input", "size": topicCandidates, "min_doc_count": topicMinRuns,
			// The background is the earlier period, pinned to the tenant
			// (its statistics come from the shard, not from the query). On
			// shards without earlier data the proposal falls back to the
			// most frequent terms, which the exact count below sorts out.
			"background_filter": tenantFilter(tenant, append(append([]map[string]any{}, scope...), rangeNs("start_ns", prevFrom, from))...),
			// Redaction markers ([REDACTED:EMAIL]), numbers and IDs are not topics.
			"exclude": "redacted|[0-9]+|[0-9a-f]{8,}",
		}}},
	}
	body := map[string]any{"size": 0, "aggs": map[string]any{
		"cur":  map[string]any{"filter": rangeNs("start_ns", from, to), "aggs": cur},
		"prev": map[string]any{"filter": rangeNs("start_ns", prevFrom, from), "aggs": stats()},
	}}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), both, body, &resp); err != nil {
		return nil, err
	}
	c, p := resp.Aggregations.Cur, resp.Aggregations.Prev
	t := &query.Topics{Rising: []query.Topic{}, New: []query.Topic{},
		Runs: uint64(c.Runs.Value), PreviousRuns: uint64(p.Runs.Value), AvgChars: c.Chars.Value, PrevAvgChars: p.Chars.Value}
	cands := c.Sample.Terms.Buckets
	// Without an earlier period there is nothing to compare with.
	if len(cands) == 0 || t.Runs == 0 || t.PreviousRuns == 0 {
		return t, nil
	}

	filters := map[string]any{}
	for _, b := range cands {
		if !stopwords[b.Key] {
			filters[b.Key] = map[string]any{"match": map[string]any{"user_input": b.Key}}
		}
	}
	if len(filters) == 0 {
		return t, nil
	}
	var cnt struct {
		Aggregations struct {
			Topics struct {
				Buckets map[string]struct {
					Cur struct {
						Runs     valueAgg      `json:"runs"`
						Examples hits[stepDoc] `json:"examples"`
					} `json:"cur"`
					Prev struct {
						Runs valueAgg `json:"runs"`
					} `json:"prev"`
				} `json:"buckets"`
			} `json:"topics"`
		} `json:"aggregations"`
	}
	cntBody := map[string]any{"size": 0, "aggs": map[string]any{"topics": map[string]any{
		"filters": map[string]any{"filters": filters},
		"aggs": map[string]any{
			"cur": map[string]any{"filter": rangeNs("start_ns", from, to), "aggs": map[string]any{
				"runs": runs(),
				"examples": map[string]any{"top_hits": map[string]any{"size": 10,
					"sort": []any{map[string]any{"start_ns": "desc"}}, "_source": []string{"trace_id"}}},
			}},
			"prev": map[string]any{"filter": rangeNs("start_ns", prevFrom, from), "aggs": map[string]any{"runs": runs()}},
		},
	}}}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), both, cntBody, &cnt); err != nil {
		return nil, err
	}
	for term, b := range cnt.Aggregations.Topics.Buckets {
		tp := query.Topic{Term: term, Runs: uint64(b.Cur.Runs.Value), PreviousRuns: uint64(b.Prev.Runs.Value), Examples: []string{}}
		if tp.Runs < topicMinRuns {
			continue
		}
		tp.Share, tp.PreviousShare = float64(tp.Runs)/float64(t.Runs), float64(tp.PreviousRuns)/float64(t.PreviousRuns)
		seen := map[string]bool{}
		for _, h := range b.Cur.Examples.Hits.Hits {
			if id := h.Source.TraceID; id != "" && !seen[id] && len(tp.Examples) < 3 {
				seen[id] = true
				tp.Examples = append(tp.Examples, id)
			}
		}
		switch {
		case tp.PreviousRuns == 0:
			t.New = append(t.New, tp)
		case tp.Share >= risingLift*tp.PreviousShare:
			t.Rising = append(t.Rising, tp)
		}
	}
	sort.Slice(t.New, func(i, j int) bool {
		if t.New[i].Runs != t.New[j].Runs {
			return t.New[i].Runs > t.New[j].Runs
		}
		return t.New[i].Term < t.New[j].Term
	})
	sort.Slice(t.Rising, func(i, j int) bool {
		li, lj := t.Rising[i].Share/t.Rising[i].PreviousShare, t.Rising[j].Share/t.Rising[j].PreviousShare
		if li != lj {
			return li > lj
		}
		return t.Rising[i].Term < t.Rising[j].Term
	})
	if len(t.New) > topicsPerList {
		t.New = t.New[:topicsPerList]
	}
	if len(t.Rising) > topicsPerList {
		t.Rising = t.Rising[:topicsPerList]
	}
	return t, nil
}

// stopwords are common English words that are never a topic on their own.
var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a about after again all also am an and any are as at be been before but by
		can could did do does doing don for from get got had has have having he her here hi hello him his how i if in
		into is it its just let like me more my no not now of on one or our out please so some than thank thanks that
		the their them then there these they this to too up us want was we were what when where which who why will with
		would yes you your`) {
		m[w] = true
	}
	return m
}()
