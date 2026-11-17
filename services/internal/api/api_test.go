package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/services/internal/control"
	"github.com/bindle-ai/bindle/services/internal/query"
	"github.com/bindle-ai/bindle/services/internal/ratelimit"
	"github.com/bindle-ai/bindle/services/internal/sdkconfig"
	"github.com/bindle-ai/bindle/services/internal/session"
)

// fakeStore records the tenant and arguments of every call.
type fakeStore struct {
	mu      sync.Mutex
	tenants []string
	filter  query.RunFilter
	runs    []query.Run
	trace   *query.Trace
	step    *query.StepDetail
	ov      *query.Overview
	loops   []query.Loop
	from    time.Time
	to      time.Time
	bucket  time.Duration
	minRep  int
	err     error
}

func (f *fakeStore) rec(t string) {
	f.mu.Lock()
	f.tenants = append(f.tenants, t)
	f.mu.Unlock()
}

func (f *fakeStore) ListRuns(_ context.Context, t string, fl query.RunFilter) ([]query.Run, error) {
	f.rec(t)
	f.filter = fl
	return f.runs, f.err
}
func (f *fakeStore) GetTrace(_ context.Context, t, _ string, _ int) (*query.Trace, error) {
	f.rec(t)
	return f.trace, f.err
}
func (f *fakeStore) GetStep(_ context.Context, t, _, _ string) (*query.StepDetail, error) {
	f.rec(t)
	return f.step, f.err
}
func (f *fakeStore) Overview(_ context.Context, t string, from, to time.Time, b time.Duration) (*query.Overview, error) {
	f.rec(t)
	f.from, f.to, f.bucket = from, to, b
	if f.ov == nil {
		return &query.Overview{}, f.err
	}
	return f.ov, f.err
}
func (f *fakeStore) Loops(_ context.Context, t string, from, to time.Time, n int) ([]query.Loop, error) {
	f.rec(t)
	f.from, f.to, f.minRep = from, to, n
	return f.loops, f.err
}

// tokenResolver is a test-only resolver: one known token per tenant. Tokens
// starting with "member-" resolve as role member, others as owner.
type tokenResolver map[string]string

func (m tokenResolver) Resolve(r *http.Request) (session.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if t, ok := m[tok]; ok {
		role := control.RoleOwner
		if strings.HasPrefix(tok, "member-") {
			role = control.RoleMember
		}
		return session.Principal{TenantID: t, UserID: "usr_" + tok, Role: role}, nil
	}
	return session.Principal{}, session.ErrUnauthenticated
}

var now = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

const (
	traceID = "0123456789abcdef0123456789abcdef"
	stepID  = "0123456789abcdef"
)

func newServer(st *fakeStore) http.Handler {
	s := &Server{
		Store:    st,
		Sessions: tokenResolver{"tok-a": "tenant-a", "tok-b": "tenant-b"},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return now },
	}
	return s.Handler()
}

func get(t *testing.T, h http.Handler, path, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var body map[string]any
	if strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: bad JSON: %v: %s", path, err, w.Body)
		}
	}
	return w, body
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

var allPaths = []string{
	"/api/runs", "/api/traces/" + traceID, "/api/traces/" + traceID + "/steps/" + stepID,
	"/api/overview", "/api/alerts", "/api/nope", "/api/me", "/api/keys",
}

func TestAuthRequiredEverywhere(t *testing.T) {
	st := &fakeStore{}
	h := newServer(st)
	for _, p := range allPaths {
		for _, tok := range []string{"", "wrong"} {
			w, body := get(t, h, p, tok)
			if w.Code != 401 || errCode(body) != "unauthenticated" || w.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("%s token=%q: %d %v", p, tok, w.Code, body)
			}
		}
	}
	if len(st.tenants) != 0 {
		t.Fatalf("store called without auth: %v", st.tenants)
	}
	if w, _ := get(t, h, "/healthz", ""); w.Code != 200 {
		t.Errorf("healthz = %d", w.Code)
	}
}

