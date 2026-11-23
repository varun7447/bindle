package ledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/model"
)

// The canonical encoding is part of the evidence format: these vectors must
// never change for version 1.
func TestCanonicalVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/canonical_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Entry     Entry  `json:"entry"`
		Tenant    string `json:"tenant"`
		PrevHash  string `json:"prev_hash"`
		Canonical string `json:"canonical"`
		Hash      string `json:"hash"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) == 0 {
		t.Fatal("no vectors")
	}
	for i, v := range vectors {
		e := v.Entry
		e.TenantID = v.Tenant
		e.PrevHash, _ = hex.DecodeString(v.PrevHash)
		if got := string(Canonical(e)); got != v.Canonical {
			t.Errorf("vector %d canonical:\n got %s\nwant %s", i, got, v.Canonical)
		}
		if got := hex.EncodeToString(Hash(e)); got != v.Hash {
			t.Errorf("vector %d hash %s, want %s", i, got, v.Hash)
		}
	}
}

func TestCanonicalIsOrderIndependent(t *testing.T) {
	e := Entry{TenantID: "t", Agent: "a", Seq: 1, Day: "2026-11-23", At: time.Date(2026, 11, 23, 1, 2, 3, 4000, time.UTC),
		Kind: KindStep, SecurityFlags: []string{"secret_exposed", "prompt_injection"}, CostUSD: "0", Detail: "{}", PrevHash: Genesis}
	f := e
	f.SecurityFlags = []string{"prompt_injection", "secret_exposed"}
	if string(Canonical(e)) != string(Canonical(f)) {
		t.Fatal("flag order changed the canonical form")
	}
	g := e
	g.StepName = "<b>&"
	if !strings.Contains(string(Canonical(g)), `"step_name":"<b>&"`) {
		t.Fatalf("canonical form escapes HTML: %s", Canonical(g))
	}
}

func TestFromStepKeepsContentOut(t *testing.T) {
	st := model.Step{TenantID: "t1", TraceID: strings.Repeat("a", 32), StepID: strings.Repeat("b", 16), Name: "lookup_order",
		Type: model.StepTool, Status: model.StatusFailure, ToolName: "lookup_order", AgentName: "refund-agent", AgentVersion: "v3",
		Input: "my card is 4111 1111 1111 1111", Output: "secret output", Reasoning: "private reasoning",
		ErrorMessage: "timeout for jane@example.com after 20ms", ErrorPattern: "timeout for <email> after <n>ms",
		InputTokens: 12, OutputTokens: 3, CostUSD: 0.00042, CostKnown: true, StartUnixNano: 1_700_000_000_123_456_789,
		SecurityFlags: []string{"secret_exposed"}}
	e := FromStep(st)
	c := string(Canonical(e))
	for _, bad := range []string{"4111", "secret output", "private reasoning", "jane@example.com"} {
		if strings.Contains(c, bad) {
			t.Errorf("ledger entry contains %q: %s", bad, c)
		}
	}
	if e.ErrorClass != st.ErrorPattern || e.CostUSD != "0.00042" || e.At.Nanosecond()%1000 != 0 || e.Kind != KindStep {
		t.Fatalf("entry %+v", e)
	}
}

func TestFormatCost(t *testing.T) {
	for in, want := range map[float64]string{0: "0", -1: "0", 0.1: "0.1", 1e-9: "0.000000001", 12.5: "12.5"} {
		if got := FormatCost(in); got != want {
			t.Errorf("FormatCost(%v) = %q, want %q", in, got, want)
		}
	}
}

type fakeFeatures map[string]bool

func (f fakeFeatures) Enabled(_ context.Context, tenant, feature string) bool {
	return feature == Feature && f[tenant]
}

type memWriter struct{ n int }

func (m *memWriter) Write(_ context.Context, steps []model.Step) error { m.n += len(steps); return nil }

// ---- integration (BINDLE_POSTGRES_TEST_URL) -------------------------------

type testDB struct {
	owner, api, ingest, alerts *sql.DB
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func withDB(t *testing.T, admin, db, user, pass string) string {
	t.Helper()
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	return u.String()
}

func open(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestDB(t *testing.T) testDB {
	t.Helper()
	admin := os.Getenv("BINDLE_POSTGRES_TEST_URL")
	if admin == "" {
		t.Skip("BINDLE_POSTGRES_TEST_URL not set")
	}
	ctx := context.Background()
	sfx := randHex(4)
	name := "ledger_itest_" + sfx
	roles := control.Roles{API: "lt_api_" + sfx, Ingest: "lt_ingest_" + sfx, Processor: "lt_proc_" + sfx, Alerts: "lt_alerts_" + sfx,
		APIPassword: randHex(12), IngestPassword: randHex(12), ProcessorPassword: randHex(12), AlertsPassword: randHex(12)}
	root := open(t, admin)
	if _, err := root.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = root.ExecContext(ctx, "DROP DATABASE IF EXISTS "+pq.QuoteIdentifier(name)+" WITH (FORCE)")
		for _, r := range []string{roles.API, roles.Ingest, roles.Processor, roles.Alerts} {
			_, _ = root.ExecContext(ctx, "DROP ROLE IF EXISTS "+pq.QuoteIdentifier(r))
		}
	})
	owner := open(t, withDB(t, admin, name, "", ""))
	if _, err := control.Migrate(ctx, owner, roles); err != nil {
		t.Fatal(err)
	}
	return testDB{
		owner:  owner,
		api:    open(t, withDB(t, admin, name, roles.API, roles.APIPassword)),
		ingest: open(t, withDB(t, admin, name, roles.Ingest, roles.IngestPassword)),
		alerts: open(t, withDB(t, admin, name, roles.Alerts, roles.AlertsPassword)),
	}
}

func step(tenant, agent string, i int) model.Step {
	return model.Step{TenantID: tenant, AgentName: agent, TraceID: fmt.Sprintf("%032x", i/3+1), StepID: fmt.Sprintf("%016x", i+1),
		Name: fmt.Sprintf("step-%d", i), Type: model.StepTool, ToolName: "lookup", Status: model.StatusSuccess,
		StartUnixNano: uint64(time.Now().Add(-time.Hour).UnixNano()) + uint64(i)*1000, CostUSD: 0.001, CostKnown: true}
}

func entries(steps ...model.Step) []Entry {
	out := make([]Entry, len(steps))
	for i, s := range steps {
		out[i] = FromStep(s)
	}
	return out
}

func mustVerify(t *testing.T, r *Reader, tenant, agent string) Result {
	t.Helper()
	now := time.Now()
	res, err := r.Verify(context.Background(), tenant, agent, now.AddDate(0, 0, -30), now)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestIntegrationLedgerChain(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	w, r := &Writer{DB: db.ingest}, &Reader{DB: db.api}

	batch := entries(step("t1", "refund-agent", 0), step("t1", "refund-agent", 1), step("t1", "refund-agent", 2))
	if n, err := w.Append(ctx, "t1", "refund-agent", batch); err != nil || n != 3 {
		t.Fatalf("append: %d %v", n, err)
	}
	// A retried batch (and a batch with an internal duplicate) adds nothing.
	if n, err := w.Append(ctx, "t1", "refund-agent", append(batch, batch[0])); err != nil || n != 0 {
		t.Fatalf("retry appended %d %v", n, err)
	}
	if n, err := w.Append(ctx, "t1", "refund-agent", entries(step("t1", "refund-agent", 3), step("t1", "refund-agent", 3))); err != nil || n != 1 {
		t.Fatalf("append 4th: %d %v", n, err)
	}
	if res := mustVerify(t, r, "t1", "refund-agent"); !res.OK || res.Entries != 4 || res.FirstSeq != 1 || res.LastSeq != 4 {
		t.Fatalf("verify %+v", res)
	}
	list, err := r.List(ctx, "t1", "refund-agent", time.Now().AddDate(0, 0, -1), time.Now(), 0, 2)
	if err != nil || len(list) != 2 || list[0].Seq != 4 || list[1].Seq != 3 {
		t.Fatalf("list %+v %v", list, err)
	}
	if more, _ := r.List(ctx, "t1", "refund-agent", time.Now().AddDate(0, 0, -1), time.Now(), list[1].Seq, 10); len(more) != 2 || more[0].Seq != 2 {
		t.Fatalf("next page %+v", more)
	}

	// Tenant isolation: another tenant sees nothing, and its own chain is separate.
	if other, _ := r.List(ctx, "t2", "refund-agent", time.Now().AddDate(0, 0, -1), time.Now(), 0, 10); len(other) != 0 {
		t.Fatalf("t2 sees t1's entries: %+v", other)
	}
	if n, err := w.Append(ctx, "t2", "refund-agent", entries(step("t2", "refund-agent", 0))); err != nil || n != 1 {
		t.Fatalf("t2 append %d %v", n, err)
	}
	if res := mustVerify(t, r, "t2", "refund-agent"); !res.OK || res.Entries != 1 || res.FirstSeq != 1 {
		t.Fatalf("t2 verify %+v", res)
	}
	var visible int
	if err := r.inTenant(ctx, "t2", func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM control.ledger_entries`).Scan(&visible)
	}); err != nil || visible != 1 {
		t.Fatalf("t2 visible rows %d %v", visible, err)
	}
}

