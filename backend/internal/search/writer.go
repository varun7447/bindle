package search

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/payload"
)

// Writer indexes normalized steps and maintains the trace rollups. It
// implements pipeline.Sink. Authenticate it as the writer user.
type Writer struct {
	Client *Client
	Names  Names
	Log    *slog.Logger
	// Retention bounds accepted step times: older steps (whose daily index
	// may already be deleted), or ones more than a day ahead, are dropped.
	Retention time.Duration
	Now       func() time.Time
	// Payloads, if set, receives input/output/reasoning fields larger than
	// OffloadBytes; the index keeps a PreviewBytes preview and a reference.
	Payloads     payload.Store
	OffloadBytes int // default 32 KiB
	PreviewBytes int // default 4 KiB
	// MaxBulkBytes caps one bulk request; larger batches are split, so a
	// big export can't arrive in OpenSearch as one huge request (heap and
	// direct-memory pressure). Default 8 MiB.
	MaxBulkBytes int
}

const maxFutureSkew = 24 * time.Hour

// stepDoc is the indexed form of a step.
type stepDoc struct {
	TenantID        string                 `json:"tenant_id"`
	TraceID         string                 `json:"trace_id"`
	StepID          string                 `json:"step_id"`
	ParentStepID    string                 `json:"parent_step_id"`
	Name            string                 `json:"name"`
	Type            string                 `json:"type"`
	Status          string                 `json:"status"`
	Attempt         int64                  `json:"attempt"`
	Framework       string                 `json:"framework"`
	ServiceName     string                 `json:"service_name"`
	AgentName       string                 `json:"agent_name"`
	AgentVersion    string                 `json:"agent_version"`
	SessionID       string                 `json:"session_id"`
	Environment     string                 `json:"environment"`
	Provider        string                 `json:"provider"`
	Model           string                 `json:"model"`
	ToolName        string                 `json:"tool_name"`
	InputTokens     int64                  `json:"input_tokens"`
	OutputTokens    int64                  `json:"output_tokens"`
	CachedTokens    int64                  `json:"cached_tokens"`
	CostUSD         float64                `json:"cost_usd"`
	CostKnown       bool                   `json:"cost_known"`
	StartTime       string                 `json:"start_time"`
	StartNs         int64                  `json:"start_ns"`
	DurationMs      float64                `json:"duration_ms"`
	ErrorMessage    string                 `json:"error_message"`
	ErrorPattern    string                 `json:"error_pattern"`
	ScoreName       string                 `json:"score_name,omitempty"`
	ScoreValue      *float64               `json:"score_value,omitempty"`
	ScorePass       string                 `json:"score_pass,omitempty"`
	ScoreSource     string                 `json:"score_source,omitempty"`
	Input           string                 `json:"input"`
	Output          string                 `json:"output"`
	Reasoning       string                 `json:"reasoning"`
	UserInput       string                 `json:"user_input,omitempty"`
	UserInputBytes  int                    `json:"user_input_bytes,omitempty"`
	SecurityFlags   []string               `json:"security_flags,omitempty"`
	SecurityReasons []string               `json:"security_reasons,omitempty"`
	DataClasses     []string               `json:"data_classes,omitempty"`
	PassportDrift   []string               `json:"passport_drift,omitempty"`
	Attributes      map[string]string      `json:"attributes"`
	Links           []string               `json:"links"`
	Correlations    []string               `json:"correlations,omitempty"`
	SkillsUsed      []string               `json:"skills_used,omitempty"`
	InputHash       string                 `json:"input_hash"`
	InputBytes      int                    `json:"input_bytes"`
	OutputBytes     int                    `json:"output_bytes"`
	ReasoningBytes  int                    `json:"reasoning_bytes"`
	Redacted        bool                   `json:"redacted"`
	RedactedKinds   []string               `json:"redacted_kinds"`
	IngestedAt      string                 `json:"ingested_at"`
	PayloadRefs     map[string]payload.Ref `json:"payload_refs,omitempty"`
}

