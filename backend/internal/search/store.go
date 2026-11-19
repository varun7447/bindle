package search

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/evals"
	"github.com/bindle-ai/bindle/backend/internal/payload"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

// Store implements query.Store. Authenticate it as the reader user.
type Store struct {
	Client *Client
	Names  Names
	// Payloads, if set, is read for offloaded fields in step detail, up to
	// MaxPayloadBytes per field (default 1 MiB).
	Payloads        payload.Store
	MaxPayloadBytes int64
	Log             *slog.Logger
}

var _ query.Store = (*Store)(nil)

func redactedMarker() *regexp.Regexp { return regexp.MustCompile(`\[REDACTED:([A-Z0-9_]+)\]`) }

// redactedKinds lists the distinct redaction labels in the given texts.
func redactedKinds(texts ...string) []string {
	seen := map[string]bool{}
	for _, t := range texts {
		for _, m := range redactedRe.FindAllStringSubmatch(t, -1) {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tenantFilter wraps clauses in a bool filter that always pins tenant_id.
func tenantFilter(tenant string, clauses ...map[string]any) map[string]any {
	f := []any{map[string]any{"term": map[string]any{"tenant_id": tenant}}}
	for _, c := range clauses {
		if c != nil {
			f = append(f, c)
		}
	}
	return map[string]any{"bool": map[string]any{"filter": f}}
}

// search is the only read path. It requires a tenant, routes to the
// tenant's shard and adds the tenant filter itself; callers supply only
// extra clauses and the rest of the request body.
func (s *Store) search(ctx context.Context, tenant, index string, clauses []map[string]any, body map[string]any, out any) error {
	if tenant == "" {
		return query.ErrNoTenant
	}
	req := map[string]any{"track_total_hits": false}
	for k, v := range body {
		req[k] = v
	}
	req["query"] = tenantFilter(tenant, clauses...)
	q := url.Values{"routing": {tenant}, "ignore_unavailable": {"true"}, "allow_no_indices": {"true"}}
	return s.Client.Do(ctx, http.MethodPost, "/"+index+"/_search", q, req, out)
}

func rangeNs(field string, from, to time.Time) map[string]any {
	return map[string]any{"range": map[string]any{field: map[string]any{"gte": from.UnixNano(), "lt": to.UnixNano()}}}
}

func term(field string, v any) map[string]any {
	return map[string]any{"term": map[string]any{field: v}}
}

func nsTime(ns int64) query.Time { return query.Time{Time: time.Unix(0, ns).UTC()} }

type hits[T any] struct {
	Hits struct {
		Hits []struct {
			Source T `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

// ---- runs ----------------------------------------------------------------

func (d traceDoc) run() query.Run {
	return query.Run{
		TraceID: d.TraceID, Started: nsTime(d.FirstStartNs), Steps: uint64(d.Steps), FailedSteps: uint64(d.FailedSteps),
		CostUSD: d.CostUSD, CostUnknownSteps: uint64(d.CostUnknownSteps), InputTokens: d.InputTokens, OutputTokens: d.OutputTokens,
		CachedTokens: d.CachedTokens, LLMCalls: d.LLMCalls, ToolCalls: d.ToolCalls, PeakInputTokens: d.PeakInputTokens,
		Agent: d.AgentName, AgentVersion: d.AgentVersion, Framework: d.Framework, Environment: d.Environment,
		SessionID: d.SessionID, RootStatus: d.RootStatus, DurationMs: d.RootDurationMs,
	}
}

func failedClause() map[string]any {
	return map[string]any{"bool": map[string]any{
		"should":               []any{map[string]any{"range": map[string]any{"failed_steps": map[string]any{"gt": 0}}}, term("root_status", "failure")},
		"minimum_should_match": 1,
	}}
}

// runsRequest builds the runs-list clauses and body. Exposed for tests.
func runsRequest(f query.RunFilter) ([]map[string]any, map[string]any) {
	clauses := []map[string]any{rangeNs("first_start_ns", f.From, f.To)}
	if f.Agent != "" {
		clauses = append(clauses, term("agent_name", f.Agent))
	}
	if f.Version != "" {
		clauses = append(clauses, term("agent_version", f.Version))
	}
	if f.SessionID != "" {
		clauses = append(clauses, term("session_id", f.SessionID))
	}
	if f.Environment != "" {
		clauses = append(clauses, term("environment", f.Environment))
	}
	switch f.Status {
	case "failed":
		clauses = append(clauses, failedClause())
	case "ok":
		clauses = append(clauses, map[string]any{"bool": map[string]any{"must_not": []any{failedClause()}}})
	}
	body := map[string]any{
		"size": f.Limit,
		"sort": []any{map[string]any{"first_start_ns": "desc"}, map[string]any{"trace_id": "desc"}},
	}
	if f.AfterTrace != "" {
		body["search_after"] = []any{f.AfterStart.UnixNano(), f.AfterTrace}
	}
	return clauses, body
}

// ListRuns implements query.Store.
func (s *Store) ListRuns(ctx context.Context, tenant string, f query.RunFilter) ([]query.Run, error) {
	clauses, body := runsRequest(f)
	var resp hits[traceDoc]
	if err := s.search(ctx, tenant, s.Names.Traces(), clauses, body, &resp); err != nil {
		return nil, err
	}
	out := make([]query.Run, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		out = append(out, h.Source.run())
	}
	return out, nil
}

// ---- trace ---------------------------------------------------------------

var summaryExcludes = []string{"input", "output", "reasoning", "attributes", "links", "user_input"}

func (d stepDoc) summary() query.StepSummary {
	return query.StepSummary{
		StepID: d.StepID, ParentStepID: d.ParentStepID, Name: d.Name, Type: d.Type, Status: d.Status, Attempt: d.Attempt,
		StartTime: nsTime(d.StartNs), DurationMs: d.DurationMs, Provider: d.Provider, Model: d.Model, ToolName: d.ToolName,
		InputTokens: d.InputTokens, OutputTokens: d.OutputTokens, CachedTokens: d.CachedTokens, CostUSD: d.CostUSD,
		CostKnown: d.CostKnown, ErrorMessage: d.ErrorMessage, InputBytes: uint64(d.InputBytes),
		OutputBytes: uint64(d.OutputBytes), ReasoningBytes: uint64(d.ReasoningBytes), Redacted: d.Redacted,
		SecurityFlags: d.SecurityFlags, SecurityReasons: d.SecurityReasons,
	}
}

// GetTrace implements query.Store; nil, nil when not found for this tenant.
func (s *Store) GetTrace(ctx context.Context, tenant, traceID string, maxSteps int) (*query.Trace, error) {
	var tr hits[traceDoc]
	if err := s.search(ctx, tenant, s.Names.Traces(), []map[string]any{term("trace_id", traceID)}, map[string]any{"size": 1}, &tr); err != nil {
		return nil, err
	}
	if len(tr.Hits.Hits) == 0 {
		return nil, nil
	}
	var st hits[stepDoc]
	body := map[string]any{
		"size":    maxSteps + 1,
		"sort":    []any{map[string]any{"start_ns": "asc"}, map[string]any{"step_id": "asc"}},
		"_source": map[string]any{"excludes": summaryExcludes},
	}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), []map[string]any{term("trace_id", traceID)}, body, &st); err != nil {
		return nil, err
	}
	t := &query.Trace{Run: tr.Hits.Hits[0].Source.run()}
	for _, h := range st.Hits.Hits {
		t.Steps = append(t.Steps, h.Source.summary())
	}
	if len(t.Steps) > maxSteps {
		t.Steps, t.Truncated = t.Steps[:maxSteps], true
	}
	return t, nil
}

// GetStep implements query.Store; nil, nil when not found for this tenant.
func (s *Store) GetStep(ctx context.Context, tenant, traceID, stepID string) (*query.StepDetail, error) {
	var st hits[stepDoc]
	clauses := []map[string]any{term("trace_id", traceID), term("step_id", stepID)}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, map[string]any{"size": 1}, &st); err != nil {
		return nil, err
	}
	if len(st.Hits.Hits) == 0 {
		return nil, nil
	}
	d := st.Hits.Hits[0].Source
	sd := &query.StepDetail{
		StepSummary: d.summary(), TraceID: d.TraceID, Framework: d.Framework,
		Input: d.Input, Output: d.Output, Reasoning: d.Reasoning,
		Attributes: d.Attributes, Links: d.Links, RedactedKinds: d.RedactedKinds,
	}
	s.loadPayloads(ctx, tenant, d, sd)
	return sd, nil
}

// loadPayloads replaces previews with offloaded content. The object key is
// rebuilt from the caller's tenant and the step's IDs, never read from the
// document, and must match the stored reference.
func (s *Store) loadPayloads(ctx context.Context, tenant string, d stepDoc, sd *query.StepDetail) {
	if len(d.PayloadRefs) == 0 {
		return
	}
	max := s.MaxPayloadBytes
	if max <= 0 {
		max = 1 << 20
	}
	day := time.Unix(0, d.StartNs).UTC().Format("2006.01.02")
	for _, f := range []struct {
		name string
		val  *string
	}{{"input", &sd.Input}, {"output", &sd.Output}, {"reasoning", &sd.Reasoning}} {
		ref, ok := d.PayloadRefs[f.name]
		if !ok {
			continue
		}
		key, err := payload.Key(tenant, day, d.TraceID, d.StepID, f.name)
		if err != nil || key != ref.Key || s.Payloads == nil {
			sd.TruncatedFields = append(sd.TruncatedFields, f.name)
			continue
		}
		data, truncated, err := s.Payloads.Get(ctx, key, max)
		if err != nil {
			if s.Log != nil {
				s.Log.Error("payload fetch failed", "tenant", tenant, "field", f.name, "err", err)
			}
			sd.TruncatedFields = append(sd.TruncatedFields, f.name)
			continue
		}
		*f.val = string(data)
		if truncated {
			sd.TruncatedFields = append(sd.TruncatedFields, f.name)
		}
	}
}

// ---- overview ------------------------------------------------------------

// Overview implements query.Store for runs started in [from, to).
func (s *Store) Overview(ctx context.Context, tenant string, from, to time.Time, bucket time.Duration) (*query.Overview, error) {
	var kpi struct {
		Aggregations struct {
			Runs   struct{ Value float64 } `json:"runs"`
			Failed struct {
				DocCount uint64 `json:"doc_count"`
			} `json:"failed"`
			Pct         struct{ Values map[string]*float64 } `json:"pct"`
			Cost        numValue                             `json:"cost"`
			CostUnknown numValue                             `json:"cost_unknown"`
			InTok       numValue                             `json:"in_tok"`
			OutTok      numValue                             `json:"out_tok"`
			Steps       numValue                             `json:"steps"`
			FailedSteps numValue                             `json:"failed_steps"`
			Series      struct {
				Buckets []struct {
					Key      int64  `json:"key"`
					DocCount uint64 `json:"doc_count"`
					Failed   struct {
						DocCount uint64 `json:"doc_count"`
					} `json:"failed"`
					Cost numValue `json:"cost"`
				} `json:"buckets"`
			} `json:"series"`
		} `json:"aggregations"`
	}
	sum := func(f string) map[string]any { return map[string]any{"sum": map[string]any{"field": f}} }
	body := map[string]any{"size": 0, "aggs": map[string]any{
		"runs":         map[string]any{"value_count": map[string]any{"field": "trace_id"}},
		"failed":       map[string]any{"filter": failedClause()},
		"pct":          map[string]any{"percentiles": map[string]any{"field": "root_duration_ms", "percents": []float64{50, 95}}},
		"cost":         sum("cost_usd"),
		"cost_unknown": sum("cost_unknown_steps"),
		"in_tok":       sum("input_tokens"),
		"out_tok":      sum("output_tokens"),
		"steps":        sum("steps"),
		"failed_steps": sum("failed_steps"),
		"series": map[string]any{
			"date_histogram": map[string]any{"field": "first_start", "fixed_interval": strconv.Itoa(int(bucket/time.Second)) + "s"},
			"aggs":           map[string]any{"failed": map[string]any{"filter": failedClause()}, "cost": sum("cost_usd")},
		},
	}}
	if err := s.search(ctx, tenant, s.Names.Traces(), []map[string]any{rangeNs("first_start_ns", from, to)}, body, &kpi); err != nil {
		return nil, err
	}
	a := kpi.Aggregations
	pct := func(k string) float64 {
		if v := a.Pct.Values[k]; v != nil {
			return *v
		}
		return 0
	}
	o := &query.Overview{
		Runs: uint64(a.Runs.Value), FailedRuns: a.Failed.DocCount, P50RunMs: pct("50.0"), P95RunMs: pct("95.0"),
		CostUSD: a.Cost.get(), CostUnknownSteps: uint64(a.CostUnknown.get()),
		InputTokens: int64(a.InTok.get()), OutputTokens: int64(a.OutTok.get()),
		Steps: uint64(a.Steps.get()), FailedSteps: uint64(a.FailedSteps.get()),
	}
	for _, b := range a.Series.Buckets {
		if b.DocCount == 0 {
			continue
		}
		o.Series = append(o.Series, query.SeriesPoint{
			Bucket: query.Time{Time: time.UnixMilli(b.Key).UTC()}, Runs: b.DocCount, FailedRuns: b.Failed.DocCount, CostUSD: b.Cost.get(),
		})
	}

	var tools struct {
		Aggregations struct {
			Tools struct {
				Buckets []struct {
					Key      string `json:"key"`
					DocCount uint64 `json:"doc_count"`
					Failures struct {
						DocCount uint64 `json:"doc_count"`
					} `json:"failures"`
				} `json:"buckets"`
			} `json:"tools"`
		} `json:"aggregations"`
	}
	toolBody := map[string]any{"size": 0, "aggs": map[string]any{"tools": map[string]any{
		"terms": map[string]any{"field": "tool_name", "size": 10, "exclude": []string{""},
			"order": []any{map[string]any{"failures": "desc"}, map[string]any{"_key": "asc"}}},
		"aggs": map[string]any{"failures": map[string]any{"filter": term("status", "failure")}},
	}}}
	clauses := []map[string]any{term("type", "tool_call"), rangeNs("start_ns", from, to)}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, toolBody, &tools); err != nil {
		return nil, err
	}
	for _, b := range tools.Aggregations.Tools.Buckets {
		if b.Failures.DocCount > 0 {
			o.TopFailingTools = append(o.TopFailingTools, query.ToolFailure{ToolName: b.Key, Failures: b.Failures.DocCount, Calls: b.DocCount})
		}
	}
	return o, nil
}

// ---- loops ---------------------------------------------------------------

// Loops implements query.Store: same tool + same input repeated in a trace.
func (s *Store) Loops(ctx context.Context, tenant string, from, to time.Time, minRepeats int) ([]query.Loop, error) {
	var resp struct {
		Aggregations struct {
			Loops struct {
				Buckets []struct {
					Key      []json.RawMessage `json:"key"`
					DocCount uint64            `json:"doc_count"`
					First    numValue          `json:"first"`
					Last     numValue          `json:"last"`
				} `json:"buckets"`
			} `json:"loops"`
		} `json:"aggregations"`
	}
	body := map[string]any{"size": 0, "aggs": map[string]any{"loops": map[string]any{
		"multi_terms": map[string]any{
			"terms":         []any{map[string]any{"field": "trace_id"}, map[string]any{"field": "tool_name"}, map[string]any{"field": "input_hash"}},
			"size":          100,
			"min_doc_count": minRepeats,
			"order":         map[string]any{"_count": "desc"},
		},
		"aggs": map[string]any{
			"first": map[string]any{"min": map[string]any{"field": "start_time"}},
			"last":  map[string]any{"max": map[string]any{"field": "start_time"}},
		},
	}}}
	clauses := []map[string]any{term("type", "tool_call"), rangeNs("start_ns", from, to)}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return nil, err
	}
	var out []query.Loop
	for _, b := range resp.Aggregations.Loops.Buckets {
		if len(b.Key) < 2 || b.DocCount < uint64(minRepeats) {
			continue
		}
		var traceID, tool string
		_ = json.Unmarshal(b.Key[0], &traceID)
		_ = json.Unmarshal(b.Key[1], &tool)
		out = append(out, query.Loop{
			TraceID: traceID, ToolName: tool, Repeats: b.DocCount,
			// min/max return epoch milliseconds: fine for display.
			FirstSeen: query.Time{Time: time.UnixMilli(int64(b.First.get())).UTC()},
			LastSeen:  query.Time{Time: time.UnixMilli(int64(b.Last.get())).UTC()},
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Repeats != out[j].Repeats {
			return out[i].Repeats > out[j].Repeats
		}
		return out[i].LastSeen.After(out[j].LastSeen.Time)
	})
	return out, nil
}

// CostlyRuns returns runs started in [from, to) that cost at least
// minCostUSD, most expensive first (alert evaluation).
func (s *Store) CostlyRuns(ctx context.Context, tenant string, from, to time.Time, minCostUSD float64, limit int) ([]query.Run, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	clauses := []map[string]any{rangeNs("first_start_ns", from, to), {"range": map[string]any{"cost_usd": map[string]any{"gte": minCostUSD}}}}
	body := map[string]any{"size": limit, "sort": []any{map[string]any{"cost_usd": "desc"}, map[string]any{"trace_id": "asc"}}}
	var resp hits[traceDoc]
	if err := s.search(ctx, tenant, s.Names.Traces(), clauses, body, &resp); err != nil {
		return nil, err
	}
	out := make([]query.Run, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		out = append(out, h.Source.run())
	}
	return out, nil
}

// Insights implements query.Store: per-agent summary, failures grouped by
// step name, and the previous period's totals.
func (s *Store) Insights(ctx context.Context, tenant string, from, to time.Time) (*query.Insights, error) {
	out := &query.Insights{Agents: []query.AgentSummary{}, Issues: []query.Issue{}, Outliers: []query.Outlier{}}
	sum := func(f string) map[string]any { return map[string]any{"sum": map[string]any{"field": f}} }
	p95 := map[string]any{"percentiles": map[string]any{"field": "root_duration_ms", "percents": []float64{95}}}
	type pctAgg struct {
		Values map[string]*float64 `json:"values"`
	}
	pickAt := func(p pctAgg, k string) float64 {
		if v := p.Values[k]; v != nil {
			return *v
		}
		return 0
	}
	pick := func(p pctAgg) float64 { return pickAt(p, "95.0") }
	median := func(f string) map[string]any {
		return map[string]any{"percentiles": map[string]any{"field": f, "percents": []float64{50}}}
	}

	// Agents.
	var agents struct {
		Aggregations struct {
			Agents struct {
				Buckets []struct {
					Key      string `json:"key"`
					DocCount uint64 `json:"doc_count"`
					Failed   struct {
						DocCount uint64 `json:"doc_count"`
					} `json:"failed"`
					Cost        numValue `json:"cost"`
					CostUnknown numValue `json:"cost_unknown"`
					P95         pctAgg   `json:"p95"`
					MedSteps    pctAgg   `json:"med_steps"`
					MedCost     pctAgg   `json:"med_cost"`
					Last        numValue `json:"last"`
				} `json:"buckets"`
			} `json:"agents"`
		} `json:"aggregations"`
	}
	agentBody := map[string]any{"size": 0, "aggs": map[string]any{"agents": map[string]any{
		"terms": map[string]any{"field": "agent_name", "size": 20, "missing": ""},
		"aggs": map[string]any{
			"failed": map[string]any{"filter": failedClause()}, "cost": sum("cost_usd"), "cost_unknown": sum("cost_unknown_steps"),
			"p95": p95, "last": map[string]any{"max": map[string]any{"field": "first_start_ns"}},
			"med_steps": median("steps"), "med_cost": median("cost_usd"),
		},
	}}}
	if err := s.search(ctx, tenant, s.Names.Traces(), []map[string]any{rangeNs("first_start_ns", from, to)}, agentBody, &agents); err != nil {
		return nil, err
	}
	for _, b := range agents.Aggregations.Agents.Buckets {
		out.Agents = append(out.Agents, query.AgentSummary{
			Agent: b.Key, Runs: b.DocCount, FailedRuns: b.Failed.DocCount, CostUSD: b.Cost.get(),
			CostPartial: b.CostUnknown.get() > 0, P95RunMs: pick(b.P95), LastRunStart: nsTime(int64(b.Last.get())),
			MedianSteps: pickAt(b.MedSteps, "50.0"), MedianCost: pickAt(b.MedCost, "50.0"),
		})
	}

	// Issues: failed steps grouped by name, with the latest example.
	var issues struct {
		Aggregations struct {
			Names struct {
				Buckets []struct {
					Key      string     `json:"key"`
					DocCount uint64     `json:"doc_count"`
					Runs     numValue   `json:"runs"`
					Last     numValue   `json:"last"`
					Sample   sampleHits `json:"sample"`
					Patterns struct {
						Buckets []struct {
							Key      string     `json:"key"`
							DocCount uint64     `json:"doc_count"`
							Sample   sampleHits `json:"sample"`
						} `json:"buckets"`
					} `json:"patterns"`
				} `json:"buckets"`
			} `json:"names"`
		} `json:"aggregations"`
	}
	issueBody := map[string]any{"size": 0, "aggs": map[string]any{"names": map[string]any{
		"terms": map[string]any{"field": "name", "size": 10, "order": []any{map[string]any{"_count": "desc"}, map[string]any{"_key": "asc"}}},
		"aggs": map[string]any{
			"runs":   map[string]any{"cardinality": map[string]any{"field": "trace_id"}},
			"last":   map[string]any{"max": map[string]any{"field": "start_ns"}},
			"sample": latestSample(),
			"patterns": map[string]any{
				"terms": map[string]any{"field": "error_pattern", "size": 3, "exclude": []string{""},
					"order": []any{map[string]any{"_count": "desc"}, map[string]any{"_key": "asc"}}},
				"aggs": map[string]any{"sample": latestSample()},
			},
		},
	}}}
	clauses := []map[string]any{term("status", "failure"), rangeNs("start_ns", from, to)}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, issueBody, &issues); err != nil {
		return nil, err
	}
	for _, b := range issues.Aggregations.Names.Buckets {
		is := query.Issue{StepName: b.Key, Failures: b.DocCount, Runs: uint64(b.Runs.get()), LastSeen: nsTime(int64(b.Last.get()))}
		if h := b.Sample.Hits.Hits; len(h) > 0 {
			is.StepType, is.SampleError, is.SampleTrace, is.SampleStepID = h[0].Source.Type, h[0].Source.ErrorMessage, h[0].Source.TraceID, h[0].Source.StepID
		}
		is.Patterns = []query.ErrorPattern{}
		for _, p := range b.Patterns.Buckets {
			ep := query.ErrorPattern{Pattern: p.Key, Failures: p.DocCount}
			if h := p.Sample.Hits.Hits; len(h) > 0 {
				ep.SampleTrace, ep.SampleStep = h[0].Source.TraceID, h[0].Source.StepID
			}
			is.Patterns = append(is.Patterns, ep)
		}
		out.Issues = append(out.Issues, is)
	}

	// Unusual runs: far above their agent's median steps or cost.
	outliers, err := s.outliers(ctx, tenant, from, to, out.Agents)
	if err != nil {
		return nil, err
	}
	out.Outliers = outliers

	// Previous period of the same length.
	var prev struct {
		Aggregations struct {
			Runs   struct{ Value float64 } `json:"runs"`
			Failed struct {
				DocCount uint64 `json:"doc_count"`
			} `json:"failed"`
			Cost numValue `json:"cost"`
			P95  pctAgg   `json:"p95"`
		} `json:"aggregations"`
	}
	prevBody := map[string]any{"size": 0, "aggs": map[string]any{
		"runs": map[string]any{"value_count": map[string]any{"field": "trace_id"}}, "failed": map[string]any{"filter": failedClause()},
		"cost": sum("cost_usd"), "p95": p95,
	}}
	span := to.Sub(from)
	if err := s.search(ctx, tenant, s.Names.Traces(), []map[string]any{rangeNs("first_start_ns", from.Add(-span), from)}, prevBody, &prev); err != nil {
		return nil, err
	}
	a := prev.Aggregations
	out.Previous = query.PeriodTotals{Runs: uint64(a.Runs.Value), FailedRuns: a.Failed.DocCount, CostUSD: a.Cost.get(), P95RunMs: pick(a.P95)}
	return out, nil
}

type sampleHits struct {
	Hits struct {
		Hits []struct {
			Source struct {
				TraceID      string `json:"trace_id"`
				StepID       string `json:"step_id"`
				Type         string `json:"type"`
				ErrorMessage string `json:"error_message"`
			} `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

func latestSample() map[string]any {
	return map[string]any{"top_hits": map[string]any{"size": 1, "sort": []any{map[string]any{"start_ns": "desc"}},
		"_source": []string{"trace_id", "step_id", "type", "error_message"}}}
}

// Outlier thresholds. An agent needs OutlierMinRuns runs for a meaningful
// median; a run is unusual at OutlierFactor times the median (and, for
// steps, at least OutlierMinExtraSteps more), so small agents with 2-step
// runs don't flag a 6-step run.
const (
	OutlierMinRuns       = 10
	OutlierFactor        = 3.0
	OutlierMinExtraSteps = 10
	maxOutliers          = 10
)

// outlierLimits returns, per agent, the step count and cost at which a run
// counts as unusual (0 = no limit). Exposed for tests.
func outlierLimits(agents []query.AgentSummary) map[string][2]float64 {
	out := map[string][2]float64{}
	for _, a := range agents {
		if a.Runs < OutlierMinRuns {
			continue
		}
		var lim [2]float64
		if a.MedianSteps > 0 {
			lim[0] = math.Max(math.Ceil(a.MedianSteps*OutlierFactor), a.MedianSteps+OutlierMinExtraSteps)
		}
		if a.MedianCost > 0 {
			lim[1] = a.MedianCost * OutlierFactor
		}
		if lim != [2]float64{} {
			out[a.Agent] = lim
		}
	}
	return out
}

func (s *Store) outliers(ctx context.Context, tenant string, from, to time.Time, agents []query.AgentSummary) ([]query.Outlier, error) {
	limits := outlierLimits(agents)
	out := []query.Outlier{}
	if len(limits) == 0 {
		return out, nil
	}
	var should []any
	names := make([]string, 0, len(limits))
	for name := range limits {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lim := limits[name]
		var over []any
		if lim[0] > 0 {
			over = append(over, map[string]any{"range": map[string]any{"steps": map[string]any{"gte": lim[0]}}})
		}
		if lim[1] > 0 {
			over = append(over, map[string]any{"range": map[string]any{"cost_usd": map[string]any{"gte": lim[1]}}})
		}
		should = append(should, map[string]any{"bool": map[string]any{
			"filter": []any{term("agent_name", name)},
			"should": over, "minimum_should_match": 1,
		}})
	}
	clauses := []map[string]any{rangeNs("first_start_ns", from, to), {"bool": map[string]any{"should": should, "minimum_should_match": 1}}}
	body := map[string]any{"size": maxOutliers, "sort": []any{map[string]any{"cost_usd": "desc"}, map[string]any{"steps": "desc"}, map[string]any{"trace_id": "asc"}}}
	var resp hits[traceDoc]
	if err := s.search(ctx, tenant, s.Names.Traces(), clauses, body, &resp); err != nil {
		return nil, err
	}
	med := map[string]query.AgentSummary{}
	for _, a := range agents {
		med[a.Agent] = a
	}
	for _, h := range resp.Hits.Hits {
		run := h.Source.run()
		lim, a := limits[run.Agent], med[run.Agent]
		o := query.Outlier{Run: run, Reason: "cost", Value: run.CostUSD, Typical: a.MedianCost}
		if lim[0] > 0 && float64(run.Steps) >= lim[0] {
			o = query.Outlier{Run: run, Reason: "steps", Value: float64(run.Steps), Typical: a.MedianSteps}
		}
		out = append(out, o)
	}
	return out, nil
}

// CostByAgent implements query.Store: priced spend per agent for runs
// started in [from, to). Runs without an agent name are under "".
func (s *Store) CostByAgent(ctx context.Context, tenant string, from, to time.Time) (map[string]float64, error) {
	var resp struct {
		Aggregations struct {
			Agents struct {
				Buckets []struct {
					Key  string   `json:"key"`
					Cost numValue `json:"cost"`
				} `json:"buckets"`
			} `json:"agents"`
		} `json:"aggregations"`
	}
	body := map[string]any{"size": 0, "aggs": map[string]any{"agents": map[string]any{
		"terms": map[string]any{"field": "agent_name", "size": 500, "missing": ""},
		"aggs":  map[string]any{"cost": map[string]any{"sum": map[string]any{"field": "cost_usd"}}},
	}}}
	if err := s.search(ctx, tenant, s.Names.Traces(), []map[string]any{rangeNs("first_start_ns", from, to)}, body, &resp); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, b := range resp.Aggregations.Agents.Buckets {
		out[b.Key] = b.Cost.get()
	}
	return out, nil
}

// Models implements query.Store: LLM calls per provider/model, most used
// first (at most 100).
func (s *Store) Models(ctx context.Context, tenant string, from, to time.Time) ([]query.ModelUsage, error) {
	var resp struct {
		Aggregations struct {
			Models struct {
				Buckets []struct {
					Key      []json.RawMessage `json:"key"`
					DocCount uint64            `json:"doc_count"`
					Unpriced struct {
						DocCount uint64 `json:"doc_count"`
					} `json:"unpriced"`
					Cost   numValue `json:"cost"`
					In     numValue `json:"in"`
					Out    numValue `json:"out"`
					Cached numValue `json:"cached"`
				} `json:"buckets"`
			} `json:"models"`
		} `json:"aggregations"`
	}
	sum := func(f string) map[string]any { return map[string]any{"sum": map[string]any{"field": f}} }
	body := map[string]any{"size": 0, "aggs": map[string]any{"models": map[string]any{
		"multi_terms": map[string]any{
			"terms": []any{map[string]any{"field": "provider", "missing": ""}, map[string]any{"field": "model"}},
			"size":  100, "order": map[string]any{"_count": "desc"},
		},
		"aggs": map[string]any{
			"unpriced": map[string]any{"filter": term("cost_known", false)},
			"cost":     sum("cost_usd"), "in": sum("input_tokens"), "out": sum("output_tokens"), "cached": sum("cached_tokens"),
		},
	}}}
	clauses := []map[string]any{term("type", "llm_call"), rangeNs("start_ns", from, to)}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return nil, err
	}
	out := []query.ModelUsage{}
	for _, b := range resp.Aggregations.Models.Buckets {
		if len(b.Key) < 2 {
			continue
		}
		var provider, model string
		_ = json.Unmarshal(b.Key[0], &provider)
		_ = json.Unmarshal(b.Key[1], &model)
		if model == "" {
			continue
		}
		out = append(out, query.ModelUsage{Provider: provider, Model: model, Calls: b.DocCount, Unpriced: b.Unpriced.DocCount,
			CostUSD: b.Cost.get(), InputTokens: int64(b.In.get()), OutputTokens: int64(b.Out.get()), CachedTokens: int64(b.Cached.get())})
	}
	return out, nil
}

// argErrorQuery matches failures whose message points at the arguments the
// model produced: the model misread the tool's schema.
func argErrorQuery() map[string]any {
	return map[string]any{"match": map[string]any{"error_message": map[string]any{
		"query": "invalid argument arguments parameter parameters schema validation validationerror required missing", "operator": "or"}}}
}

// Tools implements query.Store: per-tool calls, failures, latency and result
// size, most failures first (at most 100 tools).
func (s *Store) Tools(ctx context.Context, tenant string, from, to time.Time) ([]query.ToolHealth, error) {
	type pct struct {
		Values map[string]*float64 `json:"values"`
	}
	var resp struct {
		Aggregations struct {
			Tools struct {
				Buckets []struct {
					Key      string `json:"key"`
					DocCount uint64 `json:"doc_count"`
					Failures struct {
						DocCount uint64 `json:"doc_count"`
						Args     struct {
							DocCount uint64 `json:"doc_count"`
						} `json:"args"`
						Pattern keyBuckets `json:"pattern"`
						Sample  sampleHits `json:"sample"`
					} `json:"failures"`
					Runs      numValue   `json:"runs"`
					Latency   pct        `json:"latency"`
					AvgOut    numValue   `json:"avg_out"`
					MaxOut    numValue   `json:"max_out"`
					Framework keyBuckets `json:"framework"`
					Sample    sampleHits `json:"sample"`
				} `json:"buckets"`
			} `json:"tools"`
		} `json:"aggregations"`
	}
	body := map[string]any{"size": 0, "aggs": map[string]any{"tools": map[string]any{
		"terms": map[string]any{"field": "tool_name", "size": 100, "exclude": []string{""}, "order": []any{map[string]any{"failures": "desc"}, map[string]any{"_count": "desc"}}},
		"aggs": map[string]any{
			"failures": map[string]any{"filter": term("status", "failure"), "aggs": map[string]any{
				"args":    map[string]any{"filter": argErrorQuery()},
				"pattern": map[string]any{"terms": map[string]any{"field": "error_pattern", "size": 1, "exclude": []string{""}}},
				"sample":  latestSample(),
			}},
			"runs":      map[string]any{"cardinality": map[string]any{"field": "trace_id"}},
			"latency":   map[string]any{"percentiles": map[string]any{"field": "duration_ms", "percents": []float64{50, 95}}},
			"avg_out":   map[string]any{"avg": map[string]any{"field": "output_bytes"}},
			"max_out":   map[string]any{"max": map[string]any{"field": "output_bytes"}},
			"framework": map[string]any{"terms": map[string]any{"field": "framework", "size": 1, "exclude": []string{"", "unknown"}}},
			"sample":    latestSample(),
		},
	}}}
	clauses := []map[string]any{term("type", "tool_call"), rangeNs("start_ns", from, to)}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return nil, err
	}
	at := func(p pct, k string) float64 {
		if v := p.Values[k]; v != nil {
			return *v
		}
		return 0
	}
	out := []query.ToolHealth{}
	for _, b := range resp.Aggregations.Tools.Buckets {
		t := query.ToolHealth{ToolName: b.Key, Framework: b.Framework.first(), Calls: b.DocCount, Failures: b.Failures.DocCount,
			ArgErrors: b.Failures.Args.DocCount, Runs: uint64(b.Runs.get()), P50Ms: at(b.Latency, "50.0"), P95Ms: at(b.Latency, "95.0"),
			AvgOutputBytes: b.AvgOut.get(), MaxOutputBytes: b.MaxOut.get(), TopError: b.Failures.Pattern.first()}
		sample := b.Sample
		if b.Failures.DocCount > 0 {
			sample = b.Failures.Sample
		}
		if h := sample.Hits.Hits; len(h) > 0 {
			t.SampleTrace, t.SampleStep = h[0].Source.TraceID, h[0].Source.StepID
		}
		out = append(out, t)
	}
	return out, nil
}

// AgentVersions implements query.Store: one row per version of agent, most
// recent first (at most 20 versions).
func (s *Store) AgentVersions(ctx context.Context, tenant, agent string, from, to time.Time) ([]query.VersionStats, error) {
	type pct struct {
		Values map[string]*float64 `json:"values"`
	}
	var runs struct {
		Aggregations struct {
			Versions struct {
				Buckets []struct {
					Key      string `json:"key"`
					DocCount uint64 `json:"doc_count"`
					Failed   struct {
						DocCount uint64 `json:"doc_count"`
					} `json:"failed"`
					Cost        numValue `json:"cost"`
					CostUnknown numValue `json:"cost_unknown"`
					Dur         pct      `json:"dur"`
					Steps       numValue `json:"steps"`
					LLM         numValue `json:"llm"`
					In          numValue `json:"in"`
					Out         numValue `json:"out"`
					Cached      numValue `json:"cached"`
					First       numValue `json:"first"`
					Last        numValue `json:"last"`
				} `json:"buckets"`
			} `json:"versions"`
		} `json:"aggregations"`
	}
	sum := func(f string) map[string]any { return map[string]any{"sum": map[string]any{"field": f}} }
	body := map[string]any{"size": 0, "aggs": map[string]any{"versions": map[string]any{
		"terms": map[string]any{"field": "agent_version", "size": 20, "missing": "", "order": map[string]any{"last": "desc"}},
		"aggs": map[string]any{
			"failed": map[string]any{"filter": failedClause()}, "cost": sum("cost_usd"), "cost_unknown": sum("cost_unknown_steps"),
			"dur":   map[string]any{"percentiles": map[string]any{"field": "root_duration_ms", "percents": []float64{50, 95}}},
			"steps": map[string]any{"avg": map[string]any{"field": "steps"}}, "llm": map[string]any{"avg": map[string]any{"field": "llm_calls"}},
			"in": sum("input_tokens"), "out": sum("output_tokens"), "cached": sum("cached_tokens"),
			"first": map[string]any{"min": map[string]any{"field": "first_start_ns"}}, "last": map[string]any{"max": map[string]any{"field": "first_start_ns"}},
		},
	}}}
	clauses := []map[string]any{rangeNs("first_start_ns", from, to), term("agent_name", agent)}
	if err := s.search(ctx, tenant, s.Names.Traces(), clauses, body, &runs); err != nil {
		return nil, err
	}
	var tools struct {
		Aggregations struct {
			Versions struct {
				Buckets []struct {
					Key      string `json:"key"`
					DocCount uint64 `json:"doc_count"`
					Failed   struct {
						DocCount uint64 `json:"doc_count"`
					} `json:"failed"`
				} `json:"buckets"`
			} `json:"versions"`
		} `json:"aggregations"`
	}
	tbody := map[string]any{"size": 0, "aggs": map[string]any{"versions": map[string]any{
		"terms": map[string]any{"field": "agent_version", "size": 20, "missing": ""},
		"aggs":  map[string]any{"failed": map[string]any{"filter": term("status", "failure")}},
	}}}
	tclauses := []map[string]any{term("type", "tool_call"), term("agent_name", agent), rangeNs("start_ns", from, to)}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), tclauses, tbody, &tools); err != nil {
		return nil, err
	}
	toolsBy := map[string][2]uint64{}
	for _, b := range tools.Aggregations.Versions.Buckets {
		toolsBy[b.Key] = [2]uint64{b.DocCount, b.Failed.DocCount}
	}
	at := func(p pct, k string) float64 {
		if v := p.Values[k]; v != nil {
			return *v
		}
		return 0
	}
	out := []query.VersionStats{}
	for _, b := range runs.Aggregations.Versions.Buckets {
		tc := toolsBy[b.Key]
		out = append(out, query.VersionStats{
			Version: b.Key, Runs: b.DocCount, FailedRuns: b.Failed.DocCount, CostUSD: b.Cost.get(), CostPartial: b.CostUnknown.get() > 0,
			P50RunMs: at(b.Dur, "50.0"), P95RunMs: at(b.Dur, "95.0"), AvgSteps: b.Steps.get(), AvgLLMCalls: b.LLM.get(),
			InputTokens: int64(b.In.get()), OutputTokens: int64(b.Out.get()), CachedTokens: int64(b.Cached.get()),
			ToolCalls: tc[0], ToolFailures: tc[1],
			FirstRunStart: nsTime(int64(b.First.get())), LastRunStart: nsTime(int64(b.Last.get())),
		})
	}
	return out, nil
}

// maxEvalRuns bounds RunsForEval.
const maxEvalRuns = 500

// RunsForEval implements query.Store. Output is the root step's output, else
// the last LLM call's; input is the root step's, else the first LLM call's.
// Fields over the offload threshold are read from their inline preview.
func (s *Store) RunsForEval(ctx context.Context, tenant string, traceIDs []string) ([]evals.Run, error) {
	if len(traceIDs) == 0 {
		return []evals.Run{}, nil
	}
	if len(traceIDs) > maxEvalRuns {
		traceIDs = traceIDs[:maxEvalRuns]
	}
	ids := map[string]any{"terms": map[string]any{"trace_id": traceIDs}}
	var traces hits[traceDoc]
	if err := s.search(ctx, tenant, s.Names.Traces(), []map[string]any{ids}, map[string]any{"size": len(traceIDs)}, &traces); err != nil {
		return nil, err
	}
	type io struct {
		Hits struct {
			Hits []struct {
				Source struct {
					Input  string `json:"input"`
					Output string `json:"output"`
				} `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	type filtered struct {
		Hit io `json:"hit"`
	}
	var steps struct {
		Aggregations struct {
			Traces struct {
				Buckets []struct {
					Key   string `json:"key"`
					Tools struct {
						Names keyList `json:"names"`
					} `json:"tools"`
					Root     filtered `json:"root"`
					FirstLLM filtered `json:"first_llm"`
					LastLLM  filtered `json:"last_llm"`
				} `json:"buckets"`
			} `json:"traces"`
		} `json:"aggregations"`
	}
	hit := func(order string) map[string]any {
		return map[string]any{"top_hits": map[string]any{"size": 1, "sort": []any{map[string]any{"start_ns": order}}, "_source": []string{"input", "output"}}}
	}
	body := map[string]any{"size": 0, "aggs": map[string]any{"traces": map[string]any{
		"terms": map[string]any{"field": "trace_id", "size": len(traceIDs)},
		"aggs": map[string]any{
			"tools":     map[string]any{"filter": term("type", "tool_call"), "aggs": map[string]any{"names": map[string]any{"terms": map[string]any{"field": "tool_name", "size": 100}}}},
			"root":      map[string]any{"filter": term("parent_step_id", ""), "aggs": map[string]any{"hit": hit("asc")}},
			"first_llm": map[string]any{"filter": term("type", "llm_call"), "aggs": map[string]any{"hit": hit("asc")}},
			"last_llm":  map[string]any{"filter": term("type", "llm_call"), "aggs": map[string]any{"hit": hit("desc")}},
		},
	}}}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), []map[string]any{ids}, body, &steps); err != nil {
		return nil, err
	}
	pick := func(f filtered) (string, string) {
		if h := f.Hit.Hits.Hits; len(h) > 0 {
			return h[0].Source.Input, h[0].Source.Output
		}
		return "", ""
	}
	type extra struct {
		tools         []string
		input, output string
	}
	byTrace := map[string]extra{}
	for _, b := range steps.Aggregations.Traces.Buckets {
		var e extra
		for _, t := range b.Tools.Names.Buckets {
			e.tools = append(e.tools, t.Key)
		}
		rin, rout := pick(b.Root)
		fin, _ := pick(b.FirstLLM)
		_, lout := pick(b.LastLLM)
		e.input, e.output = first(rin, fin), first(rout, lout)
		byTrace[b.Key] = e
	}
	order := map[string]int{}
	for i, id := range traceIDs {
		order[id] = i
	}
	out := make([]evals.Run, 0, len(traces.Hits.Hits))
	for _, h := range traces.Hits.Hits {
		d := h.Source
		e := byTrace[d.TraceID]
		tools := e.tools
		if tools == nil {
			tools = []string{}
		}
		out = append(out, evals.Run{
			TraceID: d.TraceID, Agent: d.AgentName, Version: d.AgentVersion, Input: e.input, Output: e.output, Tools: tools,
			Steps: d.Steps, CostUSD: d.CostUSD, CostKnown: d.CostUnknownSteps == 0, DurationMs: d.RootDurationMs,
			Failed: d.FailedSteps > 0 || d.RootStatus == "failure",
		})
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i].TraceID] < order[out[j].TraceID] })
	return out, nil
}

type keyList struct {
	Buckets []struct {
		Key string `json:"key"`
	} `json:"buckets"`
}

func first(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// Scores implements query.Store: bindle.score() results for agent's runs in
// [from, to), per version and score name. Scores are joined to runs by trace
// (a score sent later from CI has no agent of its own).
func (s *Store) Scores(ctx context.Context, tenant, agent string, from, to time.Time) ([]query.VersionScore, error) {
	var resp struct {
		Aggregations struct {
			Traces struct {
				Buckets []struct {
					Key   string `json:"key"`
					Names struct {
						Buckets []struct {
							Key    string   `json:"key"`
							Count  uint64   `json:"doc_count"`
							Pass   keyCount `json:"pass"`
							Sum    numValue `json:"sum"`
							Valued struct {
								Value float64 `json:"value"`
							} `json:"valued"`
						} `json:"buckets"`
					} `json:"names"`
				} `json:"buckets"`
			} `json:"traces"`
		} `json:"aggregations"`
	}
	body := map[string]any{"size": 0, "aggs": map[string]any{"traces": map[string]any{
		"terms": map[string]any{"field": "trace_id", "size": 2000},
		"aggs": map[string]any{"names": map[string]any{
			"terms": map[string]any{"field": "score_name", "size": 20},
			"aggs": map[string]any{
				"pass":   map[string]any{"terms": map[string]any{"field": "score_pass", "size": 2}},
				"sum":    map[string]any{"sum": map[string]any{"field": "score_value"}},
				"valued": map[string]any{"value_count": map[string]any{"field": "score_value"}},
			},
		}},
	}}}
	clauses := []map[string]any{term("type", "eval"), {"exists": map[string]any{"field": "score_name"}}, rangeNs("start_ns", from, to.Add(7*24*time.Hour))}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Aggregations.Traces.Buckets) == 0 {
		return []query.VersionScore{}, nil
	}
	ids := make([]string, 0, len(resp.Aggregations.Traces.Buckets))
	for _, b := range resp.Aggregations.Traces.Buckets {
		ids = append(ids, b.Key)
	}
	var traces hits[traceDoc]
	tclauses := []map[string]any{{"terms": map[string]any{"trace_id": ids}}, term("agent_name", agent), rangeNs("first_start_ns", from, to)}
	if err := s.search(ctx, tenant, s.Names.Traces(), tclauses, map[string]any{"size": len(ids), "_source": []string{"trace_id", "agent_version"}}, &traces); err != nil {
		return nil, err
	}
	version := map[string]string{}
	for _, h := range traces.Hits.Hits {
		version[h.Source.TraceID] = h.Source.AgentVersion
	}
	type key struct{ v, n string }
	acc := map[key]*query.VersionScore{}
	sums := map[key]float64{}
	for _, b := range resp.Aggregations.Traces.Buckets {
		v, ok := version[b.Key]
		if !ok {
			continue // not this agent, or outside the range
		}
		for _, n := range b.Names.Buckets {
			k := key{v, n.Key}
			vs := acc[k]
			if vs == nil {
				vs = &query.VersionScore{Version: v, Name: n.Key}
				acc[k] = vs
			}
			vs.Count += n.Count
			vs.Passed += n.Pass.count("pass")
			vs.Failed += n.Pass.count("fail")
			vs.Valued += uint64(n.Valued.Value)
			sums[k] += n.Sum.get()
		}
	}
	out := make([]query.VersionScore, 0, len(acc))
	for k, vs := range acc {
		if vs.Valued > 0 {
			vs.Average = sums[k] / float64(vs.Valued)
		}
		out = append(out, *vs)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return out[i].Version > out[j].Version
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

type keyCount struct {
	Buckets []struct {
		Key      string `json:"key"`
		DocCount uint64 `json:"doc_count"`
	} `json:"buckets"`
}

func (k keyCount) count(key string) uint64 {
	for _, b := range k.Buckets {
		if b.Key == key {
			return b.DocCount
		}
	}
	return 0
}

// ListSessions implements query.Store: sessions whose latest run started in
// [From, To), most recent first.
func (s *Store) ListSessions(ctx context.Context, tenant string, f query.SessionFilter) ([]query.Session, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var resp struct {
		Aggregations struct {
			Sessions struct {
				Buckets []struct {
					Key      string `json:"key"`
					DocCount uint64 `json:"doc_count"`
					Failed   struct {
						DocCount uint64 `json:"doc_count"`
					} `json:"failed"`
					Cost        numValue       `json:"cost"`
					CostUnknown numValue       `json:"cost_unknown"`
					Steps       numValue       `json:"steps"`
					In          numValue       `json:"in"`
					Out         numValue       `json:"out"`
					Dur         numValue       `json:"dur"`
					First       numValue       `json:"first"`
					Last        numValue       `json:"last"`
					Agent       keyBuckets     `json:"agent"`
					Latest      hits[traceDoc] `json:"latest"`
				} `json:"buckets"`
			} `json:"sessions"`
		} `json:"aggregations"`
	}
	sum := func(f string) map[string]any { return map[string]any{"sum": map[string]any{"field": f}} }
	body := map[string]any{"size": 0, "aggs": map[string]any{"sessions": map[string]any{
		"terms": map[string]any{"field": "session_id", "size": limit, "exclude": []string{""}, "order": map[string]any{"last": "desc"}},
		"aggs": map[string]any{
			"failed": map[string]any{"filter": failedClause()}, "cost": sum("cost_usd"), "cost_unknown": sum("cost_unknown_steps"),
			"steps": sum("steps"), "in": sum("input_tokens"), "out": sum("output_tokens"), "dur": sum("root_duration_ms"),
			"first": map[string]any{"min": map[string]any{"field": "first_start_ns"}}, "last": map[string]any{"max": map[string]any{"field": "first_start_ns"}},
			"agent":  map[string]any{"terms": map[string]any{"field": "agent_name", "size": 1, "exclude": []string{""}}},
			"latest": map[string]any{"top_hits": map[string]any{"size": 1, "sort": []any{map[string]any{"first_start_ns": "desc"}}, "_source": []string{"failed_steps", "root_status"}}},
		},
	}}}
	clauses := []map[string]any{rangeNs("first_start_ns", f.From, f.To)}
	if f.Agent != "" {
		clauses = append(clauses, term("agent_name", f.Agent))
	}
	if err := s.search(ctx, tenant, s.Names.Traces(), clauses, body, &resp); err != nil {
		return nil, err
	}
	out := []query.Session{}
	for _, b := range resp.Aggregations.Sessions.Buckets {
		se := query.Session{SessionID: b.Key, Agent: b.Agent.first(), Runs: b.DocCount, FailedRuns: b.Failed.DocCount,
			CostUSD: b.Cost.get(), CostPartial: b.CostUnknown.get() > 0, Steps: int64(b.Steps.get()),
			InputTokens: int64(b.In.get()), OutputTokens: int64(b.Out.get()), DurationMs: b.Dur.get(),
			FirstStart: nsTime(int64(b.First.get())), LastStart: nsTime(int64(b.Last.get()))}
		if h := b.Latest.Hits.Hits; len(h) > 0 {
			se.LastFailed = h[0].Source.FailedSteps > 0 || h[0].Source.RootStatus == "failure"
		}
		out = append(out, se)
	}
	return out, nil
}

// feedbackAgg aggregates bindle.feedback() eval steps per trace.
type feedbackAgg struct {
	Aggregations struct {
		Traces struct {
			Buckets []struct {
				Key   string   `json:"key"`
				Count uint64   `json:"doc_count"`
				Pass  keyCount `json:"pass"`
				Avg   numValue `json:"avg"`
			} `json:"buckets"`
		} `json:"traces"`
	} `json:"aggregations"`
}

func feedbackBody(size int) map[string]any {
	return map[string]any{"size": 0, "aggs": map[string]any{"traces": map[string]any{
		"terms": map[string]any{"field": "trace_id", "size": size},
		"aggs": map[string]any{
			"pass": map[string]any{"terms": map[string]any{"field": "score_pass", "size": 2}},
			"avg":  map[string]any{"avg": map[string]any{"field": "score_value"}},
		},
	}}}
}

func (a feedbackAgg) summaries() map[string]query.FeedbackSummary {
	out := map[string]query.FeedbackSummary{}
	for _, b := range a.Aggregations.Traces.Buckets {
		fs := query.FeedbackSummary{Count: b.Count, Positive: b.Pass.count("pass"), Negative: b.Pass.count("fail")}
		if b.Avg.Value != nil {
			v := *b.Avg.Value
			fs.Average = &v
		}
		out[b.Key] = fs
	}
	return out
}

// Feedback implements query.Store.
func (s *Store) Feedback(ctx context.Context, tenant string, traceIDs []string) (map[string]query.FeedbackSummary, error) {
	if len(traceIDs) == 0 {
		return map[string]query.FeedbackSummary{}, nil
	}
	if len(traceIDs) > 1000 {
		traceIDs = traceIDs[:1000]
	}
	var resp feedbackAgg
	clauses := []map[string]any{term("score_source", "user"), {"terms": map[string]any{"trace_id": traceIDs}}}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, feedbackBody(len(traceIDs)), &resp); err != nil {
		return nil, err
	}
	return resp.summaries(), nil
}

// RecentFeedback implements query.Store.
func (s *Store) RecentFeedback(ctx context.Context, tenant string, from, to time.Time) (map[string]query.FeedbackSummary, error) {
	var resp feedbackAgg
	clauses := []map[string]any{term("score_source", "user"), rangeNs("start_ns", from, to)}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, feedbackBody(1000), &resp); err != nil {
		return nil, err
	}
	return resp.summaries(), nil
}

// RunsByIDs implements query.Store.
func (s *Store) RunsByIDs(ctx context.Context, tenant string, traceIDs []string) ([]query.Run, error) {
	if len(traceIDs) == 0 {
		return []query.Run{}, nil
	}
	if len(traceIDs) > 1000 {
		traceIDs = traceIDs[:1000]
	}
	var resp hits[traceDoc]
	if err := s.search(ctx, tenant, s.Names.Traces(), []map[string]any{{"terms": map[string]any{"trace_id": traceIDs}}}, map[string]any{"size": len(traceIDs)}, &resp); err != nil {
		return nil, err
	}
	out := make([]query.Run, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		out = append(out, h.Source.run())
	}
	return out, nil
}

// Highlight markers: Unicode private-use characters, which never appear in
// normal text, so no HTML is involved (the console splits on them).
const hlPre, hlPost = "\ue000", "\ue001"

// Search implements query.Store: one hit per run (its best-matching step),
// best match first. Offloaded fields are searched in their inline preview.
func (s *Store) Search(ctx context.Context, tenant string, f query.SearchFilter) ([]query.SearchHit, error) {
	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var resp struct {
		Hits struct {
			Hits []struct {
				Score     float64             `json:"_score"`
				Source    stepDoc             `json:"_source"`
				Highlight map[string][]string `json:"highlight"`
			} `json:"hits"`
		} `json:"hits"`
	}
	fields := []string{"input", "output", "error_message", "reasoning"}
	match := map[string]any{"simple_query_string": map[string]any{
		"query": f.Query, "fields": []string{"input", "output", "error_message^2", "reasoning", "name^2", "tool_name^2"},
		"default_operator": "and", "lenient": true,
	}}
	clauses := []map[string]any{rangeNs("start_ns", f.From, f.To)}
	if f.Agent != "" {
		clauses = append(clauses, term("agent_name", f.Agent))
	}
	if f.Type != "" {
		clauses = append(clauses, term("type", f.Type))
	}
	if f.Status == "failure" {
		clauses = append(clauses, term("status", "failure"))
	}
	hl := map[string]any{}
	for _, fl := range fields {
		hl[fl] = map[string]any{"fragment_size": 180, "number_of_fragments": 1, "no_match_size": 0}
	}
	body := map[string]any{
		"size":      limit,
		"_source":   []string{"trace_id", "step_id", "name", "type", "status", "agent_name", "agent_version", "session_id", "start_ns"},
		"collapse":  map[string]any{"field": "trace_id"},
		"sort":      []any{"_score", map[string]any{"start_ns": "desc"}},
		"highlight": map[string]any{"pre_tags": []string{hlPre}, "post_tags": []string{hlPost}, "encoder": "default", "fields": hl},
	}
	// Filters don't score; the text match does.
	if err := s.searchScored(ctx, tenant, s.Names.StepsPattern(), clauses, []map[string]any{match}, body, &resp); err != nil {
		return nil, err
	}
	out := []query.SearchHit{}
	for _, h := range resp.Hits.Hits {
		d := h.Source
		hit := query.SearchHit{TraceID: d.TraceID, StepID: d.StepID, StepName: d.Name, StepType: d.Type, Status: d.Status,
			Agent: d.AgentName, AgentVersion: d.AgentVersion, SessionID: d.SessionID, Start: nsTime(d.StartNs), Score: h.Score}
		for _, fl := range []struct{ field, label string }{{"error_message", "error"}, {"output", "output"}, {"input", "input"}, {"reasoning", "reasoning"}} {
			if frags := h.Highlight[fl.field]; len(frags) > 0 {
				hit.Field, hit.Snippet = fl.label, frags[0]
				break
			}
		}
		out = append(out, hit)
	}
	return out, nil
}

// searchScored is search with a scored "must" part: filter clauses (always
// including the tenant) don't score, must clauses do.
func (s *Store) searchScored(ctx context.Context, tenant, index string, filters, must []map[string]any, body map[string]any, out any) error {
	if tenant == "" {
		return query.ErrNoTenant
	}
	req := map[string]any{"track_total_hits": false}
	for k, v := range body {
		req[k] = v
	}
	q := tenantFilter(tenant, filters...)
	mustAny := make([]any, 0, len(must))
	for _, m := range must {
		mustAny = append(mustAny, m)
	}
	q["bool"].(map[string]any)["must"] = mustAny
	req["query"] = q
	params := url.Values{"routing": {tenant}, "ignore_unavailable": {"true"}, "allow_no_indices": {"true"}}
	return s.Client.Do(ctx, http.MethodPost, "/"+index+"/_search", params, req, out)
}

// ErrorPatterns returns failure counts per error pattern in [from, to)
// (at most 500 patterns), with the latest example.
func (s *Store) ErrorPatterns(ctx context.Context, tenant string, from, to time.Time) ([]query.ErrorPatternCount, error) {
	var resp struct {
		Aggregations struct {
			Patterns struct {
				Buckets []struct {
					Key      string     `json:"key"`
					DocCount uint64     `json:"doc_count"`
					Sample   sampleHits `json:"sample"`
					Name     keyBuckets `json:"name"`
				} `json:"buckets"`
			} `json:"patterns"`
		} `json:"aggregations"`
	}
	body := map[string]any{"size": 0, "aggs": map[string]any{"patterns": map[string]any{
		"terms": map[string]any{"field": "error_pattern", "size": 500, "exclude": []string{""}},
		"aggs":  map[string]any{"sample": latestSample(), "name": map[string]any{"terms": map[string]any{"field": "name", "size": 1}}},
	}}}
	clauses := []map[string]any{term("status", "failure"), rangeNs("start_ns", from, to)}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &resp); err != nil {
		return nil, err
	}
	out := []query.ErrorPatternCount{}
	for _, b := range resp.Aggregations.Patterns.Buckets {
		e := query.ErrorPatternCount{Pattern: b.Key, Failures: b.DocCount, StepName: b.Name.first()}
		if h := b.Sample.Hits.Hits; len(h) > 0 {
			e.SampleTrace, e.SampleStep = h[0].Source.TraceID, h[0].Source.StepID
		}
		out = append(out, e)
	}
	return out, nil
}
