package query

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCH is a stand-in ClickHouse HTTP server. It records every request and
// answers with canned JSONEachRow bodies chosen by a substring of the SQL.
type fakeCH struct {
	mu      sync.Mutex
	reqs    []fakeReq
	answers map[string]string // SQL substring -> response body
	status  int
}

type fakeReq struct {
	SQL    string
	Params url.Values
	User   string
}

func newFakeCH(t *testing.T) (*fakeCH, *Client) {
	f := &fakeCH{answers: map[string]string{}, status: 200}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, fakeReq{SQL: string(b), Params: r.URL.Query(), User: r.Header.Get("X-ClickHouse-User")})
		status, answers := f.status, f.answers
		f.mu.Unlock()
		if status != 200 {
			w.Header().Set("X-ClickHouse-Exception-Code", "60")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "Code: 60. DB::Exception: Table bindle.secret_internal_name does not exist")
			return
		}
		for k, v := range answers {
			if strings.Contains(string(b), k) {
				_, _ = io.WriteString(w, v)
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return f, &Client{URL: srv.URL, User: "reader", Password: "pw"}
}

func (f *fakeCH) all() []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeReq(nil), f.reqs...)
}

var (
	t0   = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	ctx0 = context.Background()
)

func TestEveryQueryBindsTenantAndIsReadOnly(t *testing.T) {
	f, c := newFakeCH(t)
	_, _ = c.ListRuns(ctx0, "tenant-1", RunFilter{From: t0, To: t0.Add(time.Hour), Limit: 5})
	_, _ = c.GetTrace(ctx0, "tenant-1", strings.Repeat("a", 32), 10)
	_, _ = c.GetStep(ctx0, "tenant-1", strings.Repeat("a", 32), strings.Repeat("b", 16))
	_, _ = c.Overview(ctx0, "tenant-1", t0, t0.Add(time.Hour), time.Hour)
	_, _ = c.Loops(ctx0, "tenant-1", t0, t0.Add(time.Hour), 5)

	reqs := f.all()
	if len(reqs) < 6 {
		t.Fatalf("expected at least 6 queries, got %d", len(reqs))
	}
	for _, r := range reqs {
		if got := r.Params.Get("param_tenant"); got != "tenant-1" {
			t.Errorf("param_tenant = %q in %s", got, r.SQL)
		}
		if !strings.Contains(r.SQL, "tenant_id = {tenant:String}") {
			t.Errorf("query not tenant-scoped: %s", r.SQL)
		}
		if r.Params.Get("readonly") != "1" || r.Params.Get("result_overflow_mode") != "throw" ||
			r.Params.Get("max_execution_time") == "" || r.Params.Get("max_result_rows") == "" {
			t.Errorf("missing safety settings: %v", r.Params)
		}
		if r.User != "reader" {
			t.Errorf("user header = %q", r.User)
		}
		if !strings.HasSuffix(r.SQL, "FORMAT JSONEachRow") {
			t.Errorf("format: %s", r.SQL)
		}
	}
}

