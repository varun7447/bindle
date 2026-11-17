package search

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/bindle-ai/bindle/services/internal/payload"
	"github.com/bindle-ai/bindle/services/internal/query"
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

var summaryExcludes = []string{"input", "output", "reasoning", "attributes", "links"}

func (d stepDoc) summary() query.StepSummary {
	return query.StepSummary{
		StepID: d.StepID, ParentStepID: d.ParentStepID, Name: d.Name, Type: d.Type, Status: d.Status, Attempt: d.Attempt,
		StartTime: nsTime(d.StartNs), DurationMs: d.DurationMs, Provider: d.Provider, Model: d.Model, ToolName: d.ToolName,
		InputTokens: d.InputTokens, OutputTokens: d.OutputTokens, CachedTokens: d.CachedTokens, CostUSD: d.CostUSD,
		CostKnown: d.CostKnown, ErrorMessage: d.ErrorMessage, InputBytes: uint64(d.InputBytes),
		OutputBytes: uint64(d.OutputBytes), ReasoningBytes: uint64(d.ReasoningBytes), Redacted: d.Redacted,
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
