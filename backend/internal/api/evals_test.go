package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/cost"
	"github.com/bindle-ai/bindle/backend/internal/evals"
	"github.com/bindle-ai/bindle/backend/internal/query"
	"github.com/bindle-ai/bindle/backend/internal/ratelimit"
	"github.com/bindle-ai/bindle/backend/internal/replay"
)

// fakeEvals is an in-memory EvalStore for one tenant.
type fakeEvals struct {
	scorers  []evals.Scorer
	datasets map[string]*control.Dataset
	items    map[string][]evals.Run
	runs     []control.EvalRun
	tenants  []string
}

func newFakeEvals() *fakeEvals {
	return &fakeEvals{datasets: map[string]*control.Dataset{}, items: map[string][]evals.Run{}}
}

func (f *fakeEvals) rec(t string) { f.tenants = append(f.tenants, t) }

func (f *fakeEvals) ListScorers(_ context.Context, t string) ([]evals.Scorer, error) {
	f.rec(t)
	return f.scorers, nil
}
func (f *fakeEvals) CreateScorer(_ context.Context, t, _ string, s evals.Scorer) (evals.Scorer, error) {
	f.rec(t)
	for _, x := range f.scorers {
		if x.Name == s.Name {
			return s, control.ErrDuplicate
		}
	}
	s.ID = "sc_" + strings.Repeat("0", 19) + string(rune('a'+len(f.scorers)))
	f.scorers = append(f.scorers, s)
	return s, nil
}
func (f *fakeEvals) UpdateScorer(_ context.Context, t, id string, s evals.Scorer) (evals.Scorer, error) {
	f.rec(t)
	for i, x := range f.scorers {
		if x.ID == id {
			s.ID, s.Kind = x.ID, x.Kind
			if err := evals.Validate(s); err != nil {
				return s, err
			}
			f.scorers[i] = s
			return s, nil
		}
	}
	return s, control.ErrNotFound
}
func (f *fakeEvals) DeleteScorer(_ context.Context, t, id string) error {
	f.rec(t)
	for i, x := range f.scorers {
		if x.ID == id {
			f.scorers = append(f.scorers[:i], f.scorers[i+1:]...)
			return nil
		}
	}
	return control.ErrNotFound
}
func (f *fakeEvals) CreateDataset(_ context.Context, t, _, name, desc string) (control.Dataset, error) {
	f.rec(t)
	if name == "" {
		return control.Dataset{}, evals.Invalid("name must be 1-80 characters on one line")
	}
	d := control.Dataset{ID: "ds_" + strings.Repeat("1", 20), Name: name, Description: desc}
	f.datasets[d.ID] = &d
	return d, nil
}
func (f *fakeEvals) ListDatasets(_ context.Context, t string) ([]control.Dataset, error) {
	f.rec(t)
	out := []control.Dataset{}
	for _, d := range f.datasets {
		out = append(out, *d)
	}
	return out, nil
}
func (f *fakeEvals) GetDataset(_ context.Context, t, id string) (control.Dataset, []evals.Run, error) {
	f.rec(t)
	d, ok := f.datasets[id]
	if !ok {
		return control.Dataset{}, nil, control.ErrNotFound
	}
	return *d, append([]evals.Run(nil), f.items[id]...), nil
}
func (f *fakeEvals) AddDatasetItems(_ context.Context, t, id string, runs []evals.Run) (int, error) {
	f.rec(t)
	if _, ok := f.datasets[id]; !ok {
		return 0, control.ErrNotFound
	}
	f.items[id] = append(f.items[id], runs...)
	return len(runs), nil
}
func (f *fakeEvals) RemoveDatasetItem(_ context.Context, t, id, trace string) error {
	f.rec(t)
	return nil
}
func (f *fakeEvals) DeleteDataset(_ context.Context, t, id string) error {
	f.rec(t)
	delete(f.datasets, id)
	return nil
}
func (f *fakeEvals) RecordEvalRun(_ context.Context, t, _, ds string, items int, tallies []evals.Tally, fails []control.Failure) (control.EvalRun, error) {
	f.rec(t)
	e := control.EvalRun{ID: "ev_x", DatasetID: ds, Items: items, Tallies: tallies, Failures: fails}
	f.runs = append(f.runs, e)
	return e, nil
}
func (f *fakeEvals) ListEvalRuns(_ context.Context, t, ds string) ([]control.EvalRun, error) {
	f.rec(t)
	return f.runs, nil
}

