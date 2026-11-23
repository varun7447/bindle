package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/correlate"
	"github.com/bindle-ai/bindle/backend/internal/outcomes"
)

func TestIntegrationOutcomeEvents(t *testing.T) {
	ctx := context.Background()
	_, apiDB, ingestDB := newTestDB(t)
	alertsDB := open(t, lastAlertsDSN)
	store := &Store{DB: apiDB}
	tenant := func(sub string) string {
		u, err := store.EnsureUser(ctx, sub, sub+"@example.com")
		if err != nil {
			t.Fatal(err)
		}
		m, err := store.Provision(ctx, u, sub)
		if err != nil {
			t.Fatal(err)
		}
		return m.TenantID
	}
	a, b := tenant("outcomes-a"), tenant("outcomes-b")

	// Settings: defaults, then saved and read by ingest.
	if s, err := store.GetOutcomeSettings(ctx, a); err != nil || s.PendingHours != 24 || len(s.ReversalOutcomes) != 0 {
		t.Fatalf("defaults %+v %v", s, err)
	}
	if _, err := store.PutOutcomeSettings(ctx, a, "u", outcomes.Settings{ReversalOutcomes: []string{"ticket_reopened"}, PendingHours: 48}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutOutcomeSettings(ctx, a, "u", outcomes.Settings{PendingHours: 999}); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("bad settings: %v", err)
	}
	in := &OutcomeIngest{DB: ingestDB}
	if s, err := in.Settings(ctx, a); err != nil || s.PendingHours != 48 || !s.IsReversal("ticket_reopened") {
		t.Fatalf("ingest settings %+v %v", s, err)
	}
	if s, err := in.Settings(ctx, b); err != nil || s.PendingHours != 24 {
		t.Fatalf("b settings %+v %v", s, err)
	}

	// Endpoints: the secret is returned once; the API role can't read it back.
	ep, secret, err := store.CreateOutcomeEndpoint(ctx, a, "u", "Helpdesk")
	if err != nil || !strings.HasPrefix(secret, "obsec_") {
		t.Fatal(err)
	}
	if _, err := apiDB.ExecContext(ctx, `SELECT secret FROM control.outcome_endpoints`); err == nil {
		t.Fatal("the api role can read endpoint secrets")
	}
	if gotT, gotS, ok, err := in.EndpointSecret(ctx, ep.ID); err != nil || !ok || gotT != a || gotS != secret {
		t.Fatalf("endpoint lookup %v %v %v", gotT, ok, err)
	}
	if list, err := store.ListOutcomeEndpoints(ctx, b); err != nil || len(list) != 0 {
		t.Fatalf("b sees a's endpoints: %v %v", list, err)
	}
	if err := store.RevokeOutcomeEndpoint(ctx, a, ep.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := in.EndpointSecret(ctx, ep.ID); ok {
		t.Fatal("revoked endpoint still accepted")
	}

	// Events: stored once, readable only by their tenant.
	now := time.Now().UTC().Truncate(time.Millisecond)
	body := []byte(`{"correlation":{"ticket_id":"T-123"},"name":"ticket_reopened","success":false}`)
	ev, err := outcomes.Parse(a, body, outcomes.Settings{PendingHours: 1}, now, "api", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := in.Record(ctx, ev); err != nil || !created {
		t.Fatalf("record %v %v", created, err)
	}
	if again, created, err := in.Record(ctx, ev); err != nil || created || again.ID != ev.ID || again.Status != outcomes.StatusPending {
		t.Fatalf("duplicate %+v %v %v", again, created, err)
	}
	evB, _ := outcomes.Parse(b, body, outcomes.Settings{}, now, "api", "")
	if _, _, err := in.Record(ctx, evB); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Status(ctx, b, ev.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("b can read a's event: %v", err)
	}
	// Correlations are stored as tenant-bound hashes only.
	var raw string
	if err := apiDB.QueryRowContext(ctx, `SELECT array_to_string(correlations, ',') FROM control.outcome_events`).Scan(&raw); err == nil {
		t.Fatal("api role read events without a tenant")
	}
	if listed, err := store.ListOutcomeEvents(ctx, a, "", 10); err != nil || len(listed) != 1 ||
		listed[0].Correlations[0] != correlate.FromValue(a, "ticket_id", "T-123") || strings.Contains(listed[0].Correlations[0], "T-123") {
		t.Fatalf("list %+v %v", listed, err)
	}

	// Matcher store (alerts role): due tenants, attempts, matched outcomes.
	ms := &OutcomeMatchStore{DB: alertsDB}
	due, err := ms.DueOutcomeTenants(ctx)
	if err != nil || len(due) != 2 {
		t.Fatalf("due tenants %v %v", due, err)
	}
	evs, err := ms.DueOutcomeEvents(ctx, a, 10)
	if err != nil || len(evs) != 1 || evs[0].ID != ev.ID {
		t.Fatalf("due events %+v %v", evs, err)
	}
	matched := outcomes.Attempt(evs[0], []string{strings.Repeat("a", 32)}, now)
	if err := ms.SaveOutcomeAttempt(ctx, a, matched); err != nil {
		t.Fatal(err)
	}
	if err := ms.SaveOutcomeAttempt(ctx, b, matched); !errors.Is(err, ErrNotFound) {
		t.Fatalf("saved a's event under b: %v", err)
	}
	if st, err := in.Status(ctx, a, ev.ID); err != nil || st.Status != outcomes.StatusMatched || len(st.MatchedTraces) != 1 {
		t.Fatalf("status after match %+v %v", st, err)
	}
	facts, err := store.MatchedOutcomes(ctx, a, now.Add(-time.Hour))
	if err != nil || len(facts[strings.Repeat("a", 32)]) != 1 || facts[strings.Repeat("a", 32)][0].Name != "ticket_reopened" {
		t.Fatalf("matched outcomes %+v %v", facts, err)
	}
	if facts, _ := store.MatchedOutcomes(ctx, b, now.Add(-time.Hour)); len(facts) != 0 {
		t.Fatalf("b sees a's matches: %v", facts)
	}
	// Expired: unmatched, still listed.
	evsB, _ := ms.DueOutcomeEvents(ctx, b, 10)
	if err := ms.SaveOutcomeAttempt(ctx, b, outcomes.Attempt(evsB[0], nil, evsB[0].ExpiresAt)); err != nil {
		t.Fatal(err)
	}
	if listed, _ := store.ListOutcomeEvents(ctx, b, outcomes.StatusUnmatched, 10); len(listed) != 1 {
		t.Fatalf("unmatched event not listed: %+v", listed)
	}
	if n, err := ms.PruneOutcomeEvents(ctx); err != nil || n != 0 {
		t.Fatalf("prune %d %v", n, err)
	}
	// Ingest can't change an event's status; only the matcher can.
	if _, err := ingestDB.ExecContext(ctx, `UPDATE control.outcome_events SET status = 'matched'`); err == nil {
		t.Fatal("ingest role updated events")
	}
}
