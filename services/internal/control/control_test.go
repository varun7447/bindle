package control

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/bindle-ai/bindle/services/internal/sdkconfig"
)

func TestKeyFormat(t *testing.T) {
	k, display, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k, KeyPrefix) || len(k) != len(KeyPrefix)+52 || display != k[:16] || strings.ToLower(k) != k {
		t.Fatalf("key %q display %q", k, display)
	}
	k2, _, _ := newKey()
	if k == k2 {
		t.Fatal("keys must be random")
	}
	if len(HashKey(k)) != 32 || hex.EncodeToString(HashKey(k)) == hex.EncodeToString(HashKey(k2)) {
		t.Fatal("hash")
	}
}

func TestRenderAndRoleValidation(t *testing.T) {
	if got := renderMigration("GRANT x TO {{api}}, {{ingest}};", Roles{API: "a_r", Ingest: "i_r"}); got != `GRANT x TO "a_r", "i_r";` {
		t.Errorf("rendered %q", got)
	}
	for _, r := range []Roles{{API: "x", Ingest: "x"}, {API: "Bad Name", Ingest: "i"}, {API: "a;drop", Ingest: "i"}} {
		if _, err := Migrate(context.Background(), nil, r); err == nil {
			t.Errorf("accepted roles %+v", r)
		}
	}
}

func TestCleanName(t *testing.T) {
	if cleanName("  ") != "default" || len(cleanName(strings.Repeat("x", 500))) != maxKeyName {
		t.Fatal("cleanName")
	}
}

