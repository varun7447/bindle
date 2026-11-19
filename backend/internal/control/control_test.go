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

	"github.com/bindle-ai/bindle/backend/internal/evals"
	"github.com/bindle-ai/bindle/backend/internal/sdkconfig"
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
	if got := renderMigration("GRANT x TO {{api}}, {{ingest}}, {{processor}};", Roles{API: "a_r", Ingest: "i_r", Processor: "p_r"}); got != `GRANT x TO "a_r", "i_r", "p_r";` {
		t.Errorf("rendered %q", got)
	}
	for _, r := range []Roles{{API: "x", Ingest: "x", Processor: "p", Alerts: "al"}, {API: "Bad Name", Ingest: "i", Processor: "p", Alerts: "al"}, {API: "a;drop", Ingest: "i", Processor: "p", Alerts: "al"}, {API: "a", Ingest: "i", Processor: "p"}} {
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
	roles := Roles{API: "itest_api_" + sfx, Ingest: "itest_ingest_" + sfx, Processor: "itest_proc_" + sfx, Alerts: "itest_alerts_" + sfx,
		APIPassword: randHex(12), IngestPassword: randHex(12), ProcessorPassword: randHex(12), AlertsPassword: randHex(12)}
	root := open(t, admin)
	if _, err := root.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(dbName)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = root.ExecContext(ctx, "DROP DATABASE IF EXISTS "+pq.QuoteIdentifier(dbName)+" WITH (FORCE)")
		for _, r := range []string{roles.API, roles.Ingest, roles.Processor, roles.Alerts} {
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
	lastProcessorDSN = withDB(t, admin, dbName, roles.Processor, roles.ProcessorPassword)
	lastAlertsDSN = withDB(t, admin, dbName, roles.Alerts, roles.AlertsPassword)
	return owner, open(t, withDB(t, admin, dbName, roles.API, roles.APIPassword)), open(t, withDB(t, admin, dbName, roles.Ingest, roles.IngestPassword))
}

// lastProcessorDSN is the processor-role URL of the most recent newTestDB.
var lastProcessorDSN, lastAlertsDSN string

func TestIntegrationLeases(t *testing.T) {
	ctx := context.Background()
	_, apiDB, ingestDB := newTestDB(t)
	l := &Leases{DB: open(t, lastProcessorDSN)}
	if _, ok, err := l.Acquire(ctx, "s", "shard-0", "a", time.Minute); !ok || err != nil {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	if _, ok, _ := l.Acquire(ctx, "s", "shard-0", "b", time.Minute); ok {
		t.Fatal("b took a live lease")
	}
	if err := l.Checkpoint(ctx, "s", "shard-0", "a", "seq-1", false); err != nil {
		t.Fatal(err)
	}
	if err := l.Checkpoint(ctx, "s", "shard-0", "b", "seq-9", false); err == nil {
		t.Fatal("non-owner checkpointed")
	}
	if ok, _ := l.Renew(ctx, "s", "shard-0", "b", time.Minute); ok {
		t.Fatal("non-owner renewed")
	}
	if _, ok, _ := l.Acquire(ctx, "s", "shard-1", "b", time.Millisecond); !ok {
		t.Fatal("acquire shard-1")
	}
	time.Sleep(20 * time.Millisecond)
	lease, ok, err := l.Acquire(ctx, "s", "shard-0", "a", time.Minute)
	if !ok || err != nil || lease.Checkpoint != "seq-1" {
		t.Fatalf("re-acquire own lease: %+v %v %v", lease, ok, err)
	}
	if _, ok, _ := l.Acquire(ctx, "s", "shard-1", "a", time.Minute); !ok {
		t.Fatal("expired lease not taken over")
	}
	if err := l.Checkpoint(ctx, "s", "shard-1", "a", "end", true); err != nil {
		t.Fatal(err)
	}
	if done, _ := l.Finished(ctx, "s", "shard-1"); !done {
		t.Fatal("finished")
	}
	for _, db := range []*sql.DB{apiDB, ingestDB} {
		if _, err := db.ExecContext(ctx, "SELECT * FROM control.stream_leases"); err == nil {
			t.Error("non-processor role can read leases")
		}
	}
	if _, err := l.DB.ExecContext(ctx, "SELECT * FROM control.api_keys"); err == nil {
		t.Error("processor role can read keys")
	}
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

func TestIntegrationLimitsAndBudgets(t *testing.T) {
	ctx := context.Background()
	_, apiDB, ingestDB := newTestDB(t)
	alertsDB := open(t, lastAlertsDSN)
	store := &Store{DB: apiDB}
	alice, _ := store.EnsureUser(ctx, "sub-a", "")
	ma, _ := store.Provision(ctx, alice, "A")
	bob, _ := store.EnsureUser(ctx, "sub-b", "")
	mb, _ := store.Provision(ctx, bob, "B")

	limits := []sdkconfig.Limit{{Agent: "*", MaxStepsPerRun: 100}, {Agent: "refunds", DailyBudgetUSD: 5, PauseOverBudget: true}}
	saved, err := store.PutLimits(ctx, ma.TenantID, alice.ID, 0, limits)
	if err != nil || saved.Version != 1 || len(saved.Limits) != 2 || saved.SampleRate != 1 || !saved.CaptureContent {
		t.Fatalf("first limits save: %+v %v", saved, err)
	}
	if _, err := store.PutLimits(ctx, ma.TenantID, alice.ID, 0, limits); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale limits save must conflict: %v", err)
	}
	// Saving SDK settings keeps limits, and bumps the shared version.
	saved.SampleRate = 0.5
	cfg, err := store.PutSDKConfig(ctx, ma.TenantID, alice.ID, saved)
	if err != nil || cfg.Version != 2 {
		t.Fatalf("config save: %+v %v", cfg, err)
	}
	if got, _ := store.GetSDKConfig(ctx, ma.TenantID); len(got.Limits) != 2 || got.Limits[1].DailyBudgetUSD != 5 || got.SampleRate != 0.5 {
		t.Fatalf("limits lost after config save: %+v", got)
	}
	if _, err := store.PutLimits(ctx, ma.TenantID, alice.ID, 2, []sdkconfig.Limit{{Agent: ""}}); err == nil {
		t.Fatal("invalid limits saved")
	}

	// Budget state: alerts role finds the tenant and records once.
	as := &AlertStore{DB: alertsDB}
	bts, err := as.BudgetTenants(ctx)
	if err != nil || len(bts) != 1 || bts[0].Tenant != ma.TenantID || len(bts[0].Limits) != 2 {
		t.Fatalf("budget tenants: %+v %v", bts, err)
	}
	today := time.Now().UTC()
	if created, err := as.RecordBudgetExceeded(ctx, ma.TenantID, "refunds", today, 5.2, 5); err != nil || !created {
		t.Fatalf("record: %v %v", created, err)
	}
	if created, err := as.RecordBudgetExceeded(ctx, ma.TenantID, "refunds", today, 6, 5); err != nil || created {
		t.Fatalf("second record must be a no-op: %v %v", created, err)
	}
	if got, _ := store.GetSDKConfig(ctx, ma.TenantID); len(got.OverBudget) != 1 || got.OverBudget[0] != "refunds" {
		t.Fatalf("over budget (api): %+v", got.OverBudget)
	}
	if got, _ := store.GetSDKConfig(ctx, mb.TenantID); len(got.OverBudget) != 0 || len(got.Limits) != 0 {
		t.Fatalf("tenant B sees A's limits or budget state: %+v", got)
	}
	r := &ConfigResolver{DB: ingestDB}
	res, err := r.Policy(ctx, ma.TenantID)
	if err != nil || len(res.Config.Limits) != 2 || len(res.Config.OverBudget) != 1 || res.Config.Version != 2 {
		t.Fatalf("resolver: %+v %v", res.Config, err)
	}
	// Prices share the version and reach ingest compiled.
	pr, err := store.PutPrices(ctx, ma.TenantID, alice.ID, 2, []sdkconfig.PriceOverride{{Model: "gpt-x", InputPerMTok: 1, OutputPerMTok: 2}})
	if err != nil || pr.Version != 3 || len(pr.Prices) != 1 || len(pr.Limits) != 2 {
		t.Fatalf("prices save: %+v %v", pr, err)
	}
	r2 := &ConfigResolver{DB: ingestDB}
	res2, err := r2.Policy(ctx, ma.TenantID)
	if err != nil || res2.Prices == nil {
		t.Fatalf("resolver prices: %+v %v", res2, err)
	}
	if c, ok := res2.Prices.Compute("openai", "gpt-x", 1_000_000, 0, 0); !ok || c != 1 {
		t.Errorf("resolved price: %v %v", c, ok)
	}
	if res, _ := r2.Policy(ctx, mb.TenantID); res.Prices != nil {
		t.Error("tenant B got A's prices")
	}
	if _, err := alertsDB.ExecContext(ctx, "UPDATE control.sdk_config SET limits = '[]'"); err == nil {
		t.Fatal("alerts role can change limits")
	}
	if _, err := apiDB.ExecContext(ctx, "DELETE FROM control.budget_state"); err == nil {
		t.Fatal("api role can delete budget state")
	}
}

func TestIntegrationRunStops(t *testing.T) {
	ctx := context.Background()
	_, apiDB, ingestDB := newTestDB(t)
	store := &Store{DB: apiDB}
	alice, _ := store.EnsureUser(ctx, "sub-a", "")
	ma, _ := store.Provision(ctx, alice, "A")
	bob, _ := store.EnsureUser(ctx, "sub-b", "")
	mb, _ := store.Provision(ctx, bob, "B")
	trace := "0af7651916cd43dd8448eb211c80319c"

	if _, ok, err := store.GetRunStop(ctx, ma.TenantID, trace); err != nil || ok {
		t.Fatalf("not stopped yet: %v %v", ok, err)
	}
	first, err := store.StopRun(ctx, ma.TenantID, alice.ID, trace)
	if err != nil || first.TraceID != trace || first.StoppedAt.IsZero() {
		t.Fatalf("stop: %+v %v", first, err)
	}
	again, err := store.StopRun(ctx, ma.TenantID, alice.ID, trace)
	if err != nil || !again.StoppedAt.Equal(first.StoppedAt) {
		t.Fatalf("second stop must keep the first time: %+v %v", again, err)
	}
	if _, err := store.StopRun(ctx, ma.TenantID, alice.ID, "not-a-trace"); err == nil {
		t.Fatal("invalid trace ID accepted")
	}
	if _, ok, _ := store.GetRunStop(ctx, mb.TenantID, trace); ok {
		t.Fatal("tenant B sees A's stop")
	}
	// Tenant A never saved settings: ingest still gets the stop, with defaults.
	r := &ConfigResolver{DB: ingestDB}
	res, err := r.Policy(ctx, ma.TenantID)
	if err != nil || len(res.Config.StoppedRuns) != 1 || res.Config.StoppedRuns[0] != trace ||
		res.Config.Version != 0 || res.Config.SampleRate != 1 || !res.Config.CaptureContent {
		t.Fatalf("resolver: %+v %v", res.Config, err)
	}
	if res, _ := r.Policy(ctx, mb.TenantID); len(res.Config.StoppedRuns) != 0 {
		t.Fatalf("tenant B got A's stops: %v", res.Config.StoppedRuns)
	}
	if _, err := ingestDB.ExecContext(ctx, "SELECT 1 FROM control.run_stops"); err == nil {
		t.Fatal("ingest role can read run_stops directly")
	}
}

func TestIntegrationEvals(t *testing.T) {
	ctx := context.Background()
	_, apiDB, _ := newTestDB(t)
	store := &Store{DB: apiDB}
	alice, _ := store.EnsureUser(ctx, "sub-a", "")
	ma, _ := store.Provision(ctx, alice, "A")
	bob, _ := store.EnsureUser(ctx, "sub-b", "")
	mb, _ := store.Provision(ctx, bob, "B")

	sc, err := store.CreateScorer(ctx, ma.TenantID, alice.ID, evals.Scorer{Name: "mentions refund", Kind: evals.KindContains, Value: "refund", Enabled: true})
	if err != nil || sc.ID == "" {
		t.Fatalf("create scorer: %+v %v", sc, err)
	}
	if _, err := store.CreateScorer(ctx, ma.TenantID, alice.ID, evals.Scorer{Name: "mentions refund", Kind: evals.KindJSONValid}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate name: %v", err)
	}
	if _, err := store.CreateScorer(ctx, ma.TenantID, alice.ID, evals.Scorer{Name: "bad", Kind: evals.KindRegex, Value: "(a+)+"}); !evals.IsInvalid(err) {
		t.Errorf("invalid scorer: %v", err)
	}
	up, err := store.UpdateScorer(ctx, ma.TenantID, sc.ID, evals.Scorer{Name: "says refund", Kind: evals.KindJSONValid, Value: "refund", Enabled: false})
	if err != nil || up.Kind != evals.KindContains || up.Name != "says refund" || up.Enabled {
		t.Fatalf("update keeps kind: %+v %v", up, err)
	}
	if got, _ := store.ListScorers(ctx, mb.TenantID); len(got) != 0 {
		t.Fatalf("tenant B sees A's checks: %+v", got)
	}
	if err := store.DeleteScorer(ctx, mb.TenantID, sc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant B deleted A's check: %v", err)
	}

	ds, err := store.CreateDataset(ctx, ma.TenantID, alice.ID, "  regressions ", "refund edge cases")
	if err != nil || ds.Name != "regressions" {
		t.Fatalf("dataset: %+v %v", ds, err)
	}
	if _, err := store.CreateDataset(ctx, ma.TenantID, alice.ID, "regressions", ""); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate dataset: %v", err)
	}
	runs := []evals.Run{
		{TraceID: strings.Repeat("a", 32), Agent: "refunds", Version: "v1", Input: "where is my refund", Output: strings.Repeat("x", 70<<10), Tools: []string{"lookup_order"}, Steps: 5, CostKnown: true},
		{TraceID: strings.Repeat("b", 32), Agent: "refunds", Version: "v1", Output: "done"},
	}
	if n, err := store.AddDatasetItems(ctx, ma.TenantID, ds.ID, runs); err != nil || n != 2 {
		t.Fatalf("add: %d %v", n, err)
	}
	if n, err := store.AddDatasetItems(ctx, ma.TenantID, ds.ID, runs[:1]); err != nil || n != 0 {
		t.Fatalf("re-add is a no-op: %d %v", n, err)
	}
	if _, err := store.AddDatasetItems(ctx, mb.TenantID, ds.ID, runs); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant B added to A's dataset: %v", err)
	}
	d, items, err := store.GetDataset(ctx, ma.TenantID, ds.ID)
	if err != nil || d.Items != 2 || len(items[0].Output) != maxItemText || items[0].Tools[0] != "lookup_order" || !items[0].CostKnown {
		t.Fatalf("get: %+v %d %v", d, len(items), err)
	}
	ev, err := store.RecordEvalRun(ctx, ma.TenantID, alice.ID, ds.ID, 2, []evals.Tally{{ScorerID: sc.ID, Name: "says refund", Passed: 1, Failed: 1}},
		[]Failure{{TraceID: runs[1].TraceID, ScorerID: sc.ID, Scorer: "says refund", Detail: "output doesn't contain \"refund\""}})
	if err != nil || ev.ID == "" {
		t.Fatalf("record: %v", err)
	}
	list, err := store.ListDatasets(ctx, ma.TenantID)
	if err != nil || len(list) != 1 || list[0].Items != 2 || list[0].LastEval == nil || list[0].LastEval.Tallies[0].Failed != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if evs, _ := store.ListEvalRuns(ctx, ma.TenantID, ds.ID); len(evs) != 1 || len(evs[0].Failures) != 1 {
		t.Fatalf("eval runs: %+v", evs)
	}
	if got, _ := store.ListDatasets(ctx, mb.TenantID); len(got) != 0 {
		t.Fatal("tenant B sees A's datasets")
	}
	if err := store.RemoveDatasetItem(ctx, ma.TenantID, ds.ID, runs[1].TraceID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteDataset(ctx, ma.TenantID, ds.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.GetDataset(ctx, ma.TenantID, ds.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted dataset: %v", err)
	}
}
