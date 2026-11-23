package outcomes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/correlate"
)

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func TestParse(t *testing.T) {
	body := []byte(`{"correlation": {"ticket_id": "T-123", "order": 981}, "name": "ticket_reopened", "success": false,
		"value": 2, "comment": "reopened by jane.doe@example.com", "occurred_at": "2026-11-20T11:00:00Z"}`)
	ev, err := Parse("t_1", body, Settings{PendingHours: 48}, now, "api", "")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Name != "ticket_reopened" || ev.Success || *ev.Value != 2 || ev.Status != StatusPending {
		t.Fatalf("unexpected event %+v", ev)
	}
	want := []string{correlate.FromValue("t_1", "order", "981"), correlate.FromValue("t_1", "ticket_id", "T-123")}
	if len(ev.Correlations) != 2 || ev.Correlations[0] != want[0] || ev.Correlations[1] != want[1] {
		t.Fatalf("correlations = %v, want %v", ev.Correlations, want)
	}
	if strings.Contains(ev.Comment, "jane.doe") || !strings.Contains(ev.Comment, "[REDACTED:EMAIL]") {
		t.Fatalf("comment not redacted: %q", ev.Comment)
	}
	if !ev.ExpiresAt.Equal(now.Add(48*time.Hour)) || !ev.OccurredAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("times: expires %v occurred %v", ev.ExpiresAt, ev.OccurredAt)
	}
	// Same content, later: the same event (retries and replays are idempotent).
	again, _ := Parse("t_1", body, Settings{}, now.Add(time.Minute), "api", "")
	if again.ID != ev.ID {
		t.Fatal("a re-sent event must keep its ID")
	}
	other, _ := Parse("t_2", body, Settings{}, now, "api", "")
	if other.ID == ev.ID || other.Correlations[1] == ev.Correlations[1] {
		t.Fatal("the same event in another tenant must not share IDs or hashes")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"not json":     `nope`,
		"unknown":      `{"correlation":{"a":"b"},"name":"x","success":true,"extra":1}`,
		"no corr":      `{"correlation":{},"name":"x","success":true}`,
		"bad key":      `{"correlation":{"Ticket":"b"},"name":"x","success":true}`,
		"empty value":  `{"correlation":{"a":""},"name":"x","success":true}`,
		"float value":  `{"correlation":{"a":1.5},"name":"x","success":true}`,
		"object value": `{"correlation":{"a":{"b":1}},"name":"x","success":true}`,
		"no name":      `{"correlation":{"a":"b"},"success":true}`,
		"no success":   `{"correlation":{"a":"b"},"name":"x"}`,
		"old":          `{"correlation":{"a":"b"},"name":"x","success":true,"occurred_at":"2026-01-01T00:00:00Z"}`,
		"future":       `{"correlation":{"a":"b"},"name":"x","success":true,"occurred_at":"2026-11-21T00:00:00Z"}`,
		"bad time":     `{"correlation":{"a":"b"},"name":"x","success":true,"occurred_at":"yesterday"}`,
		"9 keys":       `{"correlation":{"a":"1","b":"1","c":"1","d":"1","e":"1","f":"1","g":"1","h":"1","i":"1"},"name":"x","success":true}`,
	}
	for name, body := range cases {
		if _, err := Parse("t", []byte(body), Settings{}, now, "api", ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	long := `{"correlation":{"a":"` + strings.Repeat("x", 257) + `"},"name":"x","success":true}`
	if _, err := Parse("t", []byte(long), Settings{}, now, "api", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("long value accepted")
	}
}

func TestSignature(t *testing.T) {
	body := []byte(`{"correlation":{"ticket_id":"T-1"},"name":"ticket_reopened","success":false}`)
	h := Sign("obsec_1", body, now)
	if err := VerifySignature(h, body, "obsec_1", now.Add(time.Minute)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := VerifySignature(h, body, "obsec_2", now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong secret: %v", err)
	}
	if err := VerifySignature(h, append(body, ' '), "obsec_1", now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("changed body: %v", err)
	}
	if err := VerifySignature(h, body, "obsec_1", now.Add(6*time.Minute)); !errors.Is(err, ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	if err := VerifySignature(Sign("obsec_1", body, now.Add(10*time.Minute)), body, "obsec_1", now); !errors.Is(err, ErrStale) {
		t.Fatalf("future: %v", err)
	}
	for _, bad := range []string{"", "t=abc,v1=00", "v1=00", "t=1"} {
		if err := VerifySignature(bad, body, "obsec_1", now); !errors.Is(err, ErrNoSignature) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	// Rotation: either of two signatures may match.
	rot := h + ",v1=" + strings.Repeat("0", 64)
	if err := VerifySignature(rot, body, "obsec_1", now); err != nil {
		t.Fatalf("rotation header rejected: %v", err)
	}
	// A replayed request is the same event, stored once.
	a, _ := Parse("t", body, Settings{}, now, "webhook", "oep_1")
	b, _ := Parse("t", body, Settings{}, now.Add(time.Minute), "webhook", "oep_1")
	if a.ID != b.ID {
		t.Fatal("replay must map to the same event")
	}
}

func TestAttempt(t *testing.T) {
	ev := Event{Status: StatusPending, ExpiresAt: now.Add(24 * time.Hour)}
	next := Attempt(ev, nil, now)
	if next.Status != StatusPending || next.Attempts != 1 || !next.NextAttemptAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("first miss: %+v", next)
	}
	matched := Attempt(next, []string{"a1"}, now.Add(time.Minute))
	if matched.Status != StatusMatched || matched.MatchedAt == nil || len(matched.MatchedTraces) != 1 {
		t.Fatalf("match: %+v", matched)
	}
	late := Attempt(Event{Status: StatusPending, Attempts: 9, ExpiresAt: now.Add(10 * time.Minute)}, nil, now)
	if !late.NextAttemptAt.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("the last try should be at the end of the window: %v", late.NextAttemptAt)
	}
	expired := Attempt(Event{Status: StatusPending, ExpiresAt: now}, nil, now)
	if expired.Status != StatusUnmatched {
		t.Fatalf("expired event should be unmatched: %+v", expired)
	}
}

type fakeStore struct {
	events map[string][]Event
	saved  []Event
}

func (f *fakeStore) DueOutcomeTenants(context.Context) ([]string, error) {
	var out []string
	for t := range f.events {
		out = append(out, t)
	}
	return out, nil
}
func (f *fakeStore) DueOutcomeEvents(_ context.Context, t string, _ int) ([]Event, error) {
	var out []Event
	for _, e := range f.events[t] {
		if e.Status == StatusPending {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeStore) SaveOutcomeAttempt(_ context.Context, t string, ev Event) error {
	f.saved = append(f.saved, ev)
	for i, e := range f.events[t] {
		if e.ID == ev.ID {
			f.events[t][i] = ev
		}
	}
	return nil
}
func (f *fakeStore) PruneOutcomeEvents(context.Context) (int64, error) { return 0, nil }

type fakeFinder map[string]map[string]string // tenant -> correlation -> trace

func (f fakeFinder) RunsByCorrelation(_ context.Context, tenant string, corr []string, _, _ time.Time) ([]string, error) {
	var out []string
	for _, c := range corr {
		if tr, ok := f[tenant][c]; ok {
			out = append(out, tr)
		}
	}
	return out, nil
}

func TestMatcherLateArrivalAndIsolation(t *testing.T) {
	c1 := correlate.FromValue("t_1", "ticket_id", "T-1")
	c2 := correlate.FromValue("t_2", "ticket_id", "T-1")
	store := &fakeStore{events: map[string][]Event{
		"t_1": {{ID: "e1", TenantID: "t_1", Correlations: []string{c1}, Status: StatusPending, ExpiresAt: now.Add(time.Hour)}},
		"t_2": {{ID: "e2", TenantID: "t_2", Correlations: []string{c2}, Status: StatusPending, ExpiresAt: now.Add(time.Hour)}},
	}}
	find := fakeFinder{"t_1": {}, "t_2": {}}
	clock := now
	m := &Matcher{Store: store, Find: find, Now: func() time.Time { return clock }}
	if matched, _, _ := m.RunOnce(context.Background()); matched != 0 {
		t.Fatal("nothing should match before the run arrives")
	}
	// The run arrives in t_1 only (t_2's run has the same ticket ID but its
	// hash is t_2's, and t_2 has no such run).
	find["t_1"][c1] = "trace-1"
	find["t_1"][c2] = "wrong-tenant" // can never be asked for: t_2's hash isn't t_1's
	clock = now.Add(2 * time.Minute)
	matched, _, err := m.RunOnce(context.Background())
	if err != nil || matched != 1 {
		t.Fatalf("matched = %d, err = %v", matched, err)
	}
	if e := store.events["t_1"][0]; e.Status != StatusMatched || e.MatchedTraces[0] != "trace-1" {
		t.Fatalf("t_1 event: %+v", e)
	}
	if e := store.events["t_2"][0]; e.Status != StatusPending {
		t.Fatalf("t_2 event must stay pending: %+v", e)
	}
	clock = now.Add(2 * time.Hour)
	_, unmatched, _ := m.RunOnce(context.Background())
	if unmatched != 1 || store.events["t_2"][0].Status != StatusUnmatched {
		t.Fatalf("expired event should be unmatched, got %+v", store.events["t_2"][0])
	}
}

func TestGradeRuns(t *testing.T) {
	s := Settings{ReversalOutcomes: []string{"ticket_reopened", "refund_reversed"}}
	runs := []RunFacts{
		{TraceID: "a", Agent: "support", Version: "v12", EvalPass: 2},
		{TraceID: "b", Agent: "support", Version: "v12", EvalPass: 1},
		{TraceID: "c", Agent: "support", Version: "v12", EvalPass: 1},
		{TraceID: "d", Agent: "support", Version: "v12", EvalFail: 1},
		{TraceID: "e", Agent: "support", Version: "v12", Failed: true},
		{TraceID: "f", Agent: "support", Version: "v11"},
	}
	out := map[string][]OutcomeFact{
		"a": {{Name: "ticket_reopened", Success: true}}, // a reversal counts even when reported as success
		"b": {{Name: "ticket_resolved", Success: true}},
		"c": {{Name: "ticket_resolved", Success: false}},
		"d": {{Name: "refund_reversed", Success: false}},
	}
	grades := GradeRuns(runs, out, s)
	if len(grades) != 2 || grades[0].Version != "v12" {
		t.Fatalf("grades = %+v", grades)
	}
	g := grades[0]
	if g.Runs != 5 || g.EvaluatedRuns != 4 || g.PassedEvals != 3 || g.Reversed != 2 || g.ReversedAfterPassing != 1 || g.Successes != 1 {
		t.Fatalf("v12 = %+v", g)
	}
	if len(g.ReversedRuns) != 1 || g.ReversedRuns[0] != (ReversedRun{TraceID: "a", Outcome: "ticket_reopened"}) {
		t.Fatalf("reversed runs = %+v", g.ReversedRuns)
	}
	if want := "v12: 75% passed evals, but 40% of runs were later reversed (refund reversed)."; g.Headline != want {
		t.Fatalf("headline = %q, want %q", g.Headline, want)
	}
	if want := "v11: 100% of runs succeeded, and none were reversed."; grades[1].Headline != want {
		t.Fatalf("headline = %q", grades[1].Headline)
	}
}

func TestNormalizeSettings(t *testing.T) {
	s, err := NormalizeSettings(Settings{ReversalOutcomes: []string{"b", "a", "b"}})
	if err != nil || s.PendingHours != DefaultPending || len(s.ReversalOutcomes) != 2 || s.ReversalOutcomes[0] != "a" {
		t.Fatalf("got %+v, %v", s, err)
	}
	for _, bad := range []Settings{{PendingHours: 200}, {ReversalOutcomes: []string{" x"}}, {ReversalOutcomes: []string{"a\nb"}}} {
		if _, err := NormalizeSettings(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
