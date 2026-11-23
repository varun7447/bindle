package api

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/outcomes"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

type fakeOutcomeAdmin struct {
	settings  map[string]outcomes.Settings
	endpoints map[string][]control.OutcomeEndpoint
	events    map[string][]outcomes.Event
	matched   map[string]map[string][]outcomes.OutcomeFact
}

func (f *fakeOutcomeAdmin) GetOutcomeSettings(_ context.Context, t string) (outcomes.Settings, error) {
	if s, ok := f.settings[t]; ok {
		return s, nil
	}
	return outcomes.DefaultSettings(), nil
}
func (f *fakeOutcomeAdmin) PutOutcomeSettings(_ context.Context, t, _ string, s outcomes.Settings) (outcomes.Settings, error) {
	clean, err := outcomes.NormalizeSettings(s)
	if err != nil {
		return outcomes.Settings{}, control.ErrInvalidSettings
	}
	f.settings[t] = clean
	return clean, nil
}
func (f *fakeOutcomeAdmin) CreateOutcomeEndpoint(_ context.Context, t, _, name string) (control.OutcomeEndpoint, string, error) {
	ep := control.OutcomeEndpoint{ID: "oep_0123456789abcdef", Name: name}
	f.endpoints[t] = append(f.endpoints[t], ep)
	return ep, "obsec_secret", nil
}
func (f *fakeOutcomeAdmin) ListOutcomeEndpoints(_ context.Context, t string) ([]control.OutcomeEndpoint, error) {
	return append([]control.OutcomeEndpoint{}, f.endpoints[t]...), nil
}
func (f *fakeOutcomeAdmin) RevokeOutcomeEndpoint(_ context.Context, t, id string) error {
	for _, e := range f.endpoints[t] {
		if e.ID == id {
			return nil
		}
	}
	return control.ErrNotFound
}
func (f *fakeOutcomeAdmin) ListOutcomeEvents(_ context.Context, t, status string, _ int) ([]outcomes.Event, error) {
	out := []outcomes.Event{}
	for _, e := range f.events[t] {
		if status == "" || e.Status == status {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeOutcomeAdmin) MatchedOutcomes(_ context.Context, t string, _ time.Time) (map[string][]outcomes.OutcomeFact, error) {
	out := map[string][]outcomes.OutcomeFact{}
	for k, v := range f.matched[t] {
		out[k] = v
	}
	return out, nil
}

// gradingStore is the fake search store with outcome grading.
type gradingStore struct {
	fakeStore
	runs     []outcomes.RunFacts
	gotExtra map[string][]outcomes.OutcomeFact
	gotRev   []string
}

func (g *gradingStore) GradingFacts(context.Context, string, time.Time, time.Time) ([]outcomes.RunFacts, map[string][]outcomes.OutcomeFact, bool, error) {
	return g.runs, map[string][]outcomes.OutcomeFact{}, false, nil
}
func (g *gradingStore) EconomicsGraded(_ context.Context, _ string, _, _ time.Time, extra map[string][]outcomes.OutcomeFact, s outcomes.Settings) ([]query.Economics, error) {
	g.gotExtra, g.gotRev = extra, s.ReversalOutcomes
	return []query.Economics{{Agent: "support", Version: "v12", Definition: query.OutcomeExplicit}}, nil
}

func TestOutcomeGradingEndpoints(t *testing.T) {
	admin := &fakeOutcomeAdmin{settings: map[string]outcomes.Settings{}, endpoints: map[string][]control.OutcomeEndpoint{},
		events: map[string][]outcomes.Event{"tenant-a": {{ID: "oev_1", Name: "ticket_reopened", Status: outcomes.StatusUnmatched,
			Correlations: []string{"ticket_id:" + strings.Repeat("a", 64)}}}},
		matched: map[string]map[string][]outcomes.OutcomeFact{"tenant-a": {"r1": {{Name: "ticket_reopened", Success: false}}}}}
	st := &gradingStore{runs: []outcomes.RunFacts{
		{TraceID: "r1", Agent: "support", Version: "v12", EvalPass: 1},
		{TraceID: "r2", Agent: "support", Version: "v12", EvalPass: 1},
	}}
	srv := &Server{Store: st, Sessions: tokenResolver{"tok-a": "tenant-a", "member-tok": "tenant-a", "tok-b": "tenant-b"},
		Features: fakeFeatures{"tenant-a": {outcomes.Feature}}, Outcomes: admin,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	h := srv.Handler()

	// Without the flag every route is hidden.
	for _, p := range []string{"/api/outcomes/settings", "/api/outcomes/endpoints", "/api/outcomes/events", "/api/agents/grading"} {
		if w, _ := get(t, h, p, "tok-b"); w.Code != 404 {
			t.Errorf("%s without the flag: %d", p, w.Code)
		}
	}

	if w, body := send(t, h, "PUT", "/api/outcomes/settings", "tok-a", `{"reversal_outcomes":["ticket_reopened"],"pending_hours":48}`); w.Code != 200 || body["pending_hours"] != float64(48) {
		t.Fatalf("put settings %d %v", w.Code, body)
	}
	if w, _ := send(t, h, "PUT", "/api/outcomes/settings", "member-tok", `{"pending_hours":12}`); w.Code != 403 {
		t.Errorf("member changed settings: %d", w.Code)
	}
	if w, _ := send(t, h, "PUT", "/api/outcomes/settings", "tok-a", `{"pending_hours":999}`); w.Code != 400 {
		t.Errorf("bad settings: %d", w.Code)
	}

	w, body := send(t, h, "POST", "/api/outcomes/endpoints", "tok-a", `{"name":"Helpdesk"}`)
	if w.Code != 201 || body["secret"] != "obsec_secret" || body["path"] != "/v1/outcome-events/webhook/oep_0123456789abcdef" {
		t.Fatalf("create endpoint %d %v", w.Code, body)
	}
	if w, body := get(t, h, "/api/outcomes/endpoints", "tok-a"); w.Code != 200 || strings.Contains(w.Body.String(), "obsec_") || len(body["endpoints"].([]any)) != 1 {
		t.Fatalf("list endpoints must not include secrets: %d %s", w.Code, w.Body)
	}
	if w, _ := send(t, h, "POST", "/api/outcomes/endpoints/oep_0123456789abcdef/revoke", "member-tok", ""); w.Code != 403 {
		t.Errorf("member revoked: %d", w.Code)
	}
	if w, _ := send(t, h, "POST", "/api/outcomes/endpoints/oep_ffffffffffffffff/revoke", "tok-a", ""); w.Code != 404 {
		t.Errorf("unknown endpoint: %d", w.Code)
	}

	w, body = get(t, h, "/api/outcomes/events?status=unmatched", "tok-a")
	if w.Code != 200 || len(body["events"].([]any)) != 1 || strings.Contains(w.Body.String(), strings.Repeat("a", 64)) {
		t.Fatalf("events must list key names only: %d %s", w.Code, w.Body)
	}
	if w, _ := get(t, h, "/api/outcomes/events?status=lost", "tok-a"); w.Code != 400 {
		t.Errorf("bad status: %d", w.Code)
	}

	w, body = get(t, h, "/api/agents/grading?window=7d", "tok-a")
	if w.Code != 200 {
		t.Fatalf("grading %d %s", w.Code, w.Body)
	}
	g := body["grades"].([]any)[0].(map[string]any)
	if g["reversed"] != float64(1) || g["headline"] != "v12: 100% passed evals, but 50% of runs were later reversed (ticket reopened)." {
		t.Fatalf("grade %v", g)
	}
	if rr := g["reversed_runs"].([]any); len(rr) != 1 || rr[0].(map[string]any)["trace_id"] != "r1" {
		t.Fatalf("reversed runs %v", rr)
	}

	// Economics switches to graded (matched events, reversal names) with the flag.
	if w, _ := get(t, h, "/api/economics?window=7d", "tok-a"); w.Code != 200 || len(st.gotExtra["r1"]) != 1 || st.gotRev[0] != "ticket_reopened" {
		t.Fatalf("graded economics %d extra %v rev %v", w.Code, st.gotExtra, st.gotRev)
	}
	st.gotExtra = nil
	if w, _ := get(t, h, "/api/economics?window=7d", "tok-b"); w.Code != 200 || st.gotExtra != nil {
		t.Fatalf("economics without the flag must not be graded")
	}
}
