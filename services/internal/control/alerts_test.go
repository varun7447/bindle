package control

import (
	"context"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/services/internal/alerts"
)

func TestIntegrationAlerts(t *testing.T) {
	ctx := context.Background()
	_, apiDB, ingestDB := newTestDB(t)
	alertsDB := open(t, lastAlertsDSN)
	store := &Store{DB: apiDB}
	ev := &AlertStore{DB: alertsDB}

	alice, _ := store.EnsureUser(ctx, "sub-a", "")
	ma, _ := store.Provision(ctx, alice, "A")
	bob, _ := store.EnsureUser(ctx, "sub-b", "")
	mb, _ := store.Provision(ctx, bob, "B")

	rules, err := store.ListAlertRules(ctx, ma.TenantID)
	if err != nil || len(rules) != len(alerts.DefaultRules()) {
		t.Fatalf("default rules: %+v %v", rules, err)
	}
	if _, err := store.CreateAlertRule(ctx, ma.TenantID, alice.ID, alerts.Rule{Kind: alerts.KindErrorRate, Threshold: 2, WindowMinutes: 60}); err == nil {
		t.Fatal("invalid rule saved")
	}
	cost, err := store.CreateAlertRule(ctx, ma.TenantID, alice.ID, alerts.Rule{Kind: alerts.KindRunCost, Threshold: 5, WindowMinutes: 60, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateAlertRule(ctx, mb.TenantID, cost.ID, cost); err != ErrNotFound {
		t.Fatalf("cross-tenant update: %v", err)
	}
	cost.Threshold, cost.Kind = 10, alerts.KindErrorRate // kind must not change
	if up, err := store.UpdateAlertRule(ctx, ma.TenantID, cost.ID, cost); err != nil || up.Threshold != 10 || up.Kind != alerts.KindRunCost {
		t.Fatalf("update: %+v %v", up, err)
	}

	if _, _, err := store.CreateAlertChannel(ctx, ma.TenantID, alice.ID, alerts.Channel{Kind: "webhook", Name: "x", Target: "https://127.0.0.1/hook"}); err == nil {
		t.Fatal("private webhook accepted")
	}
	ch, secret, err := store.CreateAlertChannel(ctx, ma.TenantID, alice.ID, alerts.Channel{Kind: "webhook", Name: "ops", Target: "https://example.com/hook"})
	if err != nil || len(secret) < 40 || ch.Display != "https://example.com/…" {
		t.Fatalf("channel: %+v %q %v", ch, secret, err)
	}
	if chs, _ := store.ListAlertChannels(ctx, mb.TenantID); len(chs) != 0 {
		t.Fatal("bob sees alice's channels")
	}
	if err := store.DeleteAlertChannel(ctx, mb.TenantID, ch.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant delete: %v", err)
	}

	// Evaluator role.
	tenants, err := ev.AlertingTenants(ctx)
	if err != nil || len(tenants) != 2 {
		t.Fatalf("tenants: %v %v", tenants, err)
	}
	if chs, err := ev.Channels(ctx, ma.TenantID); err != nil || len(chs) != 1 || chs[0].Secret != secret {
		t.Fatalf("evaluator channels: %+v %v", chs, err)
	}
	e := alerts.Event{ID: "evt_1", TenantID: ma.TenantID, RuleID: cost.ID, Kind: cost.Kind, Fingerprint: "cost:abc",
		Summary: "Run cost $12", TraceID: "abc", Details: map[string]any{"cost_usd": 12}, FiredAt: time.Now()}
	if created, err := ev.RecordEvent(ctx, e); !created || err != nil {
		t.Fatalf("record: %v %v", created, err)
	}
	e.ID = "evt_2"
	if created, err := ev.RecordEvent(ctx, e); created || err != nil {
		t.Fatalf("same fingerprint fired twice: %v %v", created, err)
	}
	// A tenant can't record an event against another tenant's rule.
	e.ID, e.TenantID, e.Fingerprint = "evt_3", mb.TenantID, "x"
	if created, _ := ev.RecordEvent(ctx, e); created {
		t.Fatal("event recorded under the wrong tenant")
	}
	if err := ev.SetDelivery(ctx, ma.TenantID, "evt_1", map[string]string{ch.ID: "sent"}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListAlertEvents(ctx, ma.TenantID, 10)
	if err != nil || len(events) != 1 || events[0].Delivery[ch.ID] != "sent" || events[0].TraceID != "abc" {
		t.Fatalf("events: %+v %v", events, err)
	}
	if evs, _ := store.ListAlertEvents(ctx, mb.TenantID, 10); len(evs) != 0 {
		t.Fatal("bob sees alice's events")
	}

	// Privileges.
	for _, stmt := range []string{"SELECT * FROM control.api_keys", "UPDATE control.alert_rules SET enabled = false", "DELETE FROM control.alert_events"} {
		if _, err := alertsDB.ExecContext(ctx, stmt); err == nil {
			t.Errorf("alerts role could run %q", stmt)
		}
	}
	for _, stmt := range []string{"SELECT * FROM control.alert_channels", "SELECT control.alerting_tenants()"} {
		if _, err := ingestDB.ExecContext(ctx, stmt); err == nil {
			t.Errorf("ingest role could run %q", stmt)
		}
	}
	for _, stmt := range []string{"DELETE FROM control.alert_events", "INSERT INTO control.alert_events (id, tenant_id, rule_id, kind, fingerprint, summary) VALUES ('e','t','r','k','f','s')",
		"UPDATE control.alert_channels SET target = 'https://evil.example'", "UPDATE control.alert_rules SET tenant_id = 'x'"} {
		if _, err := apiDB.ExecContext(ctx, stmt); err == nil {
			t.Errorf("api role could run %q", stmt)
		}
	}
}