func TestClientValuesNeverInSQL(t *testing.T) {
	f, c := newFakeCH(t)
	evil := "x' OR 1=1 --"
	_, _ = c.ListRuns(ctx0, "t", RunFilter{
		From: t0, To: t0.Add(time.Hour), Limit: 5,
		Agent: evil, Environment: evil + "env", Status: "failed",
		AfterStart: t0, AfterTrace: evil + "trace",
	})
	r := f.all()[0]
	if strings.Contains(r.SQL, evil) {
		t.Fatalf("client value spliced into SQL: %s", r.SQL)
	}
	for k, want := range map[string]string{
		"param_agent": evil, "param_env": evil + "env", "param_after_trace": evil + "trace",
		"param_from": "2026-11-16 12:00:00.000000000", "param_limit": "5",
	} {
		if got := r.Params.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, frag := range []string{havingAgent, havingEnv, havingFailed, havingCursor} {
		if !strings.Contains(r.SQL, frag) {
			t.Errorf("missing fragment %q", frag)
		}
	}
}

func TestListRunsOmitsUnusedFilters(t *testing.T) {
	f, c := newFakeCH(t)
	_, _ = c.ListRuns(ctx0, "t", RunFilter{From: t0, To: t0.Add(time.Hour), Limit: 5, Status: "ok"})
	r := f.all()[0]
	for _, frag := range []string{havingAgent, havingEnv, havingFailed, havingCursor} {
		if strings.Contains(r.SQL, frag) {
			t.Errorf("unexpected fragment %q", frag)
		}
	}
	if !strings.Contains(r.SQL, havingOK) {
		t.Error("status=ok fragment missing")
	}
	if r.Params.Has("param_agent") {
		t.Error("unused param sent")
	}
}

func TestEmptyTenantRefused(t *testing.T) {
	f, c := newFakeCH(t)
	calls := []error{
		func() error { _, err := c.ListRuns(ctx0, "", RunFilter{}); return err }(),
		func() error { _, err := c.GetTrace(ctx0, "", "x", 1); return err }(),
		func() error { _, err := c.GetStep(ctx0, "", "x", "y"); return err }(),
		func() error { _, err := c.Overview(ctx0, "", t0, t0, time.Hour); return err }(),
		func() error { _, err := c.Loops(ctx0, "", t0, t0, 1); return err }(),
	}
	for i, err := range calls {
		if !errors.Is(err, ErrNoTenant) {
			t.Errorf("call %d: err = %v", i, err)
		}
	}
	if n := len(f.all()); n != 0 {
		t.Fatalf("%d queries sent without tenant", n)
	}
}

func TestDecodeRows(t *testing.T) {
	f, c := newFakeCH(t)
	f.answers["FROM bindle.traces"] = `{"trace_id":"abc","started":"2026-11-16T12:00:00.123456789Z","steps":4,"failed":1,"cost_usd":0.5,"cost_unknown_steps":2,"agent":"a","root_status":"success","duration_ms":12.5}` + "\n\n"
	runs, err := c.ListRuns(ctx0, "t", RunFilter{From: t0, To: t0.Add(time.Hour), Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	r := runs[0]
	if r.TraceID != "abc" || r.Steps != 4 || r.CostUnknownSteps != 2 || r.Started.Nanosecond() != 123456789 {
		t.Errorf("run = %+v", r)
	}
}

func TestGetTraceMissingAndTruncation(t *testing.T) {
	f, c := newFakeCH(t)
	tr, err := c.GetTrace(ctx0, "t", "x", 2)
	if err != nil || tr != nil {
		t.Fatalf("missing trace: %+v %v", tr, err)
	}
	if n := len(f.all()); n != 1 {
		t.Errorf("steps queried for missing trace: %d queries", n)
	}
	f.answers["GROUP BY trace_id"] = `{"trace_id":"x","started":"2026-11-16T12:00:00Z","steps":3}`
	f.answers["LIMIT {limit:UInt32}"] = strings.Repeat(`{"step_id":"s","start_time":"2026-11-16T12:00:00Z"}`+"\n", 3)
	tr, err = c.GetTrace(ctx0, "t", "x", 2)
	if err != nil || tr == nil {
		t.Fatal(err)
	}
	if len(tr.Steps) != 2 || !tr.Truncated {
		t.Errorf("steps=%d truncated=%v", len(tr.Steps), tr.Truncated)
	}
	if got := f.all()[2].Params.Get("param_limit"); got != "3" {
		t.Errorf("limit param = %q, want maxSteps+1", got)
	}
}

func TestClickHouseErrors(t *testing.T) {
	f, c := newFakeCH(t)
	f.status = 404
	_, err := c.Loops(ctx0, "t", t0, t0, 1)
	var ce *Error
	if !errors.As(err, &ce) || ce.Status != 404 || ce.Code != "60" {
		t.Fatalf("err = %#v", err)
	}

	f.status = 200
	f.answers["cityHash64"] = `{"trace_id":"a","tool_name":"t","repeats":5}` + "\n" + `{"exception": "Code: 396. too many rows"}` + "\n"
	if _, err := c.Loops(ctx0, "t", t0, t0, 1); !errors.As(err, &ce) {
		t.Fatalf("mid-stream exception not surfaced: %v", err)
	}

	f.answers["cityHash64"] = "not json\n"
	if _, err := c.Loops(ctx0, "t", t0, t0, 1); err == nil {
		t.Fatal("bad row should fail")
	}
}

func TestResponseSizeCap(t *testing.T) {
	f, c := newFakeCH(t)
	c.MaxResponseBytes = 100
	f.answers["cityHash64"] = `{"trace_id":"` + strings.Repeat("a", 200) + `"}` + "\n"
	if _, err := c.Loops(ctx0, "t", t0, t0, 1); err == nil {
		t.Fatal("oversized response should fail")
	}
}
