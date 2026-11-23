package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/alerts"
	"github.com/bindle-ai/bindle/backend/internal/passport"
)

func mustPassport(t *testing.T, doc string) *passport.Passport {
	t.Helper()
	p, err := passport.Parse([]byte(doc), nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIntegrationMgmtKeysAndPassports(t *testing.T) {
	ctx := context.Background()
	owner, apiDB, ingestDB := newTestDB(t)
	store := &Store{DB: apiDB}
	workspace := func(sub string) (string, string) {
		u, err := store.EnsureUser(ctx, sub, sub+"@example.com")
		if err != nil {
			t.Fatal(err)
		}
		m, err := store.Provision(ctx, u, sub)
		if err != nil {
			t.Fatal(err)
		}
		return m.TenantID, u.ID
	}
	ta, ua := workspace("sub-pass-a")
	tb, _ := workspace("sub-pass-b")

	// Management keys: scopes validated, secret shown once, hash stored.
	if _, _, err := store.CreateMgmtKey(ctx, ta, ua, "ci", []string{"admin"}); err == nil {
		t.Fatal("unknown scope accepted")
	}
	if _, _, err := store.CreateMgmtKey(ctx, ta, ua, "ci", nil); err == nil {
		t.Fatal("empty scopes accepted")
	}
	k, secret, err := store.CreateMgmtKey(ctx, ta, ua, "ci", []string{ScopeVaultSync, ScopePassportWrite, ScopePassportWrite})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, MgmtKeyPrefix) || k.DisplayPrefix != secret[:len(MgmtKeyPrefix)+8] ||
		strings.Join(k.Scopes, ",") != "passport:write,vault:sync" {
		t.Fatalf("key %+v", k)
	}
	var stored int
	if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM control.mgmt_keys WHERE key_hash = $1`, []byte(secret)).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("raw secret stored: %d %v", stored, err)
	}
	if list, err := store.ListMgmtKeys(ctx, tb); err != nil || len(list) != 0 {
		t.Fatalf("tenant B sees %d of A's keys (%v)", len(list), err)
	}
	if err := store.RevokeMgmtKey(ctx, tb, k.ID); err != ErrNotFound {
		t.Fatalf("tenant B revoked A's key: %v", err)
	}

	res := &MgmtKeyResolver{DB: ingestDB, TTL: time.Nanosecond, NegativeTTL: time.Nanosecond}
	p, ok, err := res.Resolve(ctx, secret)
	if err != nil || !ok || p.TenantID != ta || p.KeyID != k.ID || !p.Has(ScopePassportWrite) {
		t.Fatalf("resolve %+v %v %v", p, ok, err)
	}
	// An ingest key never resolves as a management key.
	_, ingestSecret, err := store.CreateKey(ctx, ta, ua, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := res.Resolve(ctx, ingestSecret); ok || err != nil {
		t.Fatalf("ingest key resolved as management key: %v %v", ok, err)
	}
	if _, ok, _ := res.Resolve(ctx, MgmtKeyPrefix+"unknown"); ok {
		t.Fatal("unknown key resolved")
	}

	// Passports: versions, no-op re-push, history, isolation.
	w := &PassportWriter{DB: ingestDB}
	v1 := mustPassport(t, `{"version":"bindle.passport/v1","agent":"refunds","owner":"payments","tools":{"allow":["lookup_*"]}}`)
	r1, err := w.Push(ctx, ta, v1, "key:"+k.ID)
	if err != nil || r1.Version != 1 || r1.Unchanged {
		t.Fatalf("push 1: %+v %v", r1, err)
	}
	// Same document, different formatting: no new version.
	again := mustPassport(t, `{ "agent":"refunds", "version":"bindle.passport/v1", "owner":"payments", "tools":{"allow":["lookup_*"]} }`)
	if r, err := w.Push(ctx, ta, again, "key:"+k.ID); err != nil || r.Version != 1 || !r.Unchanged {
		t.Fatalf("re-push: %+v %v", r, err)
	}
	v2 := mustPassport(t, `{"version":"bindle.passport/v1","agent":"refunds","owner":"payments","tools":{"allow":[]}}`)
	if r, err := w.Push(ctx, ta, v2, "key:"+k.ID); err != nil || r.Version != 2 {
		t.Fatalf("push 2: %+v %v", r, err)
	}
	if _, err := w.Push(ctx, tb, mustPassport(t, `{"version":"bindle.passport/v1","agent":"other","owner":"o"}`), "key:x"); err != nil {
		t.Fatal(err)
	}
	hist, err := store.PassportHistory(ctx, ta, "refunds")
	if err != nil || len(hist) != 2 || hist[0].Version != 2 || hist[0].PushedBy != "key:"+k.ID || hist[0].Passport.Tools.Allow == nil || len(hist[0].Passport.Tools.Allow) != 0 {
		t.Fatalf("history %+v %v", hist, err)
	}
	if list, err := store.ListPassports(ctx, ta); err != nil || len(list) != 1 || list[0].Version != 2 {
		t.Fatalf("list %+v %v", list, err)
	}
	if list, err := store.ListPassports(ctx, tb); err != nil || len(list) != 1 || list[0].Agent != "other" {
		t.Fatalf("tenant B list %+v %v", list, err)
	}
	if h, err := store.PassportHistory(ctx, tb, "refunds"); err != nil || len(h) != 0 {
		t.Fatalf("tenant B sees A's passport history: %d %v", len(h), err)
	}
	set, err := ActivePassports(ctx, ingestDB, ta)
	if err != nil || len(set) != 1 || set["refunds"].Tools.Allow == nil {
		t.Fatalf("active %v %v", set, err)
	}
	var lastUsed *time.Time
	if keys, _ := store.ListMgmtKeys(ctx, ta); len(keys) == 1 {
		lastUsed = keys[0].LastUsedAt
	}
	if lastUsed == nil {
		t.Fatal("push didn't record key use")
	}

	// Append-only, and no app role writes passports directly.
	for _, q := range []string{`UPDATE control.passports SET pushed_by = 'x'`, `DELETE FROM control.passports`, `TRUNCATE control.passports`} {
		if _, err := owner.ExecContext(ctx, q); err == nil {
			t.Errorf("owner ran %q", q)
		}
	}
	if _, err := apiDB.ExecContext(ctx, `INSERT INTO control.passports (tenant_id, agent, version, document, sha256, pushed_by) VALUES ($1, 'x', 1, '{}', $2, 'x')`,
		ta, make([]byte, 32)); err == nil {
		t.Error("api role inserted a passport")
	}

	// The alerts role sees tenants with passports and the feature on.
	alertsDB := open(t, lastAlertsDSN)
	reader := &PassportReader{DB: alertsDB}
	if got, err := reader.Tenants(ctx); err != nil || len(got) != 0 {
		t.Fatalf("tenants before the feature: %v %v", got, err)
	}
	if err := SetFeature(ctx, owner, ta, "passports", true, "ops"); err != nil {
		t.Fatal(err)
	}
	if got, err := reader.Tenants(ctx); err != nil || len(got) != 1 || got[0] != ta {
		t.Fatalf("tenants %v %v", got, err)
	}
	if set, err := reader.Passports(ctx, ta); err != nil || set.For("refunds") == nil {
		t.Fatalf("alerts passports %v %v", set, err)
	}

	// The resolver only returns passports with the feature on.
	pr := &PassportResolver{DB: ingestDB, Features: &FeatureResolver{DB: ingestDB, TTL: time.Nanosecond}, TTL: time.Nanosecond}
	if pr.Passports(ctx, ta).For("refunds") == nil {
		t.Fatal("resolver: tenant A has a passport")
	}
	if pr.Passports(ctx, tb) != nil {
		t.Fatal("resolver: tenant B has the feature off")
	}

	// Migration 014 accepts the passport_drift alert kind.
	if _, err := store.CreateAlertRule(ctx, ta, ua, alerts.Rule{Kind: alerts.KindPassportDrift, Threshold: 1, WindowMinutes: 60, Enabled: true}); err != nil {
		t.Fatalf("passport_drift rule: %v", err)
	}

	// Revoked keys stop working.
	if err := store.RevokeMgmtKey(ctx, ta, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := res.Resolve(ctx, secret); ok {
		t.Fatal("revoked key still resolves")
	}
}