// traceDoc is the per-trace rollup.
type traceDoc struct {
	TenantID         string  `json:"tenant_id"`
	TraceID          string  `json:"trace_id"`
	FirstStart       string  `json:"first_start"`
	FirstStartNs     int64   `json:"first_start_ns"`
	Steps            int64   `json:"steps"`
	FailedSteps      int64   `json:"failed_steps"`
	CostUnknownSteps int64   `json:"cost_unknown_steps"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	LLMCalls         int64   `json:"llm_calls"`
	ToolCalls        int64   `json:"tool_calls"`
	PeakInputTokens  int64   `json:"peak_input_tokens"` // largest single LLM call: the run's peak context
	CostUSD          float64 `json:"cost_usd"`
	AgentName        string  `json:"agent_name"`
	AgentVersion     string  `json:"agent_version"`
	Framework        string  `json:"framework"`
	Environment      string  `json:"environment"`
	SessionID        string  `json:"session_id"`
	RootStatus       string  `json:"root_status"`
	RootDurationMs   float64 `json:"root_duration_ms"`
	UpdatedAt        string  `json:"updated_at"`
}

// docID is a fixed-length, collision-resistant ID; the same step always
// maps to the same document, which makes re-sent batches no-ops.
func docID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(h[:])
}

func nanoTime(ns int64) string { return time.Unix(0, ns).UTC().Format(time.RFC3339Nano) }

var redactedRe = redactedMarker()

func toDoc(s model.Step, ingested string) stepDoc {
	attrs := s.Attributes
	if attrs == nil {
		attrs = map[string]string{}
	}
	links := s.Links
	if links == nil {
		links = []string{}
	}
	kinds := redactedKinds(s.Input, s.Output, s.Reasoning, s.ErrorMessage)
	ih := sha256.Sum256([]byte(s.Input))
	return stepDoc{
		TenantID: s.TenantID, TraceID: s.TraceID, StepID: s.StepID, ParentStepID: s.ParentStepID,
		Name: s.Name, Type: s.Type, Status: s.Status, Attempt: s.Attempt,
		Framework: s.Framework, ServiceName: s.ServiceName, AgentName: s.AgentName, AgentVersion: s.AgentVersion,
		SessionID: s.SessionID, Environment: s.Environment, Provider: s.Provider, Model: s.Model, ToolName: s.ToolName,
		InputTokens: s.InputTokens, OutputTokens: s.OutputTokens, CachedTokens: s.CachedTokens,
		CostUSD: s.CostUSD, CostKnown: s.CostKnown,
		StartTime: nanoTime(int64(s.StartUnixNano)), StartNs: int64(s.StartUnixNano), DurationMs: s.DurationMs,
		ErrorMessage: s.ErrorMessage, ErrorPattern: s.ErrorPattern,
		ScoreName: s.ScoreName, ScoreValue: s.ScoreValue, ScorePass: s.ScorePass, ScoreSource: s.ScoreSource, Input: s.Input, Output: s.Output, Reasoning: s.Reasoning,
		UserInput: s.UserInput, UserInputBytes: len(s.UserInput), SecurityFlags: s.SecurityFlags, SecurityReasons: s.SecurityReasons, DataClasses: s.DataClasses, PassportDrift: s.PassportDrift,
		Attributes: attrs, Links: links, Correlations: s.Correlations, SkillsUsed: s.SkillsUsed, InputHash: hex.EncodeToString(ih[:16]),
		InputBytes: len(s.Input), OutputBytes: len(s.Output), ReasoningBytes: len(s.Reasoning),
		Redacted: len(kinds) > 0, RedactedKinds: kinds, IngestedAt: ingested,
	}
}

// Write implements pipeline.Sink. Steps are created (never overwritten),
// so retrying a batch is safe; then each touched trace's rollup is
// recomputed from its steps and stored with its step count as an external
// version, so a stale recompute can never overwrite a newer one.
func (w *Writer) Write(ctx context.Context, steps []model.Step) error {
	now := w.now()
	oldest, newest := now.Add(-w.retention()), now.Add(maxFutureSkew)
	ingested := now.Format(time.RFC3339Nano)

	var bulk bytes.Buffer
	enc := json.NewEncoder(&bulk)
	touched := map[string]map[string]bool{} // tenant -> trace IDs
	dropped := 0
	docs := make([]stepDoc, 0, len(steps))
	days := make([]string, 0, len(steps))
	for _, s := range steps {
		if s.TenantID == "" {
			continue // never write a step without a tenant
		}
		t := time.Unix(0, int64(s.StartUnixNano)).UTC()
		if t.Before(oldest) || t.After(newest) {
			dropped++
			continue
		}
		docs = append(docs, toDoc(s, ingested))
		days = append(days, t.Format("2006.01.02"))
	}
	if err := w.offload(ctx, docs, days); err != nil {
		return err
	}
	maxBulk := w.MaxBulkBytes
	if maxBulk <= 0 {
		maxBulk = 8 << 20
	}
	var chunks [][]byte
	for i, d := range docs {
		if bulk.Len() >= maxBulk {
			chunks = append(chunks, append([]byte(nil), bulk.Bytes()...))
			bulk.Reset()
		}
		meta := map[string]any{"create": map[string]any{
			"_index":  w.Names.StepsIndex(days[i]),
			"_id":     docID(d.TenantID, d.TraceID, d.StepID),
			"routing": d.TenantID,
		}}
		if err := enc.Encode(meta); err != nil {
			return err
		}
		if err := enc.Encode(d); err != nil {
			return err
		}
		if touched[d.TenantID] == nil {
			touched[d.TenantID] = map[string]bool{}
		}
		touched[d.TenantID][d.TraceID] = true
	}
	if dropped > 0 {
		w.logger().Warn("dropping steps outside the accepted time window", "dropped", dropped, "retention", w.retention().String())
	}
	if bulk.Len() > 0 {
		chunks = append(chunks, bulk.Bytes())
	}
	if len(chunks) == 0 {
		return nil
	}
	for i, c := range chunks {
		q := url.Values{}
		if i == len(chunks)-1 {
			// wait_for on the last request: the refresh it waits for also
			// covers the earlier ones, and the rollup below must see them all.
			q.Set("refresh", "wait_for")
		}
		if err := w.bulk(ctx, c, q, http.StatusConflict); err != nil {
			return err
		}
	}

	tenants := make([]string, 0, len(touched))
	for t := range touched {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	for _, tenant := range tenants {
		if err := w.rollup(ctx, tenant, keys(touched[tenant])); err != nil {
			return fmt.Errorf("search: rollup for tenant %s: %w", tenant, err)
		}
	}
	return nil
}

// bulk sends an NDJSON bulk request. Item failures with a status in ok are
// expected (duplicate creates, stale versions); any other failure is an
// error so the batcher retries the whole batch.
func (w *Writer) bulk(ctx context.Context, body []byte, q url.Values, ok ...int) error {
	var resp struct {
		Errors bool `json:"errors"`
		Items  []map[string]struct {
			Status int `json:"status"`
			Error  *struct {
				Type   string `json:"type"`
				Reason string `json:"reason"`
			} `json:"error"`
		} `json:"items"`
	}
	if err := w.Client.Do(ctx, http.MethodPost, "/_bulk", q, body, &resp); err != nil {
		return err
	}
	if !resp.Errors {
		return nil
	}
	for _, item := range resp.Items {
		for _, r := range item {
			if r.Error == nil || contains(ok, r.Status) {
				continue
			}
			return &Error{Status: r.Status, Type: r.Error.Type, Reason: r.Error.Reason}
		}
	}
	return nil
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// rollupAggs computes each trace's summary from its steps in one request.
func rollupAggs(traces []string) map[string]any {
	maxKw := func(field string, exclude ...string) map[string]any {
		t := map[string]any{"field": field, "size": 1, "order": map[string]any{"_key": "desc"}}
		if len(exclude) > 0 {
			t["exclude"] = exclude
		}
		return map[string]any{"terms": t}
	}
	return map[string]any{
		"by_trace": map[string]any{
			"terms": map[string]any{"field": "trace_id", "size": len(traces)},
			"aggs": map[string]any{
				// top_hits keeps the exact 64-bit start; numeric min would round.
				"first":         map[string]any{"top_hits": map[string]any{"size": 1, "sort": []any{map[string]any{"start_ns": "asc"}}, "_source": []string{"start_ns"}}},
				"failed":        map[string]any{"filter": map[string]any{"term": map[string]any{"status": "failure"}}},
				"cost_unknown":  map[string]any{"filter": costUnknownFilter()},
				"input_tokens":  map[string]any{"sum": map[string]any{"field": "input_tokens"}},
				"output_tokens": map[string]any{"sum": map[string]any{"field": "output_tokens"}},
				"cost_usd":      map[string]any{"sum": map[string]any{"field": "cost_usd"}},
				"cached_tokens": map[string]any{"sum": map[string]any{"field": "cached_tokens"}},
				"llm":           map[string]any{"filter": map[string]any{"term": map[string]any{"type": "llm_call"}}, "aggs": map[string]any{"peak": map[string]any{"max": map[string]any{"field": "input_tokens"}}}},
				"tools":         map[string]any{"filter": map[string]any{"term": map[string]any{"type": "tool_call"}}},
				"agent_name":    maxKw("agent_name", ""),
				"agent_version": maxKw("agent_version", ""),
				"framework":     maxKw("framework", "", "unknown"),
				"environment":   maxKw("environment", ""),
				"session_id":    maxKw("session_id", ""),
				"root": map[string]any{
					"filter": map[string]any{"term": map[string]any{"parent_step_id": ""}},
					"aggs": map[string]any{
						"status":   maxKw("status"),
						"duration": map[string]any{"max": map[string]any{"field": "duration_ms"}},
					},
				},
			},
		},
	}
}

// costUnknownFilter: model-using steps without a known price.
func costUnknownFilter() map[string]any {
	return map[string]any{"bool": map[string]any{
		"filter": []any{map[string]any{"term": map[string]any{"cost_known": false}}},
		"should": []any{
			map[string]any{"bool": map[string]any{"must_not": []any{map[string]any{"term": map[string]any{"model": ""}}}}},
			map[string]any{"term": map[string]any{"type": "llm_call"}},
		},
		"minimum_should_match": 1,
	}}
}

type keyBuckets struct {
	Buckets []struct {
		Key json.RawMessage `json:"key"`
	} `json:"buckets"`
}

func (k keyBuckets) first() string {
	if len(k.Buckets) == 0 {
		return ""
	}
	var s string
	_ = json.Unmarshal(k.Buckets[0].Key, &s)
	return s
}

type numValue struct {
	Value *float64 `json:"value"`
}

func (n numValue) get() float64 {
	if n.Value == nil {
		return 0
	}
	return *n.Value
}

func (w *Writer) rollup(ctx context.Context, tenant string, traces []string) error {
	var resp struct {
		Aggregations struct {
			ByTrace struct {
				Buckets []struct {
					Key      string `json:"key"`
					DocCount int64  `json:"doc_count"`
					First    struct {
						Hits struct {
							Hits []struct {
								Source struct {
									StartNs json.Number `json:"start_ns"`
								} `json:"_source"`
							} `json:"hits"`
						} `json:"hits"`
					} `json:"first"`
					Failed struct {
						DocCount int64 `json:"doc_count"`
					} `json:"failed"`
					CostUnknown struct {
						DocCount int64 `json:"doc_count"`
					} `json:"cost_unknown"`
					InputTokens  numValue `json:"input_tokens"`
					OutputTokens numValue `json:"output_tokens"`
					CostUSD      numValue `json:"cost_usd"`
					CachedTokens numValue `json:"cached_tokens"`
					LLM          struct {
						DocCount int64    `json:"doc_count"`
						Peak     numValue `json:"peak"`
					} `json:"llm"`
					Tools struct {
						DocCount int64 `json:"doc_count"`
					} `json:"tools"`
					AgentName    keyBuckets `json:"agent_name"`
					AgentVersion keyBuckets `json:"agent_version"`
					Framework    keyBuckets `json:"framework"`
					Environment  keyBuckets `json:"environment"`
					SessionID    keyBuckets `json:"session_id"`
					Root         struct {
						Status   keyBuckets `json:"status"`
						Duration numValue   `json:"duration"`
					} `json:"root"`
				} `json:"buckets"`
			} `json:"by_trace"`
		} `json:"aggregations"`
	}
	q := tenantFilter(tenant, map[string]any{"terms": map[string]any{"trace_id": traces}})
	body := map[string]any{"size": 0, "query": q, "aggs": rollupAggs(traces)}
	if err := w.Client.Do(ctx, http.MethodPost, "/"+w.Names.StepsPattern()+"/_search",
		url.Values{"routing": {tenant}, "ignore_unavailable": {"true"}, "allow_no_indices": {"true"}}, body, &resp); err != nil {
		return err
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	updated := w.now().Format(time.RFC3339Nano)
	for _, b := range resp.Aggregations.ByTrace.Buckets {
		var first int64
		if hits := b.First.Hits.Hits; len(hits) > 0 {
			first, _ = strconv.ParseInt(hits[0].Source.StartNs.String(), 10, 64)
		}
		doc := traceDoc{
			TenantID: tenant, TraceID: b.Key, FirstStart: nanoTime(first), FirstStartNs: first,
			Steps: b.DocCount, FailedSteps: b.Failed.DocCount, CostUnknownSteps: b.CostUnknown.DocCount,
			InputTokens: int64(b.InputTokens.get()), OutputTokens: int64(b.OutputTokens.get()), CostUSD: b.CostUSD.get(),
			CachedTokens: int64(b.CachedTokens.get()), LLMCalls: b.LLM.DocCount, ToolCalls: b.Tools.DocCount, PeakInputTokens: int64(b.LLM.Peak.get()),
			AgentName: b.AgentName.first(), AgentVersion: b.AgentVersion.first(), Framework: b.Framework.first(),
			Environment: b.Environment.first(), SessionID: b.SessionID.first(),
			RootStatus: b.Root.Status.first(), RootDurationMs: b.Root.Duration.get(), UpdatedAt: updated,
		}
		meta := map[string]any{"index": map[string]any{
			"_index": w.Names.Traces(), "_id": docID(tenant, b.Key), "routing": tenant,
			// Steps are only ever added, so a larger count is always newer.
			"version": b.DocCount, "version_type": "external_gte",
		}}
		if err := enc.Encode(meta); err != nil {
			return err
		}
		if err := enc.Encode(doc); err != nil {
			return err
		}
	}
	if out.Len() == 0 {
		return nil
	}
	// wait_for: a run is readable as soon as Write returns.
	return w.bulk(ctx, out.Bytes(), url.Values{"refresh": {"wait_for"}}, http.StatusConflict)
}

// ExpireRollups deletes rollups of traces that started before the
// retention period. (Step indices expire via the ISM policy.)
func (w *Writer) ExpireRollups(ctx context.Context) (int64, error) {
	cutoff := w.now().Add(-w.retention()).UnixNano()
	body := map[string]any{"query": map[string]any{"range": map[string]any{"first_start_ns": map[string]any{"lt": cutoff}}}}
	var resp struct {
		Deleted int64 `json:"deleted"`
	}
	err := w.Client.Do(ctx, http.MethodPost, "/"+w.Names.Traces()+"/_delete_by_query", url.Values{"conflicts": {"proceed"}}, body, &resp)
	return resp.Deleted, err
}

// Maintain runs ExpireRollups now and then every interval until ctx ends.
func (w *Writer) Maintain(ctx context.Context, interval time.Duration) {
	run := func() {
		if n, err := w.ExpireRollups(ctx); err != nil {
			if ctx.Err() == nil {
				w.logger().Error("trace rollup retention failed", "err", err)
			}
		} else if n > 0 {
			w.logger().Info("deleted expired trace rollups", "count", n)
		}
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

func (w *Writer) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *Writer) retention() time.Duration {
	if w.Retention > 0 {
		return w.Retention
	}
	return 30 * 24 * time.Hour
}

func (w *Writer) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// offload moves oversized fields to the payload store (in parallel) and
// leaves a preview and reference in the document. Keys are deterministic,
// so retrying a batch overwrites the same objects.
func (w *Writer) offload(ctx context.Context, docs []stepDoc, days []string) error {
	if w.Payloads == nil {
		return nil
	}
	limit, preview := w.OffloadBytes, w.PreviewBytes
	if limit <= 0 {
		limit = 32 << 10
	}
	if preview <= 0 {
		preview = 4 << 10
	}
	type job struct{ key, data string }
	var jobs []job
	for i := range docs {
		d := &docs[i]
		for _, f := range []struct {
			name string
			val  *string
		}{{"input", &d.Input}, {"output", &d.Output}, {"reasoning", &d.Reasoning}} {
			if len(*f.val) <= limit {
				continue
			}
			key, err := payload.Key(d.TenantID, days[i], d.TraceID, d.StepID, f.name)
			if err != nil {
				continue // unusual IDs: keep the field inline
			}
			jobs = append(jobs, job{key, *f.val})
			if d.PayloadRefs == nil {
				d.PayloadRefs = map[string]payload.Ref{}
			}
			d.PayloadRefs[f.name] = payload.Ref{Key: key, Bytes: int64(len(*f.val)), SHA256: payload.Checksum([]byte(*f.val))}
			*f.val = utf8Prefix(*f.val, preview)
		}
	}
	if len(jobs) == 0 {
		return nil
	}
	sem := make(chan struct{}, 8)
	errs := make(chan error, len(jobs))
	for _, j := range jobs {
		sem <- struct{}{}
		go func(j job) {
			defer func() { <-sem }()
			errs <- w.Payloads.Put(ctx, j.key, []byte(j.data))
		}(j)
	}
	var first error
	for range jobs {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return fmt.Errorf("search: payload offload: %w", first)
	}
	return nil
}

// utf8Prefix cuts s to at most n bytes without splitting a character.
func utf8Prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