func TestFailClosedResolverRejectsAll(t *testing.T) {
	st := &fakeStore{}
	h := (&Server{Store: st, Sessions: session.FailClosed{}}).Handler()
	for _, p := range allPaths {
		if w, _ := get(t, h, p, "tok-a"); w.Code != 401 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if len(st.tenants) != 0 {
		t.Fatal("store called")
	}
}

func TestTenantComesOnlyFromSession(t *testing.T) {
	st := &fakeStore{trace: &query.Trace{}, step: &query.StepDetail{}}
	h := newServer(st)
	for _, p := range allPaths[:5] {
		sep := "?"
		if strings.Contains(p, "?") {
			sep = "&"
		}
		r := httptest.NewRequest("GET", p+sep+"tenant=tenant-a&tenant_id=tenant-a", nil)
		r.Header.Set("Authorization", "Bearer tok-b")
		r.Header.Set("X-Bindle-Tenant", "tenant-a")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	for _, got := range st.tenants {
		if got != "tenant-b" {
			t.Fatalf("store got tenant %q, want session tenant tenant-b", got)
		}
	}
	if len(st.tenants) != 5 {
		t.Fatalf("calls = %d", len(st.tenants))
	}
}

func TestRunsDefaultsAndPagination(t *testing.T) {
	mk := func(id string, sec int) query.Run {
		return query.Run{TraceID: strings.Repeat(id, 32), Started: query.Time{Time: now.Add(-time.Duration(sec) * time.Second)},
			Steps: 3, RootStatus: "success"}
	}
	st := &fakeStore{runs: []query.Run{mk("a", 1), mk("b", 2), mk("c", 3)}}
	h := newServer(st)

	w, body := get(t, h, "/api/runs?limit=2", "tok-a")
	if w.Code != 200 {
		t.Fatalf("%d %v", w.Code, body)
	}
	if st.filter.Limit != 3 || !st.filter.To.Equal(now) || !st.filter.From.Equal(now.Add(-24*time.Hour)) {
		t.Errorf("filter = %+v", st.filter)
	}
	runs := body["runs"].([]any)
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	next, _ := body["next_cursor"].(string)
	if next == "" {
		t.Fatal("expected next_cursor")
	}

	st.runs = st.runs[:1]
	w, body = get(t, h, "/api/runs?limit=2&cursor="+next, "tok-a")
	if w.Code != 200 {
		t.Fatalf("%d %v", w.Code, body)
	}
	if st.filter.AfterTrace != strings.Repeat("b", 32) || !st.filter.AfterStart.Equal(now.Add(-2*time.Second)) {
		t.Errorf("cursor not applied: %+v", st.filter)
	}
	if _, ok := body["next_cursor"]; ok {
		t.Error("last page must not have next_cursor")
	}

	if _, _ = get(t, h, "/api/runs?limit=100000", "tok-a"); st.filter.Limit != maxLimit+1 {
		t.Errorf("limit not clamped: %d", st.filter.Limit)
	}
}

func TestRunStatusAndCost(t *testing.T) {
	st := &fakeStore{runs: []query.Run{
		{TraceID: "1", RootStatus: "success"},
		{TraceID: "2", RootStatus: "success", FailedSteps: 1},
		{TraceID: "3", RootStatus: "failure", FailedSteps: 1},
		{TraceID: "4", RootStatus: "", CostUnknownSteps: 2},
	}}
	_, body := get(t, newServer(st), "/api/runs", "tok-a")
	want := []struct {
		status  string
		partial bool
	}{{"ok", false}, {"failed", false}, {"failed", false}, {"unknown", true}}
	for i, r := range body["runs"].([]any) {
		m := r.(map[string]any)
		if m["status"] != want[i].status || m["cost_partial"] != want[i].partial {
			t.Errorf("run %d: %v", i, m)
		}
	}
}

func TestRunsValidation(t *testing.T) {
	h := newServer(&fakeStore{})
	for _, q := range []string{
		"limit=0", "limit=x", "status=bogus", "from=yesterday", "to=nope",
		"from=2026-11-16T12:00:00Z&to=2026-11-16T11:00:00Z",
		"from=2026-01-01T00:00:00Z&to=2026-11-16T00:00:00Z",
		"cursor=!!!", "cursor=e30", "agent=" + strings.Repeat("a", 300),
	} {
		w, body := get(t, h, "/api/runs?"+q, "tok-a")
		if w.Code != 400 || errCode(body) != "bad_request" {
			t.Errorf("%s: %d %v", q, w.Code, body)
		}
	}
}

func TestTraceAndStep(t *testing.T) {
	st := &fakeStore{}
	h := newServer(st)
	if w, _ := get(t, h, "/api/traces/"+traceID, "tok-a"); w.Code != 404 {
		t.Errorf("missing trace = %d", w.Code)
	}
	if w, _ := get(t, h, "/api/traces/"+traceID+"/steps/"+stepID, "tok-a"); w.Code != 404 {
		t.Errorf("missing step = %d", w.Code)
	}
	for _, p := range []string{"/api/traces/xyz", "/api/traces/" + traceID + "/steps/zz", "/api/traces/" + traceID + "aa"} {
		if w, _ := get(t, h, p, "tok-a"); w.Code != 400 {
			t.Errorf("%s = %d", p, w.Code)
		}
	}

	st.trace = &query.Trace{Run: query.Run{TraceID: traceID, RootStatus: "success"}, Truncated: true,
		Steps: []query.StepSummary{{StepID: stepID, Redacted: true}}}
	w, body := get(t, h, "/api/traces/"+strings.ToUpper(traceID), "tok-a")
	if w.Code != 200 || body["truncated"] != true || len(body["steps"].([]any)) != 1 {
		t.Fatalf("%d %v", w.Code, body)
	}
	if _, has := body["steps"].([]any)[0].(map[string]any)["input"]; has {
		t.Error("waterfall must not include payloads")
	}

	st.step = &query.StepDetail{StepSummary: query.StepSummary{StepID: stepID}, Input: "x"}
	w, body = get(t, h, "/api/traces/"+traceID+"/steps/"+stepID, "tok-a")
	step := body["step"].(map[string]any)
	if w.Code != 200 || step["input"] != "x" || step["redacted_kinds"] == nil || step["attributes"] == nil {
		t.Fatalf("%d %v", w.Code, body)
	}
}

func TestOverviewAndAlerts(t *testing.T) {
	st := &fakeStore{ov: &query.Overview{Runs: 4, FailedRuns: 1, Steps: 10, FailedSteps: 5, CostUnknownSteps: 1}}
	h := newServer(st)
	w, body := get(t, h, "/api/overview?window=7d", "tok-a")
	if w.Code != 200 || body["run_failure_rate"] != 0.25 || body["step_failure_rate"] != 0.5 || body["cost_partial"] != true {
		t.Fatalf("%d %v", w.Code, body)
	}
	if st.bucket != 6*time.Hour || !st.from.Equal(now.Add(-7*24*time.Hour)) {
		t.Errorf("window: from=%v bucket=%v", st.from, st.bucket)
	}
	if body["series"] == nil || body["top_failing_tools"] == nil {
		t.Error("lists must be [] not null")
	}
	if w, _ := get(t, h, "/api/overview?window=1y", "tok-a"); w.Code != 400 {
		t.Errorf("bad window = %d", w.Code)
	}

	st.loops = []query.Loop{{TraceID: traceID, ToolName: "search", Repeats: 7}}
	w, body = get(t, h, "/api/alerts", "tok-a")
	a := body["alerts"].([]any)
	if w.Code != 200 || len(a) != 1 || a[0].(map[string]any)["kind"] != "runaway_loop" || st.minRep != 5 ||
		!st.from.Equal(now.Add(-time.Hour)) {
		t.Fatalf("%d %v minRep=%d", w.Code, body, st.minRep)
	}
	if w, _ := get(t, h, "/api/alerts?window=2d", "tok-a"); w.Code != 400 {
		t.Errorf("bad window = %d", w.Code)
	}
}

func TestStoreErrorsAreGeneric(t *testing.T) {
	st := &fakeStore{err: errors.New(`pq: relation "bindle.secret" does not exist`)}
	h := newServer(st)
	for _, p := range allPaths[:5] {
		w, body := get(t, h, p, "tok-a")
		if w.Code != 500 || errCode(body) != "internal" || strings.Contains(w.Body.String(), "secret") {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
}

func TestRateLimitPerTenant(t *testing.T) {
	st := &fakeStore{}
	s := &Server{Store: st, Sessions: tokenResolver{"tok-a": "tenant-a", "tok-b": "tenant-b"},
		Limiter: ratelimit.New(1, 1), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.Handler()
	if w, _ := get(t, h, "/api/alerts", "tok-a"); w.Code != 200 {
		t.Fatalf("first = %d", w.Code)
	}
	w, body := get(t, h, "/api/alerts", "tok-a")
	if w.Code != 429 || errCode(body) != "rate_limited" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("second = %d %v", w.Code, body)
	}
	if w, _ := get(t, h, "/api/alerts", "tok-b"); w.Code != 200 {
		t.Fatalf("other tenant limited: %d", w.Code)
	}
}

func TestHeadersAndMethods(t *testing.T) {
	h := newServer(&fakeStore{})
	w, _ := get(t, h, "/api/alerts", "tok-a")
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers = %v", w.Header())
	}
	r := httptest.NewRequest("POST", "/api/runs", nil)
	r.Header.Set("Authorization", "Bearer tok-a")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code == 200 {
		t.Error("POST must not succeed")
	}
}

func TestPanicRecovered(t *testing.T) {
	s := &Server{Sessions: tokenResolver{"tok-a": "tenant-a"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// Store is nil: handlers panic; the recoverer must turn that into a 500.
	w, body := get(t, s.Handler(), "/api/alerts", "tok-a")
	if w.Code != 500 || errCode(body) != "internal" {
		t.Fatalf("%d %v", w.Code, body)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	c, err := decodeCursor(encodeCursor(ts, traceID))
	if err != nil || c.TraceID != traceID || !time.Unix(0, c.Started).Equal(ts) {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := decodeCursor(encodeCursor(ts, "not-hex")); err == nil {
		t.Fatal("cursor with bad trace id accepted")
	}
}

// ---- keys ------------------------------------------------------------------

type fakeKeys struct {
	tenants []string
	users   []string
	grace   time.Duration
	err     error
}

func (f *fakeKeys) ListKeys(_ context.Context, t string) ([]control.APIKey, error) {
	f.tenants = append(f.tenants, t)
	return []control.APIKey{{ID: "key_0123456789abcdef0123", Name: "prod", DisplayPrefix: "bd_live_abcd1234"}}, f.err
}
func (f *fakeKeys) CreateKey(_ context.Context, t, u, name string) (control.APIKey, string, error) {
	f.tenants, f.users = append(f.tenants, t), append(f.users, u)
	return control.APIKey{ID: "key_new", Name: name}, "bd_live_secret", f.err
}
func (f *fakeKeys) RotateKey(_ context.Context, t, u, id string, g time.Duration) (control.APIKey, string, error) {
	f.tenants, f.users, f.grace = append(f.tenants, t), append(f.users, u), g
	return control.APIKey{ID: "key_rotated"}, "bd_live_secret2", f.err
}
func (f *fakeKeys) RevokeKey(_ context.Context, t, id string) error {
	f.tenants = append(f.tenants, t)
	return f.err
}

func keysServer(k *fakeKeys) http.Handler {
	return (&Server{Store: &fakeStore{}, Keys: k,
		Sessions: tokenResolver{"tok-a": "tenant-a", "member-tok": "tenant-a", "tok-b": "tenant-b"},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
}

func send(t *testing.T, h http.Handler, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

const keyID = "key_0123456789abcdef0123"

func TestKeysLifecycleEndpoints(t *testing.T) {
	k := &fakeKeys{}
	h := keysServer(k)
	if w, body := send(t, h, "GET", "/api/keys", "tok-a", ""); w.Code != 200 || len(body["keys"].([]any)) != 1 {
		t.Fatalf("list %d %v", w.Code, body)
	}
	w, body := send(t, h, "POST", "/api/keys", "tok-a", `{"name":"prod agents"}`)
	if w.Code != 201 || body["secret"] != "bd_live_secret" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create %d %v %v", w.Code, body, w.Header())
	}
	if w, _ := send(t, h, "POST", "/api/keys/"+keyID+"/rotate", "tok-a", ""); w.Code != 201 || k.grace != 24*time.Hour {
		t.Fatalf("rotate default grace: %d %v", w.Code, k.grace)
	}
	if w, _ := send(t, h, "POST", "/api/keys/"+keyID+"/rotate", "tok-a", `{"grace_hours":0}`); w.Code != 201 || k.grace != 0 {
		t.Fatalf("rotate grace 0: %d %v", w.Code, k.grace)
	}
	if w, _ := send(t, h, "POST", "/api/keys/"+keyID+"/revoke", "tok-a", ""); w.Code != 204 {
		t.Fatalf("revoke %d", w.Code)
	}
	for _, tn := range k.tenants {
		if tn != "tenant-a" {
			t.Fatalf("key store got tenant %q", tn)
		}
	}
	for _, u := range k.users {
		if u != "usr_tok-a" {
			t.Fatalf("key store got user %q", u)
		}
	}
}

func TestKeysAuthorizationAndValidation(t *testing.T) {
	k := &fakeKeys{}
	h := keysServer(k)
	// Members can list but not change keys.
	if w, _ := send(t, h, "GET", "/api/keys", "member-tok", ""); w.Code != 200 {
		t.Errorf("member list %d", w.Code)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/keys"}, {"POST", "/api/keys/" + keyID + "/rotate"}, {"POST", "/api/keys/" + keyID + "/revoke"},
	} {
		if w, body := send(t, h, c.method, c.path, "member-tok", ""); w.Code != 403 || errCode(body) != "forbidden" {
			t.Errorf("member %s %s: %d", c.method, c.path, w.Code)
		}
		if w, _ := send(t, h, c.method, c.path, "", ""); w.Code != 401 {
			t.Errorf("anonymous %s %s: %d", c.method, c.path, w.Code)
		}
	}
	k.tenants = nil // only the rejected requests below are under test
	for _, c := range []struct{ path, body string }{
		{"/api/keys", `{"name":"x","tenant_id":"tenant-b"}`}, // unknown fields rejected
		{"/api/keys", `not json`},
		{"/api/keys", `{"name":"` + strings.Repeat("x", 5000) + `"}`},
		{"/api/keys/../rotate", ``},
		{"/api/keys/key_bad/rotate", ``},
		{"/api/keys/" + keyID + "/rotate", `{"grace_hours":-1}`},
		{"/api/keys/" + keyID + "/rotate", `{"grace_hours":1000}`},
	} {
		if w, _ := send(t, h, "POST", c.path, "tok-a", c.body); w.Code/100 == 2 {
			t.Errorf("POST %s %q: %d", c.path, c.body[:min(len(c.body), 40)], w.Code)
		}
	}
	if len(k.tenants) != 0 {
		t.Fatalf("key store called for rejected requests: %v", k.tenants)
	}
	k.err = control.ErrNotFound
	if w, _ := send(t, h, "POST", "/api/keys/"+keyID+"/revoke", "tok-b", ""); w.Code != 404 {
		t.Errorf("revoke missing key: %d", w.Code)
	}
	if w, _ := send(t, h, "POST", "/api/keys/"+keyID+"/rotate", "tok-b", ""); w.Code != 404 {
		t.Errorf("rotate missing key: %d", w.Code)
	}
}

func TestKeysUnconfigured(t *testing.T) {
	h := (&Server{Store: &fakeStore{}, Sessions: tokenResolver{"tok-a": "tenant-a"}}).Handler()
	if w, _ := send(t, h, "GET", "/api/keys", "tok-a", ""); w.Code != 503 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestMe(t *testing.T) {
	w, body := send(t, keysServer(&fakeKeys{}), "GET", "/api/me", "member-tok", "")
	if w.Code != 200 || body["tenant_id"] != "tenant-a" || body["role"] != "member" || body["can_manage_keys"] != false {
		t.Fatalf("%d %v", w.Code, body)
	}
}

type brokenResolver struct{}

func (brokenResolver) Resolve(*http.Request) (session.Principal, error) {
	return session.Principal{}, errors.New("db down")
}

func TestSessionBackendFailureIs503(t *testing.T) {
	h := (&Server{Store: &fakeStore{}, Sessions: brokenResolver{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	if w, _ := send(t, h, "GET", "/api/runs", "x", ""); w.Code != 503 {
		t.Fatalf("got %d", w.Code)
	}
}

// ---- config ------------------------------------------------------------------

type fakeConfig struct {
	stored  sdkconfig.Config
	tenants []string
	err     error
}

func (f *fakeConfig) GetSDKConfig(_ context.Context, t string) (sdkconfig.Config, error) {
	f.tenants = append(f.tenants, t)
	return f.stored, f.err
}
func (f *fakeConfig) PutSDKConfig(_ context.Context, t, _ string, c sdkconfig.Config) (sdkconfig.Config, error) {
	f.tenants = append(f.tenants, t)
	if f.err != nil {
		return c, f.err
	}
	c.Version++
	f.stored = c
	return c, nil
}

func configServerAPI(f *fakeConfig) http.Handler {
	return (&Server{Store: &fakeStore{}, Config: f,
		Sessions: tokenResolver{"tok-a": "tenant-a", "member-tok": "tenant-a"},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
}

func sendPut(t *testing.T, h http.Handler, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	return send(t, h, "PUT", "/api/config", token, body)
}

func TestConfigEndpoints(t *testing.T) {
	f := &fakeConfig{stored: sdkconfig.Default()}
	h := configServerAPI(f)
	w, body := send(t, h, "GET", "/api/config", "tok-a", "")
	if w.Code != 200 || body["sample_rate"] != float64(1) || body["version"] != float64(0) {
		t.Fatalf("get %d %v", w.Code, body)
	}
	w, body = sendPut(t, h, "tok-a", `{"version":0,"sample_rate":0.2,"capture_content":false,"redact_patterns":[{"name":"ORDER","pattern":"ord_\\d+"}]}`)
	if w.Code != 200 || body["version"] != float64(1) || body["capture_content"] != false {
		t.Fatalf("put %d %v", w.Code, body)
	}
	for _, bad := range []string{
		`{"version":1,"sample_rate":2,"capture_content":true}`,
		`{"version":1,"capture_content":true}`,
		`{"version":1,"sample_rate":0.5,"capture_content":true,"redact_patterns":[{"name":"X","pattern":"(a+)+"}]}`,
		`{"version":1,"sample_rate":0.5,"capture_content":true,"tenant_id":"other"}`,
	} {
		if w, _ := sendPut(t, h, "tok-a", bad); w.Code != 400 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w, _ := sendPut(t, h, "member-tok", `{"version":1,"sample_rate":0.5,"capture_content":true}`); w.Code != 403 {
		t.Errorf("member put: %d", w.Code)
	}
	f.err = control.ErrVersionConflict
	if w, body := sendPut(t, h, "tok-a", `{"version":1,"sample_rate":0.5,"capture_content":true}`); w.Code != 409 || errCode(body) != "conflict" {
		t.Errorf("conflict: %d %v", w.Code, body)
	}
	for _, tn := range f.tenants {
		if tn != "tenant-a" {
			t.Fatalf("config store got tenant %q", tn)
		}
	}
	if w, _ := send(t, (&Server{Store: &fakeStore{}, Sessions: tokenResolver{"tok-a": "tenant-a"}}).Handler(), "GET", "/api/config", "tok-a", ""); w.Code != 503 {
		t.Errorf("unconfigured: %d", w.Code)
	}
}