func evalServer(st *fakeStore, ev *fakeEvals) http.Handler {
	return (&Server{Store: st, Evals: ev, Sessions: tokenResolver{"tok-a": "tenant-a", "member-tok": "tenant-a"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}).Handler()
}

func TestEvalsFlow(t *testing.T) {
	st := &fakeStore{runs: []query.Run{{TraceID: strings.Repeat("a", 32)}, {TraceID: strings.Repeat("b", 32)}}}
	ev := newFakeEvals()
	h := evalServer(st, ev)

	// Checks: owners only, validated.
	if w, _ := send(t, h, "POST", "/api/scorers", "member-tok", `{"name":"x","kind":"json_valid"}`); w.Code != 403 {
		t.Errorf("member create: %d", w.Code)
	}
	for _, bad := range []string{`{"name":"x","kind":"llm_judge"}`, `{"name":"x","kind":"regex","value":"(a+)+"}`, `{"kind":"json_valid"}`} {
		if w, _ := send(t, h, "POST", "/api/scorers", "tok-a", bad); w.Code != 400 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	w, body := send(t, h, "POST", "/api/scorers", "tok-a", `{"name":"mentions refund","kind":"contains","value":"refund"}`)
	if w.Code != 201 || body["enabled"] != true {
		t.Fatalf("create: %d %v", w.Code, body)
	}
	id := body["id"].(string)
	if w, _ := send(t, h, "POST", "/api/scorers", "tok-a", `{"name":"mentions refund","kind":"json_valid"}`); w.Code != 409 {
		t.Errorf("duplicate: %d", w.Code)
	}
	send(t, h, "POST", "/api/scorers", "tok-a", `{"name":"few steps","kind":"max_steps","number":3}`)
	if w, _ := send(t, h, "PUT", "/api/scorers/"+id, "tok-a", `{"name":"mentions refund","value":""}`); w.Code != 400 {
		t.Errorf("update validated against the stored kind: %d", w.Code)
	}

	// Dataset: create, add runs, evaluate, export.
	w, body = send(t, h, "POST", "/api/datasets", "member-tok", `{"name":"refund regressions"}`)
	if w.Code != 201 {
		t.Fatalf("dataset: %d %v", w.Code, body)
	}
	ds := body["id"].(string)
	if w, _ := send(t, h, "POST", "/api/datasets/"+ds+"/items", "tok-a", `{"trace_ids":["nothex"]}`); w.Code != 400 {
		t.Errorf("bad trace: %d", w.Code)
	}
	w, body = send(t, h, "POST", "/api/datasets/"+ds+"/items", "tok-a", `{"trace_ids":["`+strings.Repeat("A", 32)+`","`+strings.Repeat("a", 32)+`","`+strings.Repeat("c", 32)+`"]}`)
	if w.Code != 200 || body["added"] != float64(2) || body["requested"] != float64(2) {
		t.Fatalf("add: %d %v", w.Code, body)
	}
	w, body = send(t, h, "POST", "/api/datasets/"+ds+"/evaluate", "member-tok", ``)
	if w.Code != 200 {
		t.Fatalf("evaluate: %d %v", w.Code, body)
	}
	tallies := body["tallies"].([]any)
	if len(tallies) != 2 || tallies[0].(map[string]any)["passed"] != float64(2) || tallies[1].(map[string]any)["failed"] != float64(2) ||
		len(body["failures"].([]any)) != 2 {
		t.Errorf("tallies %v failures %v", tallies, body["failures"])
	}
	r := httptest.NewRequest("GET", "/api/datasets/"+ds+"/export", nil)
	r.Header.Set("Authorization", "Bearer tok-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 || strings.Count(rec.Body.String(), "\n") != 2 || !strings.Contains(rec.Header().Get("Content-Disposition"), "refund-regressions.jsonl") {
		t.Errorf("export: %d %q %q", rec.Code, rec.Header().Get("Content-Disposition"), rec.Body.String())
	}
	if w, _ := get(t, h, "/api/datasets/ds_"+strings.Repeat("9", 20), "tok-a"); w.Code != 404 {
		t.Errorf("missing dataset: %d", w.Code)
	}
	if w, _ := send(t, h, "DELETE", "/api/datasets/"+ds, "member-tok", ""); w.Code != 403 {
		t.Errorf("member delete: %d", w.Code)
	}

	// Live checks per version, plus SDK scores.
	st.versions = []query.VersionStats{{Version: "v2", Runs: 2}}
	w, body = get(t, h, "/api/agents/checks?agent=refunds", "tok-a")
	if w.Code != 200 {
		t.Fatalf("checks: %d %v", w.Code, body)
	}
	vs := body["versions"].([]any)
	if len(vs) != 1 || vs[0].(map[string]any)["runs_checked"] != float64(2) || st.filter.Version != "v2" || st.filter.Agent != "refunds" ||
		len(body["scores"].([]any)) != 1 {
		t.Errorf("checks body %v filter %+v", body, st.filter)
	}
	for _, tn := range append(ev.tenants, st.tenants...) {
		if tn != "tenant-a" {
			t.Fatalf("tenant %q", tn)
		}
	}
	if w, _ := get(t, (&Server{Store: st, Sessions: tokenResolver{"tok-a": "tenant-a"}}).Handler(), "/api/scorers", "tok-a"); w.Code != 503 {
		t.Errorf("unconfigured: %d", w.Code)
	}
}

func TestSafeFilename(t *testing.T) {
	for in, want := range map[string]string{"refund regressions": "refund-regressions", "../../etc": "etc", `"; x`: "x", "": "dataset"} {
		if got := safeFilename(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

type fakeProvider struct{ got []replay.Message }

func (f *fakeProvider) Run(_ context.Context, model, key, system string, msgs []replay.Message, max int, _ *float64) (replay.Result, error) {
	f.got = msgs
	if key == "fake-bad-key-0" {
		return replay.Result{}, &replay.ProviderError{Status: 401, Message: "invalid x-api-key fake-bad-key-0"}
	}
	return replay.Result{Output: "Refund issued.", Model: model, InputTokens: 1_000_000, OutputTokens: 0}, nil
}

func TestReplay(t *testing.T) {
	st := &fakeStore{step: &query.StepDetail{StepSummary: query.StepSummary{StepID: strings.Repeat("1", 16), Type: "llm_call", Model: "gpt-x", InputTokens: 9}, Output: "old answer"}}
	fp := &fakeProvider{}
	srv := &Server{Store: st, Replay: &replay.Runner{Providers: map[string]replay.Provider{"openai": fp}},
		Prices:   cost.New(map[string]cost.Price{"gpt-x": {InputPerMTok: 2, OutputPerMTok: 8}}),
		Sessions: tokenResolver{"tok-a": "tenant-a"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now },
		ReplayLimiter: ratelimit.New(1, 3)}
	h := srv.Handler()
	path := "/api/traces/" + strings.Repeat("a", 32) + "/steps/" + strings.Repeat("1", 16) + "/replay"
	body := `{"provider":"openai","model":"gpt-x","api_key":"fake-good-key-1","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Where is my refund?"}]}`
	w, out := send(t, h, "POST", path, "tok-a", body)
	if w.Code != 200 || out["result"].(map[string]any)["output"] != "Refund issued." || out["cost_usd"] != float64(2) || out["cost_known"] != true ||
		out["original"].(map[string]any)["output"] != "old answer" {
		t.Fatalf("replay: %d %v", w.Code, out)
	}
	if len(fp.got) != 1 || fp.got[0].Role != "user" {
		t.Errorf("prepared messages: %+v", fp.got)
	}
	w, out = send(t, h, "POST", path, "tok-a", strings.Replace(body, "fake-good-key-1", "fake-bad-key-0", 1))
	if w.Code != 502 || strings.Contains(w.Body.String(), "fake-bad-key-0") {
		t.Errorf("provider error must not echo the key: %d %s", w.Code, w.Body.String())
	}
	if w, _ := send(t, h, "POST", path, "tok-a", strings.Replace(body, `"openai"`, `"anthropic"`, 1)); w.Code != 429 && w.Code != 400 {
		t.Errorf("unsupported provider: %d", w.Code)
	}
	// rate limit: burst 3 used
	if w, _ := send(t, h, "POST", path, "tok-a", body); w.Code != 429 {
		t.Errorf("rate limit: %d", w.Code)
	}
	st.step.Type = "tool_call"
	srv.ReplayLimiter = ratelimit.New(100, 100)
	if w, _ := send(t, srv.Handler(), "POST", path, "tok-a", body); w.Code != 400 {
		t.Errorf("tool step: %d", w.Code)
	}
	if w, _ := send(t, srv.Handler(), "POST", "/api/traces/x/steps/y/replay", "tok-a", body); w.Code != 400 {
		t.Errorf("bad ids: %d", w.Code)
	}
}

func TestAWSPrices(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.Contains(r.URL.Path, "AmazonBedrock/") {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(`{"publicationDate":"2026-11-09T00:00:00Z","products":{"A":{"attributes":{"feature":"On-demand Inference","model":"Claude X","provider":"Anthropic","inferenceType":"Input tokens","usagetype":"global"}}},
			"terms":{"OnDemand":{"A":{"t":{"priceDimensions":{"d":{"unit":"1M tokens","pricePerUnit":{"USD":"5"}}}}}}}}`))
	}))
	defer srv.Close()
	s := &Server{Store: &fakeStore{}, Sessions: tokenResolver{"tok-a": "tenant-a"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		AWSPrices: &AWSPriceList{BaseURL: func(offer, region string) string { return srv.URL + "/" + offer + "/" + region }}}
	h := s.Handler()
	w, body := get(t, h, "/api/prices/aws?region=us-east-1", "tok-a")
	rows := body["prices"].([]any)
	if w.Code != 200 || len(rows) != 1 || rows[0].(map[string]any)["model"] != "Claude X" || rows[0].(map[string]any)["tier"] != "global" {
		t.Fatalf("%d %v", w.Code, body)
	}
	get(t, h, "/api/prices/aws?region=us-east-1", "tok-a")
	if calls != 2 {
		t.Errorf("cached: %d fetches", calls)
	}
	if w, _ := get(t, h, "/api/prices/aws?region=../../x", "tok-a"); w.Code != 400 {
		t.Errorf("bad region: %d", w.Code)
	}
}

func TestSessions(t *testing.T) {
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	st := &fakeStore{
		runs: []query.Run{
			{TraceID: b, SessionID: "s1", Started: query.Time{Time: now.Add(-time.Minute)}},
			{TraceID: a, SessionID: "s1", Started: query.Time{Time: now.Add(-2 * time.Minute)}, CostUSD: 0.5},
		},
		feedback: map[string]query.FeedbackSummary{b: {Negative: 1, Count: 1}},
	}
	h := newServer(st)
	w, body := get(t, h, "/api/sessions?window=24h", "tok-a")
	ss := body["sessions"].([]any)
	if w.Code != 200 || len(ss) != 2 || ss[0].(map[string]any)["feedback"].(map[string]any)["negative"] != float64(1) {
		t.Fatalf("list %d %v", w.Code, body)
	}
	w, body = get(t, h, "/api/sessions/s1", "tok-a")
	turns := body["turns"].([]any)
	if w.Code != 200 || len(turns) != 2 || turns[0].(map[string]any)["run"].(map[string]any)["trace_id"] != a ||
		turns[1].(map[string]any)["feedback"] == nil || st.filter.SessionID != "s1" || body["cost_usd"] != 0.5 {
		t.Fatalf("session %d %v", w.Code, body)
	}
	w, body = get(t, h, "/api/insights?window=24h", "tok-a")
	if u := body["unhappy_sessions"].([]any); w.Code != 200 || len(u) != 1 || u[0].(map[string]any)["session_id"] != "s1" {
		t.Errorf("unhappy %v", body["unhappy_sessions"])
	}
	st.runs = nil
	if w, _ := get(t, h, "/api/sessions/nope", "tok-a"); w.Code != 404 {
		t.Errorf("missing: %d", w.Code)
	}
}

func TestSearch(t *testing.T) {
	st := &fakeStore{}
	h := newServer(st)
	w, body := get(t, h, "/api/search?q=timeout&window=24h&status=failure&agent=refunds", "tok-a")
	if w.Code != 200 || len(body["hits"].([]any)) != 1 || st.search.Query != "timeout" || st.search.Status != "failure" || st.search.Agent != "refunds" {
		t.Fatalf("%d %v %+v", w.Code, body, st.search)
	}
	for _, bad := range []string{"/api/search", "/api/search?q=" + strings.Repeat("x", 201), "/api/search?q=x&status=ok"} {
		if w, _ := get(t, h, bad, "tok-a"); w.Code != 400 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
}
