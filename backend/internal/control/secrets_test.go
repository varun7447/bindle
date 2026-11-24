package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/alerts"
	"github.com/bindle-ai/bindle/backend/internal/secretbox"
)

// testSecrets is a local AES-GCM box with a fixed test key (not a real secret).
var testSecrets = func() secretbox.Box {
	b, err := secretbox.NewLocal(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		panic(err)
	}
	return b
}()

func TestIntegrationEncryptedSecrets(t *testing.T) {
	ctx := context.Background()
	owner, apiDB, ingestDB := newTestDB(t)
	alertsDB := open(t, lastAlertsDSN)
	store := &Store{DB: apiDB, Secrets: testSecrets}
	alice, _ := store.EnsureUser(ctx, "sub-secrets", "")
	m, _ := store.Provision(ctx, alice, "Secrets")
	tenant := m.TenantID

	// Without a key nothing is created, rather than stored in plaintext.
	bare := &Store{DB: apiDB}
	if _, _, err := bare.CreateAlertChannel(ctx, tenant, alice.ID, alerts.Channel{Kind: "slack", Name: "s", Target: "https://hooks.slack.com/services/T0/B0/xyz"}); !errors.Is(err, secretbox.ErrNotConfigured) {
		t.Fatalf("channel without a key: %v", err)
	}
	if _, _, err := bare.CreateOutcomeEndpoint(ctx, tenant, alice.ID, "x"); !errors.Is(err, secretbox.ErrNotConfigured) {
		t.Fatalf("endpoint without a key: %v", err)
	}

	slackURL := "https://hooks.slack.com/services/T0/B0/xyz123secret"
	slack, _, err := store.CreateAlertChannel(ctx, tenant, alice.ID, alerts.Channel{Kind: "slack", Name: "s", Target: slackURL})
	if err != nil {
		t.Fatal(err)
	}
	hook, hookSecret, err := store.CreateAlertChannel(ctx, tenant, alice.ID, alerts.Channel{Kind: "webhook", Name: "h", Target: "https://example.com/hook?token=abc"})
	if err != nil {
		t.Fatal(err)
	}
	ep, epSecret, err := store.CreateOutcomeEndpoint(ctx, tenant, alice.ID, "crm")
	if err != nil {
		t.Fatal(err)
	}

	// The database holds ciphertext only.
	var plainTargets, plainSecrets int
	_ = owner.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE target IS NOT NULL), count(*) FILTER (WHERE secret IS NOT NULL) FROM control.alert_channels`).Scan(&plainTargets, &plainSecrets)
	var plainEp int
	_ = owner.QueryRowContext(ctx, `SELECT count(*) FROM control.outcome_endpoints WHERE secret IS NOT NULL`).Scan(&plainEp)
	if plainTargets != 0 || plainSecrets != 0 || plainEp != 0 {
		t.Fatalf("plaintext stored: targets %d secrets %d endpoints %d", plainTargets, plainSecrets, plainEp)
	}
	var dump string
	_ = owner.QueryRowContext(ctx, `SELECT string_agg(encode(target_enc, 'escape') || COALESCE(encode(secret_enc, 'escape'), ''), ' ') FROM control.alert_channels`).Scan(&dump)
	if bytes.Contains([]byte(dump), []byte("xyz123secret")) || bytes.Contains([]byte(dump), []byte(hookSecret)) {
		t.Fatal("ciphertext contains plaintext")
	}

	// Listing needs no decryption; the masked target is stored.
	list, err := store.ListAlertChannels(ctx, tenant)
	if err != nil || len(list) != 2 || list[0].Target != "" || list[0].Display == "" {
		t.Fatalf("list: %+v %v", list, err)
	}
	// The alerts worker gets decrypted channels.
	as := &AlertStore{DB: alertsDB, Secrets: testSecrets}
	chs, err := as.Channels(ctx, tenant)
	if err != nil || len(chs) != 2 {
		t.Fatalf("deliverable: %+v %v", chs, err)
	}
	byID := map[string]alerts.Channel{chs[0].ID: chs[0], chs[1].ID: chs[1]}
	if byID[slack.ID].Target != slackURL || byID[hook.ID].Secret != hookSecret || byID[hook.ID].Target != "https://example.com/hook?token=abc" {
		t.Fatalf("decrypted: %+v", byID)
	}
	// A test delivery reads one channel decrypted.
	if got, err := store.GetAlertChannel(ctx, tenant, hook.ID); err != nil || got.Secret != hookSecret {
		t.Fatalf("get: %+v %v", got, err)
	}
	// Ingest decrypts the endpoint secret.
	oi := &OutcomeIngest{DB: ingestDB, Secrets: testSecrets}
	if tn, sec, ok, err := oi.EndpointSecret(ctx, ep.ID); err != nil || !ok || tn != tenant || sec != epSecret {
		t.Fatalf("endpoint secret: %q %q %v %v", tn, sec, ok, err)
	}

	// A ciphertext moved to another row no longer decrypts.
	if _, err := owner.ExecContext(ctx, `UPDATE control.alert_channels SET target_enc = (SELECT target_enc FROM control.alert_channels WHERE id = $1) WHERE id = $2`, slack.ID, hook.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAlertChannel(ctx, tenant, hook.ID); err == nil {
		t.Fatal("swapped ciphertext decrypted")
	}

	// Legacy plaintext rows are read as before, then migrated.
	if _, err := owner.ExecContext(ctx, `INSERT INTO control.alert_channels (id, tenant_id, kind, name, target, secret) VALUES ('ch_legacy1', $1, 'webhook', 'old', 'https://example.org/old', 'whsec_legacy')`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ExecContext(ctx, `INSERT INTO control.outcome_endpoints (id, tenant_id, name, secret) VALUES ('oep_00000000000000aa', $1, 'old', 'obsec_legacy')`, tenant); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetAlertChannel(ctx, tenant, "ch_legacy1"); err != nil || got.Secret != "whsec_legacy" || got.Display != "https://example.org/…" {
		t.Fatalf("legacy read: %+v %v", got, err)
	}
	if _, sec, ok, err := oi.EndpointSecret(ctx, "oep_00000000000000aa"); err != nil || !ok || sec != "obsec_legacy" {
		t.Fatalf("legacy endpoint: %q %v %v", sec, ok, err)
	}
	n, err := EncryptLegacySecrets(ctx, owner, testSecrets)
	if err != nil || n.AlertChannels != 1 || n.OutcomeEndpoints != 1 {
		t.Fatalf("encrypt legacy: %+v %v", n, err)
	}
	if again, err := EncryptLegacySecrets(ctx, owner, testSecrets); err != nil || again.AlertChannels != 0 || again.OutcomeEndpoints != 0 {
		t.Fatalf("not idempotent: %+v %v", again, err)
	}
	_ = owner.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE target IS NOT NULL OR secret IS NOT NULL) FROM control.alert_channels`).Scan(&plainTargets)
	_ = owner.QueryRowContext(ctx, `SELECT count(*) FROM control.outcome_endpoints WHERE secret IS NOT NULL`).Scan(&plainEp)
	if plainTargets != 0 || plainEp != 0 {
		t.Fatalf("plaintext left after migration: %d %d", plainTargets, plainEp)
	}
	if got, err := store.GetAlertChannel(ctx, tenant, "ch_legacy1"); err != nil || got.Secret != "whsec_legacy" || got.Target != "https://example.org/old" {
		t.Fatalf("migrated read: %+v %v", got, err)
	}
	if _, sec, _, err := oi.EndpointSecret(ctx, "oep_00000000000000aa"); err != nil || sec != "obsec_legacy" {
		t.Fatalf("migrated endpoint: %q %v", sec, err)
	}
	if _, err := EncryptLegacySecrets(ctx, owner, nil); !errors.Is(err, secretbox.ErrNotConfigured) {
		t.Fatalf("no key: %v", err)
	}
}