// Runs only when BINDLE_POSTGRES_TEST_URL is set to a superuser URL. It
// creates and drops its own database and roles.
func TestIntegrationControlPlane(t *testing.T) {
	ctx := context.Background()
	owner, apiDB, ingestDB := newTestDB(t)
	store := &Store{DB: apiDB}

	// First sign-in creates the user and, concurrently, exactly one tenant.
	alice, err := store.EnsureUser(ctx, "sub-alice", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := store.EnsureUser(ctx, "sub-alice", "alice@example.com"); again.ID != alice.ID {
		t.Fatal("EnsureUser not idempotent")
	}
	var wg sync.WaitGroup
	tenants := make([]string, 8)
	for i := range tenants {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, err := store.Provision(ctx, alice, "Alice's workspace")
			if err != nil {
				t.Error(err)
			}
			tenants[i] = m.TenantID
		}(i)
	}
	wg.Wait()
	for _, tn := range tenants {
		if tn != tenants[0] || tn == "" {
			t.Fatalf("concurrent provisioning created several tenants: %v", tenants)
		}
	}
	m, err := store.MembershipFor(ctx, alice.ID)
	if err != nil || m.Role != RoleOwner || !m.CanManageKeys() {
		t.Fatalf("membership %+v %v", m, err)
	}
	bob, _ := store.EnsureUser(ctx, "sub-bob", "bob@example.com")
	mb, _ := store.Provision(ctx, bob, "Bob's workspace")

	// Key lifecycle.
	k1, secret, err := store.CreateKey(ctx, m.TenantID, alice.ID, "prod agents")
	if err != nil || !strings.HasPrefix(secret, KeyPrefix) || k1.DisplayPrefix != secret[:16] {
		t.Fatalf("CreateKey: %+v %q %v", k1, secret, err)
	}
	var stored []byte
	if err := owner.QueryRowContext(ctx, "SELECT key_hash FROM control.api_keys WHERE id = $1", k1.ID).Scan(&stored); err != nil ||
		hex.EncodeToString(stored) != hex.EncodeToString(HashKey(secret)) {
		t.Fatal("only the hash must be stored")
	}
	var plaintextHits int
	_ = owner.QueryRowContext(ctx, "SELECT count(*) FROM control.api_keys WHERE position($1 in name || display_prefix) > 0", secret).Scan(&plaintextHits)
	if plaintextHits != 0 {
		t.Fatal("secret stored in plaintext")
	}

	now := time.Now()
	r := &KeyResolver{DB: ingestDB, Now: func() time.Time { return now }}
	if tn, ok := r.Resolve(ctx, secret); !ok || tn != m.TenantID {
		t.Fatalf("resolve: %q %v", tn, ok)
	}
	if _, ok := r.Resolve(ctx, KeyPrefix+"nope"); ok {
		t.Fatal("unknown key resolved")
	}
	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	if tn, ok := r.Tenant(req); !ok || tn != m.TenantID {
		t.Fatal("Tenant(req)")
	}
	if err := r.FlushUsage(ctx); err != nil {
		t.Fatalf("usage flush as ingest role: %v", err)
	}
	keys, err := store.ListKeys(ctx, m.TenantID)
	if err != nil || len(keys) != 1 || keys[0].LastUsedAt == nil {
		t.Fatalf("list: %+v %v", keys, err)
	}

	// Isolation: bob's tenant can't see or touch alice's keys.
	if keys, _ := store.ListKeys(ctx, mb.TenantID); len(keys) != 0 {
		t.Fatalf("bob sees %d keys", len(keys))
	}
	if err := store.RevokeKey(ctx, mb.TenantID, k1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant revoke: %v", err)
	}
	if _, _, err := store.RotateKey(ctx, mb.TenantID, bob.ID, k1.ID, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant rotate: %v", err)
	}

	// Rotate: new key works, old works during grace, then not after.
	k2, secret2, err := store.RotateKey(ctx, m.TenantID, alice.ID, k1.ID, time.Hour)
	if err != nil || k2.Name != "prod agents" || secret2 == secret {
		t.Fatalf("rotate: %+v %v", k2, err)
	}
	fresh := &KeyResolver{DB: ingestDB}
	if _, ok := fresh.Resolve(ctx, secret); !ok {
		t.Fatal("old key must work during the grace period")
	}
	if _, ok := fresh.Resolve(ctx, secret2); !ok {
		t.Fatal("rotated key must work")
	}
	if _, _, err := store.RotateKey(ctx, m.TenantID, alice.ID, k1.ID, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatal("rotating an already-rotated key must fail")
	}
	if _, err := owner.ExecContext(ctx, "UPDATE control.api_keys SET expires_at = now() - interval '1 second' WHERE id = $1", k1.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := (&KeyResolver{DB: ingestDB}).Resolve(ctx, secret); ok {
		t.Fatal("expired key still resolves")
	}

	// Revoke: takes effect once the cache entry expires.
	if err := store.RevokeKey(ctx, m.TenantID, k2.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Resolve(ctx, secret2); ok {
		// r cached nothing for secret2 yet, so this is a fresh lookup
		t.Fatal("revoked key resolves")
	}
	cached := &KeyResolver{DB: ingestDB, Now: func() time.Time { return now }}
	k3, secret3, _ := store.CreateKey(ctx, m.TenantID, alice.ID, "")
	if _, ok := cached.Resolve(ctx, secret3); !ok {
		t.Fatal("new key")
	}
	_ = store.RevokeKey(ctx, m.TenantID, k3.ID)
	if _, ok := cached.Resolve(ctx, secret3); !ok {
		t.Fatal("within TTL the cached answer is served")
	}
	now = now.Add(61 * time.Second)
	if _, ok := cached.Resolve(ctx, secret3); ok {
		t.Fatal("revocation must apply after the TTL")
	}

	// Privileges.
	for _, stmt := range []string{
		"SELECT * FROM control.api_keys", "SELECT * FROM control.users", "INSERT INTO control.tenants (id, name) VALUES ('x', 'x')",
	} {
		if _, err := ingestDB.ExecContext(ctx, stmt); err == nil {
			t.Errorf("ingest role could run %q", stmt)
		}
	}
	for _, stmt := range []string{
		"DELETE FROM control.api_keys", "DELETE FROM control.tenants", "UPDATE control.api_keys SET key_hash = '\\x00'",
		"UPDATE control.api_keys SET tenant_id = 'other'", "DROP TABLE control.api_keys",
	} {
		if _, err := apiDB.ExecContext(ctx, stmt); err == nil {
			t.Errorf("api role could run %q", stmt)
		}
	}
	var visible int
	if err := apiDB.QueryRowContext(ctx, "SELECT count(*) FROM control.api_keys").Scan(&visible); err != nil || visible != 0 {
		t.Errorf("api role without tenant saw %d keys (%v)", visible, err)
	}

	// Dev seeding (owner only) is idempotent.
	devKey := "bd_test_" + randHex(24)
	for i := 0; i < 2; i++ {
		if err := SeedKey(ctx, owner, "dev-tenant", "Local dev", devKey); err != nil {
			t.Fatal(err)
		}
	}
	if tn, ok := (&KeyResolver{DB: ingestDB}).Resolve(ctx, devKey); !ok || tn != "dev-tenant" {
		t.Fatalf("seeded key: %q %v", tn, ok)
	}
	if err := SeedKey(ctx, owner, "dev-tenant", "x", "short"); err == nil {
		t.Fatal("bad seed key accepted")
	}
}

func TestResolverServesStaleOnDBError(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := &KeyResolver{DB: db, Now: func() time.Time { return now }}
	h := hex.EncodeToString(HashKey("bd_live_known"))
	r.store(h, entry{tenant: "t", keyID: "k", ok: true, at: now})
	now = now.Add(2 * time.Minute) // past TTL: lookup fails, stale answer used
	if tn, ok := r.Resolve(context.Background(), "bd_live_known"); !ok || tn != "t" {
		t.Fatal("recently valid key should survive a DB outage")
	}
	now = now.Add(20 * time.Minute) // past StaleTTL
	if _, ok := r.Resolve(context.Background(), "bd_live_known"); ok {
		t.Fatal("stale answer served too long")
	}
	if _, ok := r.Resolve(context.Background(), "bd_live_unknown"); ok {
		t.Fatal("unknown key accepted during outage")
	}
}

func TestResolverCacheIsBounded(t *testing.T) {
	r := &KeyResolver{MaxEntries: 10}
	for i := 0; i < 100; i++ {
		r.store(randHex(8), entry{})
	}
	if len(r.cache) > 10 {
		t.Fatalf("cache grew to %d", len(r.cache))
	}
}

// newTestDB creates a migrated throwaway database with unique roles, or
// skips when BINDLE_POSTGRES_TEST_URL (a superuser URL) is not set.
func newTestDB(t *testing.T) (owner, apiDB, ingestDB *sql.DB) {
	t.Helper()
	admin := os.Getenv("BINDLE_POSTGRES_TEST_URL")
	if admin == "" {
		t.Skip("BINDLE_POSTGRES_TEST_URL not set")
	}
	ctx := context.Background()
	sfx := randHex(4)
	dbName := "control_itest_" + sfx
	roles := Roles{API: "itest_api_" + sfx, Ingest: "itest_ingest_" + sfx, APIPassword: randHex(12), IngestPassword: randHex(12)}
	root := open(t, admin)
	if _, err := root.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(dbName)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = root.ExecContext(ctx, "DROP DATABASE IF EXISTS "+pq.QuoteIdentifier(dbName)+" WITH (FORCE)")
		for _, r := range []string{roles.API, roles.Ingest} {
			_, _ = root.ExecContext(ctx, "DROP ROLE IF EXISTS "+pq.QuoteIdentifier(r))
		}
		root.Close()
	})
	owner = open(t, withDB(t, admin, dbName, "", ""))
	if applied, err := Migrate(ctx, owner, roles); err != nil || len(applied) == 0 {
		t.Fatalf("migrate: %v %v", applied, err)
	}
	if again, err := Migrate(ctx, owner, roles); err != nil || len(again) != 0 {
		t.Fatalf("migrate again: %v %v", again, err)
	}
	return owner, open(t, withDB(t, admin, dbName, roles.API, roles.APIPassword)), open(t, withDB(t, admin, dbName, roles.Ingest, roles.IngestPassword))
}

