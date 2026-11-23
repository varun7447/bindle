package control

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestIntegrationFeatureFlags(t *testing.T) {
	ctx := context.Background()
	owner, apiDB, ingestDB := newTestDB(t)
	store := &Store{DB: apiDB}
	u, err := store.EnsureUser(ctx, "sub-flags", "flags@example.com")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.Provision(ctx, u, "Flags workspace")
	if err != nil {
		t.Fatal(err)
	}
	tenant := m.TenantID
	r := &FeatureResolver{DB: ingestDB, TTL: time.Nanosecond}
	if r.Enabled(ctx, tenant, "ledger") {
		t.Fatal("features must be off by default")
	}
	if err := SetFeature(ctx, owner, tenant, "ledger", true, "ops"); err != nil {
		t.Fatal(err)
	}
	if err := SetFeature(ctx, owner, tenant, "ledger", true, "ops"); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := SetFeature(ctx, owner, tenant, "teleport", true, "ops"); err == nil {
		t.Fatal("unknown feature accepted")
	}
	if err := SetFeature(ctx, owner, "no-such-tenant", "ledger", true, "ops"); err == nil {
		t.Fatal("unknown tenant accepted")
	}
	if !r.Enabled(ctx, tenant, "ledger") || r.Enabled(ctx, tenant, "passports") {
		t.Fatal("ledger should be on, passports off")
	}
	if got, err := (&FeatureResolver{DB: apiDB}).List(ctx, tenant); err != nil || len(got) != 1 || got[0] != "ledger" {
		t.Fatalf("api list %v %v", got, err)
	}
	if flags, err := ListFeatures(ctx, owner, ""); err != nil || len(flags) != 1 || flags[0].EnabledBy != "ops" {
		t.Fatalf("ListFeatures %+v %v", flags, err)
	}
	// No app role can write flags, and the API role sees no rows outside a
	// tenant-scoped transaction.
	for name, db := range map[string]*sql.DB{"api": apiDB, "ingest": ingestDB} {
		if _, err := db.ExecContext(ctx, `INSERT INTO control.tenant_features (tenant_id, feature, enabled_by) VALUES ($1, 'passports', 'x')`, tenant); err == nil {
			t.Errorf("%s role enabled a feature", name)
		}
	}
	var visible int
	if err := apiDB.QueryRowContext(ctx, `SELECT count(*) FROM control.tenant_features`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("api role sees %d flag rows without a tenant: %v", visible, err)
	}
	if err := SetFeature(ctx, owner, tenant, "ledger", false, ""); err != nil {
		t.Fatal(err)
	}
	if r.Enabled(ctx, tenant, "ledger") {
		t.Fatal("ledger still on after disable")
	}
}

func TestFeatureResolverCachesAndFailsClosed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := &FeatureResolver{Now: func() time.Time { return now }}
	if r.Enabled(context.Background(), "t", "ledger") {
		t.Fatal("no database: features must be off")
	}
	r.cache = map[string]featureEntry{"t": {on: map[string]bool{"ledger": true}, at: now}}
	r.DB = &sql.DB{} // never queried while the cache is fresh
	if !r.Enabled(context.Background(), "t", "ledger") {
		t.Fatal("fresh cache ignored")
	}
	if !IsFeature("learning_loop") || IsFeature("teleport") {
		t.Fatal("IsFeature")
	}
}
