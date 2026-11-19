package search

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

type req struct {
	Method, Path, Query, User string
	Body                      []byte
}

// fakeOS records requests and answers by path suffix.
type fakeOS struct {
	mu      sync.Mutex
	reqs    []req
	answers map[string]string // path suffix -> JSON body
	status  int
}

func newFake(t *testing.T) (*fakeOS, *Client) {
	f := &fakeOS{answers: map[string]string{}, status: 200}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u, _, _ := r.BasicAuth()
		f.mu.Lock()
		f.reqs = append(f.reqs, req{r.Method, r.URL.Path, r.URL.RawQuery, u, b})
		status, answers := f.status, f.answers
		f.mu.Unlock()
		if status != 200 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"type":"index_not_found_exception","reason":"no such index [bindle-secret]"},"status":404}`)
			return
		}
		for suffix, body := range answers {
			if strings.HasSuffix(r.URL.Path, suffix) {
				_, _ = io.WriteString(w, body)
				return
			}
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return f, &Client{URL: srv.URL, User: "reader", Password: "pw"}
}

func (f *fakeOS) all() []req {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]req(nil), f.reqs...)
}

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func TestEverySearchIsTenantScoped(t *testing.T) {
	f, c := newFake(t)
	s := &Store{Client: c}
	ctx := context.Background()
	evil := `x"},{"match_all":{}}`
	_, _ = s.ListRuns(ctx, "tenant-1", query.RunFilter{From: t0, To: t0.Add(time.Hour), Limit: 5, Agent: evil})
	_, _ = s.GetTrace(ctx, "tenant-1", strings.Repeat("a", 32), 10)
	_, _ = s.GetStep(ctx, "tenant-1", strings.Repeat("a", 32), strings.Repeat("b", 16))
	_, _ = s.Overview(ctx, "tenant-1", t0, t0.Add(time.Hour), time.Hour)
	_, _ = s.Loops(ctx, "tenant-1", t0, t0.Add(time.Hour), 5)
	_, _ = s.CostlyRuns(ctx, "tenant-1", t0, t0.Add(time.Hour), 2.5, 10)
	_, _ = s.Insights(ctx, "tenant-1", t0, t0.Add(time.Hour))

	reqs := f.all()
	if len(reqs) < 7 {
		t.Fatalf("only %d requests", len(reqs))
	}
	for _, r := range reqs {
		if !strings.HasSuffix(r.Path, "/_search") || !strings.Contains(r.Query, "routing=tenant-1") || r.User != "reader" {
			t.Errorf("request %s %s?%s user=%s", r.Method, r.Path, r.Query, r.User)
		}
		var body struct {
			Query struct {
				Bool struct {
					Filter []map[string]any `json:"filter"`
				} `json:"bool"`
			} `json:"query"`
		}
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		first, _ := json.Marshal(body.Query.Bool.Filter[0])
		if string(first) != `{"term":{"tenant_id":"tenant-1"}}` {
			t.Errorf("first filter is %s, want the tenant term", first)
		}
	}
	// Client values are JSON string values, never structure.
	if !bytes.Contains(reqs[0].Body, []byte(`"agent_name":"x\"},{\"match_all\":{}}"`)) {
		t.Errorf("agent value not encoded as a plain string: %s", reqs[0].Body)
	}
}

func TestEmptyTenantSendsNothing(t *testing.T) {
	f, c := newFake(t)
	s := &Store{Client: c}
	ctx := context.Background()
	errs := []error{
		func() error { _, err := s.ListRuns(ctx, "", query.RunFilter{}); return err }(),
		func() error { _, err := s.GetTrace(ctx, "", "x", 1); return err }(),
		func() error { _, err := s.GetStep(ctx, "", "x", "y"); return err }(),
		func() error { _, err := s.Overview(ctx, "", t0, t0, time.Hour); return err }(),
		func() error { _, err := s.Loops(ctx, "", t0, t0, 1); return err }(),
		func() error { _, err := s.CostlyRuns(ctx, "", t0, t0, 1, 1); return err }(),
		func() error { _, err := s.Insights(ctx, "", t0, t0); return err }(),
	}
	for i, err := range errs {
		if !errors.Is(err, query.ErrNoTenant) {
			t.Errorf("call %d: %v", i, err)
		}
	}
	if n := len(f.all()); n != 0 {
		t.Fatalf("%d requests sent without a tenant", n)
	}
}