func TestIntegrationSDKConfig(t *testing.T) {
	ctx := context.Background()
	_, apiDB, ingestDB := newTestDB(t)
	store := &Store{DB: apiDB}
	alice, _ := store.EnsureUser(ctx, "sub-a", "")
	ma, _ := store.Provision(ctx, alice, "A")
	bob, _ := store.EnsureUser(ctx, "sub-b", "")
	mb, _ := store.Provision(ctx, bob, "B")

	c, err := store.GetSDKConfig(ctx, ma.TenantID)
	if err != nil || c.Version != 0 || c.SampleRate != 1 || !c.CaptureContent || len(c.RedactPatterns) != 0 {
		t.Fatalf("default: %+v %v", c, err)
	}
	c.SampleRate, c.CaptureContent = 0.25, false
	c.RedactPatterns = []sdkconfig.Pattern{{Name: "ORDER", Pattern: `ord_\d+`}}
	saved, err := store.PutSDKConfig(ctx, ma.TenantID, alice.ID, c)
	if err != nil || saved.Version != 1 {
		t.Fatalf("first save: %+v %v", saved, err)
	}
	if _, err := store.PutSDKConfig(ctx, ma.TenantID, alice.ID, c); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale save (version 0) must conflict: %v", err)
	}
	saved.SampleRate = 0.5
	saved2, err := store.PutSDKConfig(ctx, ma.TenantID, alice.ID, saved)
	if err != nil || saved2.Version != 2 {
		t.Fatalf("second save: %+v %v", saved2, err)
	}
	if _, err := store.PutSDKConfig(ctx, ma.TenantID, alice.ID, saved); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale save (version 1) must conflict: %v", err)
	}
	bad := saved2
	bad.RedactPatterns = []sdkconfig.Pattern{{Name: "X", Pattern: `(a+)+`}}
	if _, err := store.PutSDKConfig(ctx, ma.TenantID, alice.ID, bad); err == nil {
		t.Fatal("invalid pattern saved")
	}
	if got, _ := store.GetSDKConfig(ctx, mb.TenantID); got.Version != 0 {
		t.Fatalf("tenant B sees tenant A config: %+v", got)
	}

	now := time.Now()
	r := &ConfigResolver{DB: ingestDB, Now: func() time.Time { return now }}
	res, err := r.Policy(ctx, ma.TenantID)
	if err != nil || res.Config.Version != 2 || res.Config.SampleRate != 0.5 || res.Config.CaptureContent ||
		res.Redactor.Apply("ord_42") != "[REDACTED:ORDER]" {
		t.Fatalf("resolver: %+v %v", res.Config, err)
	}
	if res, err := r.Policy(ctx, mb.TenantID); err != nil || res.Config.Version != 0 || !res.Config.CaptureContent {
		t.Fatalf("defaults for B: %+v %v", res.Config, err)
	}
	if _, err := ingestDB.ExecContext(ctx, "SELECT * FROM control.sdk_config"); err == nil {
		t.Fatal("ingest role can read the config table directly")
	}
	if _, err := apiDB.ExecContext(ctx, "DELETE FROM control.sdk_config"); err == nil {
		t.Fatal("api role can delete configs")
	}
}

func TestConfigResolverOutage(t *testing.T) {
	db, _ := sql.Open("postgres", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	now := time.Now()
	r := &ConfigResolver{DB: db, Now: func() time.Time { return now }}
	if _, err := r.Policy(context.Background(), "t"); err == nil {
		t.Fatal("unknown tenant during outage must error, not default")
	}
	r.cache = map[string]cfgEntry{"t": {cfg: sdkconfig.Config{Version: 3, SampleRate: 0.1}, at: now}}
	now = now.Add(time.Minute)
	if res, err := r.Policy(context.Background(), "t"); err != nil || res.Config.Version != 3 {
		t.Fatalf("stale config should be served: %+v %v", res, err)
	}
	now = now.Add(time.Hour)
	if _, err := r.Policy(context.Background(), "t"); err == nil {
		t.Fatal("stale config served too long")
	}
}

func open(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db
}

func withDB(t *testing.T, dsn, db, user, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	return u.String()
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
