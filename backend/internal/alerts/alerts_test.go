package alerts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/awsv4"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

func TestValidateRule(t *testing.T) {
	for _, r := range DefaultRules() {
		if err := ValidateRule(r); err != nil {
			t.Errorf("default %+v: %v", r, err)
		}
	}
	for _, r := range []Rule{
		{Kind: KindErrorRate, Threshold: 1.5, WindowMinutes: 60},
		{Kind: KindRunawayLoop, Threshold: 2.5, WindowMinutes: 60},
		{Kind: KindRunawayLoop, Threshold: 1, WindowMinutes: 60},
		{Kind: KindRunCost, Threshold: 0, WindowMinutes: 60},
		{Kind: KindRunCost, Threshold: 1, WindowMinutes: 1},
		{Kind: "latency", Threshold: 1, WindowMinutes: 60},
	} {
		if ValidateRule(r) == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

func TestValidateChannel(t *testing.T) {
	ok := []Channel{
		{Kind: "slack", Name: "s", Target: "https://hooks.slack.com/services/T0/B0/abc123"},
		{Kind: "webhook", Name: "w", Target: "https://example.com/hook?x=1"},
		{Kind: "email", Name: "e", Target: "ops@example.com"},
	}
	for _, c := range ok {
		if err := ValidateChannel(c); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	bad := []Channel{
		{Kind: "slack", Name: "s", Target: "https://hooks.slack.com.evil.com/services/T0/B0/x"},
		{Kind: "slack", Name: "s", Target: "http://hooks.slack.com/services/T0/B0/x"},
		{Kind: "slack", Name: "s", Target: "https://hooks.slack.com/services/T0/../x"},
		{Kind: "webhook", Name: "w", Target: "http://example.com/hook"},
		{Kind: "webhook", Name: "w", Target: "https://user:pw@example.com/"},
		{Kind: "webhook", Name: "w", Target: "https://169.254.169.254/latest/meta-data"},
		{Kind: "webhook", Name: "w", Target: "https://10.0.0.5/"},
		{Kind: "webhook", Name: "w", Target: "https://[::1]/"},
		{Kind: "webhook", Name: "w", Target: "https://localhost/"},
		{Kind: "webhook", Name: "w", Target: "https://metadata.google.internal/"},
		{Kind: "email", Name: "e", Target: "Ops <ops@example.com>"},
		{Kind: "email", Name: "e", Target: "a@example.com, b@example.com"},
		{Kind: "email", Name: "", Target: "ops@example.com"},
		{Kind: "sms", Name: "x", Target: "+1555"},
	}
	for _, c := range bad {
		if ValidateChannel(c) == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func TestPublicIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1",
		"0.0.0.0", "::1", "fe80::1", "fc00::1", "fd00:ec2::254", "255.255.255.255", "::ffff:127.0.0.1"} {
		if publicIP(net.ParseIP(s)) {
			t.Errorf("%s treated as public", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "2606:4700::1111", "52.94.236.248"} {
		if !publicIP(net.ParseIP(s)) {
			t.Errorf("%s treated as private", s)
		}
	}
}

func TestSafeClientRefusesLocalAndRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, err := SafeClient().Get(srv.URL) // 127.0.0.1
	if err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("loopback reached: %v", err)
	}
}

// receiver is a TLS server reached for any hostname (the client dials it
// regardless of the URL's host), so validation sees realistic URLs.
func receiver(t *testing.T, h http.HandlerFunc) *http.Client {
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test server
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("alerts: redirects are not followed") },
	}
}

func TestWebhookIsSignedAndSlackIsText(t *testing.T) {
	var mu sync.Mutex
	var got []*http.Request
	var bodies [][]byte
	hc := receiver(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, bodies = append(got, r), append(bodies, b)
		mu.Unlock()
	})
	d := &Deliverer{HTTP: hc, ConsoleURL: "https://bindle.example.com"}
	ev := Event{ID: "evt_1", Kind: KindRunCost, RuleID: "ar_1", Summary: "Run cost $12.00", TraceID: "abc", FiredAt: time.Unix(1e9, 0)}
	if err := d.Send(context.Background(), Channel{Kind: "webhook", Name: "w", Target: "https://hooks.example.com/x", Secret: "whsec_k"}, ev, Rule{}); err != nil {
		t.Fatal(err)
	}
	r, body := got[0], bodies[0]
	mac := hmac.New(sha256.New, []byte("whsec_k"))
	mac.Write([]byte(r.Header.Get("X-Bindle-Timestamp") + "." + string(body)))
	if r.Header.Get("X-Bindle-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("bad signature")
	}
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	if payload["url"] != "https://bindle.example.com/traces/abc" || payload["summary"] != ev.Summary {
		t.Fatalf("payload %s", body)
	}
	if err := d.Send(context.Background(), Channel{Kind: "slack", Name: "s", Target: "https://hooks.slack.com/services/T/B/c"}, ev, Rule{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bodies[1]), `"text":"Bindle alert: Run cost $12.00\nhttps://bindle.example.com/traces/abc"`) {
		t.Fatalf("slack body %s", bodies[1])
	}
}