func TestIntegrationLedgerAppendOnly(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	w := &Writer{DB: db.ingest}
	if _, err := w.Append(ctx, "t1", "a", entries(step("t1", "a", 0))); err != nil {
		t.Fatal(err)
	}
	// App roles have no write privileges at all.
	for name, conn := range map[string]*sql.DB{"api": db.api, "ingest": db.ingest, "alerts": db.alerts} {
		for _, q := range []string{`UPDATE control.ledger_entries SET step_name = 'x'`, `DELETE FROM control.ledger_entries`,
			`INSERT INTO control.ledger_heads VALUES ('t1', 'b', 0, '\x00')`, `UPDATE control.ledger_heads SET seq = 0`} {
			if _, err := conn.ExecContext(ctx, q); err == nil {
				t.Errorf("%s role ran %q", name, q)
			}
		}
	}
	// Not even the owner can change or remove single entries or segments.
	for _, q := range []string{`UPDATE control.ledger_entries SET step_name = 'x'`, `DELETE FROM control.ledger_entries`,
		`TRUNCATE control.ledger_entries`, `TRUNCATE control.ledger_segments`} {
		if _, err := db.owner.ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("owner ran %q: %v", q, err)
		}
	}
	part := "control.ledger_entries_" + time.Now().UTC().Format("20060102")
	if _, err := db.owner.ExecContext(ctx, `TRUNCATE `+part); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("owner truncated the partition: %v", err)
	}
	if _, err := db.owner.ExecContext(ctx, `DELETE FROM `+part); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("owner deleted from the partition: %v", err)
	}
	// Writers can't fork the chain or backdate entries.
	tx, _ := db.ingest.BeginTx(ctx, nil)
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT control.ledger_append('t1', 'a', 0, '[]'::jsonb)`); err == nil {
		t.Error("append with a stale head was accepted")
	}
}

func TestIntegrationLedgerTamperDetection(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	w, r := &Writer{DB: db.ingest}, &Reader{DB: db.api}
	var steps []model.Step
	for i := 0; i < 6; i++ {
		steps = append(steps, step("t1", "a", i))
	}
	if _, err := w.Append(ctx, "t1", "a", entries(steps...)); err != nil {
		t.Fatal(err)
	}
	// A superuser who disables triggers can still edit rows, but verify notices.
	tamper := func(q string) {
		t.Helper()
		conn, err := db.owner.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, `SET session_replication_role = replica`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
		_, _ = conn.ExecContext(ctx, `SET session_replication_role = DEFAULT`)
	}
	tamper(`UPDATE control.ledger_entries SET cost_usd = 0 WHERE tenant_id = 't1' AND seq = 4`)
	res := mustVerify(t, r, "t1", "a")
	if res.OK || res.FirstBroken == nil || res.FirstBroken.Seq != 4 || !strings.Contains(res.FirstBroken.Reason, "hash") || res.Entries != 3 {
		t.Fatalf("edit not detected: %+v %+v", res, res.FirstBroken)
	}

	// Deleting a middle entry (with or without fixing up hashes) breaks the chain.
	db2 := newTestDB(t)
	w2, r2 := &Writer{DB: db2.ingest}, &Reader{DB: db2.api}
	if _, err := w2.Append(ctx, "t1", "a", entries(steps...)); err != nil {
		t.Fatal(err)
	}
	conn, _ := db2.owner.Conn(ctx)
	_, _ = conn.ExecContext(ctx, `SET session_replication_role = replica`)
	if _, err := conn.ExecContext(ctx, `DELETE FROM control.ledger_entries WHERE seq = 3`); err != nil {
		t.Fatal(err)
	}
	if res := mustVerify(t, r2, "t1", "a"); res.OK || res.FirstBroken.Seq != 3 || !strings.Contains(res.FirstBroken.Reason, "missing") {
		t.Fatalf("deleted entry not detected: %+v", res.FirstBroken)
	}
	// Dropping the newest entries is caught by the head.
	if _, err := conn.ExecContext(ctx, `DELETE FROM control.ledger_entries WHERE seq >= 3`); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if res := mustVerify(t, r2, "t1", "a"); res.OK || !strings.Contains(res.FirstBroken.Reason, "newer entries are missing") {
		t.Fatalf("truncated tail not detected: %+v", res.FirstBroken)
	}
}

func TestIntegrationLedgerConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	r := &Reader{DB: db.api}
	var all []model.Step
	for i := 0; i < 40; i++ {
		all = append(all, step("t1", "a", i))
	}
	// Eight writers append overlapping batches at once, as retried batches
	// from several processors would.
	want := map[int]bool{}
	for g := 0; g < 8; g++ {
		for k := 0; k < 5; k++ {
			for i := (g*5 + k) % 31; i < (g*5+k)%31+10; i++ {
				want[i] = true
			}
		}
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			w := &Writer{DB: db.ingest}
			for k := 0; k < 5; k++ {
				lo := (g*5 + k) % 31
				if _, err := w.Append(ctx, "t1", "a", entries(all[lo:lo+10]...)); err != nil {
					t.Error(err)
				}
			}
		}(g)
	}
	wg.Wait()
	res := mustVerify(t, r, "t1", "a")
	if !res.OK || res.Entries != int64(len(want)) || res.LastSeq != int64(len(want)) {
		t.Fatalf("concurrent appends: %+v %+v", res, res.FirstBroken)
	}
}

func TestIntegrationLedgerRetention(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	w, r := &Writer{DB: db.ingest}, &Reader{DB: db.api}

	// Two entries appended 10 days ago (written directly by the owner, as
	// the writer functions only append today).
	old := time.Now().UTC().AddDate(0, 0, -10)
	day := old.Format("2006-01-02")
	if _, err := db.owner.ExecContext(ctx, `SELECT control.ledger_ensure_partition($1)`, day); err != nil {
		t.Fatal(err)
	}
	prev := Genesis
	var last Entry
	for i := 0; i < 2; i++ {
		e := FromStep(step("t1", "a", i))
		e.TenantID, e.Agent, e.Seq, e.Day, e.PrevHash, e.At = "t1", "a", int64(i+1), day, prev, old.Truncate(time.Microsecond)
		e.SecurityFlags = []string{}
		e.Hash = Hash(e)
		if _, err := db.owner.ExecContext(ctx, `INSERT INTO control.ledger_entries (tenant_id, agent, seq, day, at, kind, trace_id,
			step_id, entry_key, step_type, step_name, tool_name, status, error_class, input_tokens, output_tokens, cost_usd,
			cost_known, security_flags, detail, prev_hash, hash)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17::numeric,$18,$19,$20,$21,$22)`,
			e.TenantID, e.Agent, e.Seq, e.Day, e.At, e.Kind, e.TraceID, e.StepID, e.EntryKey, e.StepType, e.StepName, e.ToolName,
			e.Status, e.ErrorClass, e.InputTokens, e.OutputTokens, e.CostUSD, e.CostKnown, pq.Array(e.SecurityFlags), e.Detail,
			e.PrevHash, e.Hash); err != nil {
			t.Fatal(err)
		}
		prev, last = e.Hash, e
	}
	if _, err := db.owner.ExecContext(ctx, `INSERT INTO control.ledger_heads VALUES ('t1', 'a', 2, $1)`, last.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(ctx, "t1", "a", entries(step("t1", "a", 5))); err != nil {
		t.Fatal(err)
	}
	if res := mustVerify(t, r, "t1", "a"); !res.OK || res.Entries != 3 {
		t.Fatalf("before retention: %+v %+v", res, res.FirstBroken)
	}

	// Retention of 5 days: the old day's segment is recorded, then its
	// partition dropped. Verification still works from the segment's hash.
	closed, dropped, err := Maintain(ctx, db.alerts, 5)
	if err != nil || closed < 1 || dropped < 1 {
		t.Fatalf("maintain: closed %d dropped %d %v", closed, dropped, err)
	}
	var n int
	if err := db.owner.QueryRowContext(ctx, `SELECT count(*) FROM control.ledger_entries WHERE seq <= 2`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old entries kept: %d %v", n, err)
	}
	var seg struct {
		first, last, count int64
		hash               []byte
	}
	if err := db.owner.QueryRowContext(ctx, `SELECT first_seq, last_seq, entries, final_hash FROM control.ledger_segments WHERE day = $1`, day).
		Scan(&seg.first, &seg.last, &seg.count, &seg.hash); err != nil || seg.first != 1 || seg.last != 2 || seg.count != 2 || hex.EncodeToString(seg.hash) != hex.EncodeToString(last.Hash) {
		t.Fatalf("segment %+v %v", seg, err)
	}
	if _, err := db.owner.ExecContext(ctx, `DELETE FROM control.ledger_segments`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("owner deleted a segment: %v", err)
	}
	res := mustVerify(t, r, "t1", "a")
	if !res.OK || res.Entries != 1 || res.FirstSeq != 3 {
		t.Fatalf("after retention: %+v %+v", res, res.FirstBroken)
	}
	// Running maintenance again changes nothing; upcoming partitions exist.
	if c, d, err := Maintain(ctx, db.alerts, 5); err != nil || c != 0 || d != 0 {
		t.Fatalf("maintain again: %d %d %v", c, d, err)
	}
	var future bool
	if err := db.owner.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, "control.ledger_entries_"+time.Now().UTC().AddDate(0, 0, 3).Format("20060102")).Scan(&future); err != nil || !future {
		t.Fatalf("no partition 3 days ahead: %v", err)
	}
	if _, _, err := Maintain(ctx, db.api, 5); err == nil {
		t.Fatal("the api role ran maintenance")
	}
}

func TestIntegrationLedgerSinkGating(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	inner := &memWriter{}
	s := &Sink{Inner: inner, Ledger: &Writer{DB: db.ingest}, Features: fakeFeatures{"on": true}}
	steps := []model.Step{step("on", "a", 0), step("off", "a", 1), step("on", "b", 2), step("on", "a", 3)}
	if err := s.Write(ctx, steps); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, steps); err != nil { // retried batch
		t.Fatal(err)
	}
	if inner.n != 8 {
		t.Fatalf("inner got %d steps", inner.n)
	}
	r := &Reader{DB: db.api}
	if res := mustVerify(t, r, "on", "a"); !res.OK || res.Entries != 2 {
		t.Fatalf("on/a %+v", res)
	}
	if res := mustVerify(t, r, "on", "b"); !res.OK || res.Entries != 1 {
		t.Fatalf("on/b %+v", res)
	}
	if res := mustVerify(t, r, "off", "a"); res.Entries != 0 {
		t.Fatalf("ledger written for a tenant without the feature: %+v", res)
	}
}
