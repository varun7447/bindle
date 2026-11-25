package control

import (
	"context"
	"errors"
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/alerts"
	"github.com/bindle-ai/bindle/backend/internal/secretbox"
)

// Test deliveries: the API (encrypt-only) records a request, the alerts
// worker claims it, decrypts the channel and records the outcome.
func TestIntegrationChannelTests(t *testing.T) {
	ctx := context.Background()
	owner, apiDB, _ := newTestDB(t)
	alertsDB := open(t, lastAlertsDSN)
	api := &Store{DB: apiDB, Secrets: secretbox.EncryptOnly{Box: testSecrets}}
	alice, _ := api.EnsureUser(ctx, "sub-ct-a", "")
	ma, _ := api.Provision(ctx, alice, "Channel tests A")
	bob, _ := api.EnsureUser(ctx, "sub-ct-b", "")
	mb, _ := api.Provision(ctx, bob, "Channel tests B")

	hook, secret, err := api.CreateAlertChannel(ctx, ma.TenantID, alice.ID, alerts.Channel{Kind: "webhook", Name: "h", Target: "https://example.com/hook?token=abc"})
	if err != nil {
		t.Fatal(err)
	}
	// The API can't decrypt a channel, even by mistake.
	if _, err := api.GetAlertChannel(ctx, ma.TenantID, hook.ID); !errors.Is(err, secretbox.ErrDecryptNotAllowed) {
		t.Fatalf("API decrypted a channel: %v", err)
	}

	ct, err := api.RequestChannelTest(ctx, ma.TenantID, alice.ID, hook.ID)
	if err != nil || ct.Status != "pending" {
		t.Fatalf("request: %+v %v", ct, err)
	}
	// Another tenant can't test or read it.
	if _, err := api.RequestChannelTest(ctx, mb.TenantID, bob.ID, hook.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant request: %v", err)
	}
	if _, err := api.GetChannelTest(ctx, mb.TenantID, hook.ID, ct.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant read: %v", err)
	}
	// The API role can't claim tests itself.
	if _, err := apiDB.ExecContext(ctx, `SELECT * FROM control.claim_channel_tests(10)`); err == nil {
		t.Fatal("api role claimed channel tests")
	}

	as := &AlertStore{DB: alertsDB, Secrets: testSecrets}
	jobs, err := as.ClaimChannelTests(ctx, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != ct.ID || jobs[0].TenantID != ma.TenantID || jobs[0].ChannelID != hook.ID {
		t.Fatalf("claim: %+v %v", jobs, err)
	}
	if again, _ := as.ClaimChannelTests(ctx, 10); len(again) != 0 {
		t.Fatalf("claimed twice: %+v", again)
	}
	ch, err := as.TestChannel(ctx, ma.TenantID, hook.ID)
	if err != nil || ch.Secret != secret || ch.Target != "https://example.com/hook?token=abc" {
		t.Fatalf("worker decrypt: %+v %v", ch, err)
	}
	if got, _ := api.GetChannelTest(ctx, ma.TenantID, hook.ID, ct.ID); got.Status != "running" {
		t.Fatalf("status while running: %+v", got)
	}
	if err := as.FinishChannelTest(ctx, ma.TenantID, ct.ID, false, "the receiver answered HTTP 500"); err != nil {
		t.Fatal(err)
	}
	if got, _ := api.GetChannelTest(ctx, ma.TenantID, hook.ID, ct.ID); got.Status != "failed" || got.Error != "the receiver answered HTTP 500" {
		t.Fatalf("result: %+v", got)
	}

	// A test whose worker stopped mid-delivery is claimed again after 2 minutes.
	stuck, _ := api.RequestChannelTest(ctx, ma.TenantID, alice.ID, hook.ID)
	if jobs, _ := as.ClaimChannelTests(ctx, 10); len(jobs) != 1 {
		t.Fatalf("claim stuck: %+v", jobs)
	}
	if _, err := owner.ExecContext(ctx, `UPDATE control.alert_channel_tests SET claimed_at = now() - interval '3 minutes' WHERE id = $1`, stuck.ID); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := as.ClaimChannelTests(ctx, 10); len(jobs) != 1 || jobs[0].ID != stuck.ID {
		t.Fatalf("reclaim: %+v", jobs)
	}
	if err := as.FinishChannelTest(ctx, ma.TenantID, stuck.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := api.GetChannelTest(ctx, ma.TenantID, hook.ID, stuck.ID); got.Status != "ok" || got.Error != "" {
		t.Fatalf("ok result: %+v", got)
	}

	// Tests older than a day are removed on the next claim, and deleting the
	// channel removes its tests.
	if _, err := owner.ExecContext(ctx, `UPDATE control.alert_channel_tests SET requested_at = now() - interval '2 days' WHERE id = $1`, ct.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = as.ClaimChannelTests(ctx, 10)
	if _, err := api.GetChannelTest(ctx, ma.TenantID, hook.ID, ct.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old test kept: %v", err)
	}
	if err := api.DeleteAlertChannel(ctx, ma.TenantID, hook.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = owner.QueryRowContext(ctx, `SELECT count(*) FROM control.alert_channel_tests`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d tests left after deleting the channel", left)
	}
}