func TestDeliveryRetriesOnlyServerErrors(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	status := 503
	hc := receiver(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(status)
	})
	d := &Deliverer{HTTP: hc}
	ch := Channel{Kind: "webhook", Name: "w", Target: "https://hooks.example.com/x"}
	if err := d.Send(context.Background(), ch, Event{}, Rule{}); err == nil || calls != 3 {
		t.Fatalf("503: %v after %d calls", err, calls)
	}
	calls, status = 0, 404
	if err := d.Send(context.Background(), ch, Event{}, Rule{}); err == nil || calls != 1 {
		t.Fatalf("404 retried: %d calls", calls)
	}
	calls, status = 0, http.StatusFound
	if err := d.Send(context.Background(), ch, Event{}, Rule{}); err == nil || calls != 1 {
		t.Fatalf("redirect: %v %d", err, calls)
	}
}

type staticCreds struct{}

func (staticCreds) Retrieve(context.Context) (awsv4.Credentials, error) {
	return awsv4.Credentials{AccessKeyID: "AKID", SecretAccessKey: "s"}, nil
}

func TestSESRequest(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"MessageId":"m"}`)
	}))
	defer srv.Close()
	s := &SES{Region: "us-west-2", From: "alerts@bindle.ai", Creds: staticCreds{}, Endpoint: srv.URL}
	if err := s.Send(context.Background(), "ops@example.com", "subj", "text"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v2/email/outbound-emails" || !strings.Contains(gotAuth, "/us-west-2/ses/aws4_request") {
		t.Fatalf("%s %s", gotPath, gotAuth)
	}
	if gotBody["FromEmailAddress"] != "alerts@bindle.ai" {
		t.Fatalf("%v", gotBody)
	}
	if err := (&Deliverer{}).Send(context.Background(), Channel{Kind: "email", Name: "e", Target: "ops@example.com"}, Event{}, Rule{}); err == nil {
		t.Fatal("email without SES configured must fail")
	}
}

// ---- evaluator ----------------------------------------------------------

type memStore struct {
	mu       sync.Mutex
	rules    map[string][]Rule
	channels map[string][]Channel
	events   map[string]Event // by rule+fingerprint
	delivery map[string]map[string]string
}

func (m *memStore) AlertingTenants(context.Context) ([]string, error) {
	var out []string
	for t := range m.rules {
		out = append(out, t)
	}
	return out, nil
}
func (m *memStore) Rules(_ context.Context, t string) ([]Rule, error) { return m.rules[t], nil }
func (m *memStore) Channels(_ context.Context, t string) ([]Channel, error) {
	return m.channels[t], nil
}
func (m *memStore) RecordEvent(_ context.Context, ev Event) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := ev.RuleID + "|" + ev.Fingerprint
	if _, ok := m.events[k]; ok {
		return false, nil
	}
	m.events[k] = ev
	return true, nil
}
func (m *memStore) SetDelivery(_ context.Context, _, id string, d map[string]string) error {
	m.delivery[id] = d
	return nil
}

type fakeDetector struct {
	overview *query.Overview
	loops    []query.Loop
	costly   []query.Run
	tenants  []string
	insights *query.Insights
	spend    map[string]float64
	versions []query.VersionStats
	feedback map[string]query.FeedbackSummary
	patterns []query.ErrorPatternCount
	before   []query.ErrorPatternCount
}

func (f *fakeDetector) Insights(_ context.Context, t string, _, _ time.Time) (*query.Insights, error) {
	f.tenants = append(f.tenants, t)
	if f.insights == nil {
		return &query.Insights{}, nil
	}
	return f.insights, nil
}
func (f *fakeDetector) CostByAgent(_ context.Context, t string, _, _ time.Time) (map[string]float64, error) {
	f.tenants = append(f.tenants, t)
	return f.spend, nil
}
func (f *fakeDetector) AgentVersions(_ context.Context, t, _ string, _, _ time.Time) ([]query.VersionStats, error) {
	f.tenants = append(f.tenants, t)
	return f.versions, nil
}
func (f *fakeDetector) RecentFeedback(_ context.Context, t string, _, _ time.Time) (map[string]query.FeedbackSummary, error) {
	f.tenants = append(f.tenants, t)
	return f.feedback, nil
}
func (f *fakeDetector) ErrorPatterns(_ context.Context, t string, from, to time.Time) ([]query.ErrorPatternCount, error) {
	f.tenants = append(f.tenants, t)
	if to.Sub(from) > 24*time.Hour { // the 7 days before
		return f.before, nil
	}
	return f.patterns, nil
}

func (f *fakeDetector) Overview(_ context.Context, t string, _, _ time.Time, _ time.Duration) (*query.Overview, error) {
	f.tenants = append(f.tenants, t)
	return f.overview, nil
}
func (f *fakeDetector) Loops(_ context.Context, t string, _, _ time.Time, _ int) ([]query.Loop, error) {
	f.tenants = append(f.tenants, t)
	return f.loops, nil
}
func (f *fakeDetector) CostlyRuns(_ context.Context, t string, _, _ time.Time, _ float64, _ int) ([]query.Run, error) {
	f.tenants = append(f.tenants, t)
	return f.costly, nil
}

func TestEvaluatorFiresOnceAndDelivers(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	hc := receiver(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		texts = append(texts, string(b))
		mu.Unlock()
	})
	st := &memStore{
		rules: map[string][]Rule{"t1": {
			{ID: "r_loop", TenantID: "t1", Kind: KindRunawayLoop, Threshold: 5, WindowMinutes: 60, Enabled: true},
			{ID: "r_rate", TenantID: "t1", Kind: KindErrorRate, Threshold: 0.2, WindowMinutes: 60, MinRuns: 10, Enabled: true},
			{ID: "r_cost", TenantID: "t1", Kind: KindRunCost, Threshold: 5, WindowMinutes: 60, Enabled: false},
		}},
		channels: map[string][]Channel{"t1": {
			{ID: "c1", Kind: "slack", Name: "s", Target: "https://hooks.slack.com/services/T/B/c", Enabled: true},
			{ID: "c2", Kind: "slack", Name: "off", Target: "https://hooks.slack.com/services/T/B/d", Enabled: false},
		}},
		events: map[string]Event{}, delivery: map[string]map[string]string{},
	}
	det := &fakeDetector{
		overview: &query.Overview{Runs: 50, FailedRuns: 20},
		loops:    []query.Loop{{TraceID: "tr1", ToolName: "search", Repeats: 7}},
		costly:   []query.Run{{TraceID: "tr2", CostUSD: 9}},
	}
	now := time.Date(2026, 11, 16, 12, 30, 0, 0, time.UTC)
	e := &Evaluator{Store: st, Detect: det, Deliver: &Deliverer{HTTP: hc}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return now }}
	for i := 0; i < 3; i++ {
		if err := e.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	if len(st.events) != 2 {
		t.Fatalf("events: %v", st.events)
	}
	if len(texts) != 2 {
		t.Fatalf("delivered %d times (disabled channel or repeat fired): %v", len(texts), texts)
	}
	for _, d := range st.delivery {
		if d["c1"] != "sent" || d["c2"] != "" {
			t.Fatalf("delivery %v", d)
		}
	}
	for _, tn := range det.tenants {
		if tn != "t1" {
			t.Fatalf("queried tenant %q", tn)
		}
	}
	// Below min_runs: no error-rate alert.
	st.events = map[string]Event{}
	det.overview = &query.Overview{Runs: 5, FailedRuns: 5}
	det.loops = nil
	_ = e.RunOnce(context.Background())
	if len(st.events) != 0 {
		t.Fatalf("fired below min_runs: %v", st.events)
	}
}

func TestQualityAndDigestRules(t *testing.T) {
	monday := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	det := &fakeDetector{
		feedback: map[string]query.FeedbackSummary{"a": {Negative: 3, Count: 3}, "b": {Positive: 2, Count: 2}},
		spend:    map[string]float64{"refunds": 5},
		versions: []query.VersionStats{{Version: "v2", Runs: 10, CostUSD: 2}, {Version: "v1", Runs: 10, CostUSD: 1}},
		patterns: []query.ErrorPatternCount{{Pattern: "timeout after <n>s", StepName: "lookup", Failures: 4, SampleTrace: "t1"}, {Pattern: "old", Failures: 9}},
		before:   []query.ErrorPatternCount{{Pattern: "old", Failures: 1}},
		overview: &query.Overview{Runs: 100, FailedRuns: 5, CostUSD: 12.5},
		insights: &query.Insights{Previous: query.PeriodTotals{Runs: 80, FailedRuns: 8, CostUSD: 10},
			Issues: []query.Issue{{StepName: "lookup", StepType: "tool_call", Failures: 4}}},
	}
	e := &Evaluator{Detect: det, Now: func() time.Time { return monday }}
	ev := func(r Rule) []Event {
		t.Helper()
		if err := ValidateRule(r); err != nil {
			t.Fatalf("%s: %v", r.Kind, err)
		}
		out, err := e.detect(context.Background(), "t", r)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := ev(Rule{Kind: KindNegativeFeedback, Threshold: 0.5, WindowMinutes: 60}); len(got) != 1 || !strings.Contains(got[0].Summary, "60%") {
		t.Errorf("negative feedback: %+v", got)
	}
	if got := ev(Rule{Kind: KindNegativeFeedback, Threshold: 0.5, WindowMinutes: 60, MinRuns: 10}); len(got) != 0 {
		t.Errorf("too little feedback: %+v", got)
	}
	if got := ev(Rule{Kind: KindCostRegression, Threshold: 0.5, WindowMinutes: 1440}); len(got) != 1 || !strings.Contains(got[0].Summary, "100% more than v1") {
		t.Errorf("cost regression: %+v", got)
	}
	if got := ev(Rule{Kind: KindCostRegression, Threshold: 1.5, WindowMinutes: 1440}); len(got) != 0 {
		t.Errorf("below threshold: %+v", got)
	}
	if got := ev(Rule{Kind: KindNewError, Threshold: 3, WindowMinutes: 60}); len(got) != 1 || got[0].TraceID != "t1" || !strings.Contains(got[0].Summary, "New error in lookup") {
		t.Errorf("new error: %+v", got)
	}
	got := ev(Rule{Kind: KindWeeklyDigest, Threshold: 1, WindowMinutes: 1440})
	if len(got) != 1 || got[0].Fingerprint != "digest:2026-W47" || !strings.Contains(got[0].Summary, "100 runs (80 the week before), 5% failed (10% before)") ||
		!strings.Contains(got[0].Summary, "lookup (4 failures)") {
		t.Errorf("digest: %+v", got)
	}
	e.Now = func() time.Time { return monday.Add(24 * time.Hour) }
	if got := ev(Rule{Kind: KindWeeklyDigest, Threshold: 1, WindowMinutes: 1440}); len(got) != 0 {
		t.Errorf("digest only on Mondays: %+v", got)
	}
	if ValidateRule(Rule{Kind: KindWeeklyDigest, Threshold: 2, WindowMinutes: 1440}) == nil || ValidateRule(Rule{Kind: KindNewError, Threshold: 0.5, WindowMinutes: 60}) == nil {
		t.Error("validation")
	}
}