func TestRunsRequestFiltersAndCursor(t *testing.T) {
	clauses, body := runsRequest(query.RunFilter{From: time.Unix(1, 0), To: time.Unix(2, 0), Limit: 7,
		Agent: "a", Environment: "e", Status: "ok", AfterStart: time.Unix(1, 5), AfterTrace: "tr"})
	got, _ := json.Marshal(map[string]any{"c": clauses, "b": body})
	for _, want := range []string{`"first_start_ns":{"gte":1000000000,"lt":2000000000}`, `"agent_name":"a"`,
		`"environment":"e"`, `"must_not"`, `"search_after":[1000000005,"tr"]`, `"size":7`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

func TestWriterBulkAndRollup(t *testing.T) {
	f, c := newFake(t)
	f.answers["/_bulk"] = `{"errors":true,"items":[{"create":{"status":201}},{"create":{"status":409,"error":{"type":"version_conflict_engine_exception","reason":"exists"}}}]}`
	f.answers["/_search"] = `{"aggregations":{"by_trace":{"buckets":[{"key":"tr","doc_count":2,
		"first":{"hits":{"hits":[{"_source":{"start_ns":1794887456850377123}}]}},
		"failed":{"doc_count":1},"cost_unknown":{"doc_count":0},
		"input_tokens":{"value":3},"output_tokens":{"value":4},"cost_usd":{"value":0},
		"agent_name":{"buckets":[{"key":"agent"}]},"agent_version":{"buckets":[]},"framework":{"buckets":[]},
		"environment":{"buckets":[]},"session_id":{"buckets":[]},
		"root":{"status":{"buckets":[{"key":"success"}]},"duration":{"value":12.5}}}]}}}`
	now := time.Unix(0, 1794887456850377123+int64(time.Minute))
	w := &Writer{Client: c, Now: func() time.Time { return now }}
	steps := []model.Step{
		{TenantID: "t1", TraceID: "tr", StepID: "s1", StartUnixNano: 1794887456850377123},
		{TenantID: "t1", TraceID: "tr", StepID: "s2", StartUnixNano: 1794887456850377124},
		{TenantID: "", TraceID: "tr", StepID: "nope", StartUnixNano: 1794887456850377124},                               // no tenant
		{TenantID: "t1", TraceID: "old", StepID: "s3", StartUnixNano: uint64(now.Add(-40 * 24 * time.Hour).UnixNano())}, // too old
	}
	if err := w.Write(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
	reqs := f.all()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d (want bulk, rollup search, rollup bulk)", len(reqs))
	}
	lines := ndjson(t, reqs[0].Body)
	if len(lines) != 4 || !strings.Contains(reqs[0].Query, "refresh=wait_for") {
		t.Fatalf("steps bulk: %d lines, query %q", len(lines), reqs[0].Query)
	}
	meta := lines[0]["create"].(map[string]any)
	if meta["_index"] != "bindle-steps-2026.11.17" || meta["routing"] != "t1" || len(meta["_id"].(string)) != 64 {
		t.Errorf("create meta = %v", meta)
	}
	if lines[1]["start_ns"].(json.Number).String() != "1794887456850377123" {
		t.Errorf("start_ns not exact: %v", lines[1]["start_ns"])
	}
	tl := ndjson(t, reqs[2].Body)
	idx := tl[0]["index"].(map[string]any)
	if idx["version"].(json.Number).String() != "2" || idx["version_type"] != "external_gte" || idx["routing"] != "t1" {
		t.Errorf("rollup meta = %v", idx)
	}
	if tl[1]["first_start_ns"].(json.Number).String() != "1794887456850377123" || tl[1]["agent_name"] != "agent" ||
		tl[1]["root_status"] != "success" || tl[1]["failed_steps"].(json.Number).String() != "1" {
		t.Errorf("rollup doc = %v", tl[1])
	}
}

func TestWriterSurfacesRealItemErrors(t *testing.T) {
	f, c := newFake(t)
	f.answers["/_bulk"] = `{"errors":true,"items":[{"create":{"status":400,"error":{"type":"mapper_parsing_exception","reason":"bad"}}}]}`
	w := &Writer{Client: c}
	err := w.Write(context.Background(), []model.Step{{TenantID: "t", TraceID: "a", StepID: "b", StartUnixNano: uint64(time.Now().UnixNano())}})
	if !IsStatus(err, 400) {
		t.Fatalf("err = %v", err)
	}
}

func TestClientErrorsAndLimits(t *testing.T) {
	f, c := newFake(t)
	f.status = 404
	err := c.Do(context.Background(), http.MethodGet, "/x", nil, nil, nil)
	var e *Error
	if !errors.As(err, &e) || e.Status != 404 || e.Type != "index_not_found_exception" {
		t.Fatalf("err = %#v", err)
	}
	f.status = 200
	f.answers["/big"] = `{"x":"` + strings.Repeat("a", 200) + `"}`
	c.MaxResponseBytes = 100
	if err := c.Do(context.Background(), http.MethodGet, "/big", nil, nil, &struct{}{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("size cap: %v", err)
	}
}

func TestRedactedKindsAndDocID(t *testing.T) {
	if got := strings.Join(redactedKinds("a [REDACTED:EMAIL] b", "[REDACTED:SECRET] [REDACTED:EMAIL]"), ","); got != "EMAIL,SECRET" {
		t.Errorf("kinds = %s", got)
	}
	if docID("a", "bc") == docID("ab", "c") {
		t.Error("docID must separate parts")
	}
}

func TestSetupRejectsBadNames(t *testing.T) {
	f, c := newFake(t)
	for _, o := range []SetupOptions{
		{Names: Names{Prefix: "Bad Prefix"}, Users: DefaultUsers},
		{Users: Users{Writer: "x", Reader: "x"}},
		{Users: Users{Writer: "../etc", Reader: "r"}},
	} {
		if err := Setup(context.Background(), c, o); err == nil {
			t.Errorf("accepted %+v", o)
		}
	}
	if len(f.all()) != 0 {
		t.Error("requests sent for invalid setup")
	}
}

func ndjson(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		d := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		d.UseNumber()
		var m map[string]any
		if err := d.Decode(&m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestFromEnvRefusesUnsafeConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		vars map[string]string
		ok   bool
	}{
		{map[string]string{"BINDLE_SEARCH_URL": "https://search.example:443"}, true},
		{map[string]string{"BINDLE_SEARCH_URL": "http://opensearch:9200"}, false},
		{map[string]string{"BINDLE_SEARCH_URL": "http://opensearch:9200", "BINDLE_SEARCH_ALLOW_HTTP": "1"}, true},
		{map[string]string{"BINDLE_SEARCH_URL": "https://u:p@search.example"}, false},
		{map[string]string{"BINDLE_SEARCH_URL": "ftp://x"}, false},
		{map[string]string{}, false},
	}
	for _, c := range cases {
		_, err := FromEnv(env(c.vars), "u", "p")
		if (err == nil) != c.ok {
			t.Errorf("%v: err=%v", c.vars, err)
		}
	}
	if _, err := FromEnv(env(map[string]string{"BINDLE_SEARCH_URL": "https://x"}), "u", ""); err == nil {
		t.Error("empty password accepted")
	}
}

func TestOutlierLimits(t *testing.T) {
	got := outlierLimits([]query.AgentSummary{
		{Agent: "big", Runs: 50, MedianSteps: 12, MedianCost: 0.02},
		{Agent: "small", Runs: 50, MedianSteps: 2},
		{Agent: "new", Runs: 3, MedianSteps: 5, MedianCost: 1},
		{Agent: "empty", Runs: 20},
	})
	if l := got["big"]; l[0] != 36 || math.Abs(l[1]-0.06) > 1e-9 {
		t.Errorf("big: %v", l)
	}
	if l := got["small"]; l[0] != 12 || l[1] != 0 { // median+10 beats 3x for tiny runs
		t.Errorf("small: %v", l)
	}
	if _, ok := got["new"]; ok {
		t.Error("agents with few runs have no reliable median")
	}
	if _, ok := got["empty"]; ok {
		t.Error("no limits without medians")
	}
}
