package pg

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/bindle-ai/bindle/services/internal/model"
	"github.com/bindle-ai/bindle/services/internal/query"
)

// Runs only when BINDLE_POSTGRES_TEST_URL is set to a superuser/owner URL,
// e.g. postgres://postgres:pw@127.0.0.1:55432/postgres?sslmode=disable.
// It creates a throwaway database and uniquely named roles, and drops both
// at the end.
func TestIntegrationPostgres(t *testing.T) {
	admin := os.Getenv("BINDLE_POSTGRES_TEST_URL")
	if admin == "" {
		t.Skip("BINDLE_POSTGRES_TEST_URL not set")
	}
	ctx := context.Background()
	suffix := randHex(4)
	dbName := "bindle_itest_" + suffix
	roles := Roles{
		Writer: "itest_writer_" + suffix, Reader: "itest_reader_" + suffix,
		WriterPassword: randHex(12), ReaderPassword: randHex(12),
	}

	root := open(t, admin)
	if _, err := root.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(dbName)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = root.ExecContext(ctx, "DROP DATABASE IF EXISTS "+pq.QuoteIdentifier(dbName)+" WITH (FORCE)")
		for _, r := range []string{roles.Writer, roles.Reader} {
			_, _ = root.ExecContext(ctx, "DROP ROLE IF EXISTS "+pq.QuoteIdentifier(r))
		}
		root.Close()
	})

	owner := open(t, withDB(t, admin, dbName, "", ""))
	applied, err := Migrate(ctx, owner, roles)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 {
		t.Fatalf("applied = %v", applied)
	}
	if again, err := Migrate(ctx, owner, roles); err != nil || len(again) != 0 {
		t.Fatalf("re-running migrations should be a no-op: %v %v", again, err)
	}

	writerDB := open(t, withDB(t, admin, dbName, roles.Writer, roles.WriterPassword))
	readerDB := open(t, withDB(t, admin, dbName, roles.Reader, roles.ReaderPassword))
	now := time.Now().UTC()
	w := &Writer{DB: writerDB}
	store := &Store{DB: readerDB}

	traceA, traceB := randHex(16), randHex(16)
	base := uint64(now.Add(-time.Minute).UnixNano()) + 123 // non-µs nanos survive
	root1 := randHex(8)
	mk := func(tenant, trace, id, parent, typ, status, tool, input string, off uint64) model.Step {
		return model.Step{
			TenantID: tenant, TraceID: trace, StepID: id, ParentStepID: parent, Name: id, Type: typ,
			Status: status, ToolName: tool, Input: input, AgentName: "agent-a", Environment: "test",
			Framework: "test", StartUnixNano: base + off, DurationMs: 5,
			Attributes: map[string]string{"k": "v\x00"}, Links: []string{"t:s"},
		}
	}
	batch1 := []model.Step{
		mk("tenant-a", traceA, root1, "", model.StepAgent, model.StatusSuccess, "", "", 0),
		mk("tenant-a", traceA, randHex(8), root1, model.StepLLM, model.StatusSuccess, "", "hi [REDACTED:EMAIL] [REDACTED:SECRET]", 1000),
		mk("tenant-b", traceB, randHex(8), "", model.StepAgent, model.StatusFailure, "", "", 0),
	}
	batch1[1].Model = "unpriced-model"
	var batch2 []model.Step
	for i := 0; i < 5; i++ {
		batch2 = append(batch2, mk("tenant-a", traceA, randHex(8), root1, model.StepTool, model.StatusFailure, "search", `{"q":"same"}`, uint64(2000+i)))
	}
	for _, b := range [][]model.Step{batch1, batch2, batch1} { // batch1 twice: retries must not double-count
		if err := w.Write(ctx, b); err != nil {
			t.Fatal(err)
		}
	}

	from, to := now.Add(-time.Hour), now.Add(time.Minute)
	runs, err := store.ListRuns(ctx, "tenant-a", query.RunFilter{From: from, To: to, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].TraceID != traceA {
		t.Fatalf("tenant-a runs = %+v", runs)
	}
	r := runs[0]
	if r.Steps != 7 || r.FailedSteps != 5 || r.RootStatus != "success" || r.CostUnknownSteps != 1 ||
		r.Agent != "agent-a" || r.Environment != "test" || r.Started.UnixNano() != int64(base) {
		t.Errorf("rollup = %+v (want 7 steps, 5 failed, started %d)", r, base)
	}
	for _, f := range []query.RunFilter{
		{From: from, To: to, Limit: 10, Status: "ok"},
		{From: from, To: to, Limit: 10, Agent: "other"},
		{From: from, To: to, Limit: 10, AfterStart: r.Started.Time, AfterTrace: r.TraceID},
	} {
		if got, err := store.ListRuns(ctx, "tenant-a", f); err != nil || len(got) != 0 {
			t.Errorf("filter %+v: %v %v", f, got, err)
		}
	}

	if tr, err := store.GetTrace(ctx, "tenant-b", traceA, 100); err != nil || tr != nil {
		t.Fatalf("tenant-b saw tenant-a trace: %+v %v", tr, err)
	}
	tr, err := store.GetTrace(ctx, "tenant-a", traceA, 3)
	if err != nil || tr == nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if len(tr.Steps) != 3 || !tr.Truncated || !tr.Steps[1].Redacted || tr.Steps[1].InputBytes == 0 {
		t.Errorf("trace = %+v", tr)
	}
	sd, err := store.GetStep(ctx, "tenant-a", traceA, tr.Steps[1].StepID)
	if err != nil || sd == nil {
		t.Fatalf("GetStep: %v", err)
	}
	if strings.Join(sd.RedactedKinds, ",") != "EMAIL,SECRET" || sd.Attributes["k"] != "v�" || len(sd.Links) != 1 {
		t.Errorf("step = %+v", sd)
	}
	if sd, err := store.GetStep(ctx, "tenant-b", traceA, tr.Steps[1].StepID); err != nil || sd != nil {
		t.Fatalf("tenant-b saw tenant-a step: %+v %v", sd, err)
	}

	ov, err := store.Overview(ctx, "tenant-a", from, to, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Runs != 1 || ov.FailedRuns != 1 || ov.Steps != 7 || len(ov.Series) == 0 ||
		len(ov.TopFailingTools) != 1 || ov.TopFailingTools[0].Failures != 5 {
		t.Errorf("overview = %+v", ov)
	}
	loops, err := store.Loops(ctx, "tenant-a", from, to, 5)
	if err != nil || len(loops) != 1 || loops[0].Repeats != 5 || loops[0].ToolName != "search" {
		t.Errorf("loops = %+v %v", loops, err)
	}
	if loops, _ := store.Loops(ctx, "tenant-b", from, to, 1); len(loops) != 0 {
		t.Errorf("tenant-b loops = %+v", loops)
	}

	// Row-level security, independent of the queries' own tenant filter.
	var n int
	if err := readerDB.QueryRowContext(ctx, "SELECT count(*) FROM bindle.steps").Scan(&n); err != nil || n != 0 {
		t.Errorf("reader with no tenant set saw %d rows (%v)", n, err)
	}
	inTenant := func(db *sql.DB, tenant, stmt string) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, "SELECT set_config('bindle.tenant', $1, true)", tenant); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err := inTenant(readerDB, "tenant-a", "SELECT count(*) FROM bindle.steps WHERE tenant_id = 'tenant-b'"); err != nil {
		t.Fatal(err)
	}
	var crossCount int
	tx, _ := readerDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	_, _ = tx.ExecContext(ctx, "SELECT set_config('bindle.tenant', 'tenant-a', true)")
	_ = tx.QueryRowContext(ctx, "SELECT count(*) FROM bindle.steps WHERE tenant_id = 'tenant-b'").Scan(&crossCount)
	_ = tx.Rollback()
	if crossCount != 0 {
		t.Errorf("RLS let tenant-a read %d tenant-b rows", crossCount)
	}
	if err := inTenant(writerDB, "tenant-a", "INSERT INTO bindle.traces (tenant_id, trace_id, first_start, first_start_ns) VALUES ('tenant-b', 'x', now(), 0)"); err == nil {
		t.Error("writer inserted a row for another tenant")
	}
	for _, stmt := range []string{
		"INSERT INTO bindle.traces (tenant_id, trace_id, first_start, first_start_ns) VALUES ('tenant-a', 'x', now(), 0)",
		"DELETE FROM bindle.traces",
	} {
		if err := inTenant(readerDB, "tenant-a", stmt); err == nil {
			t.Errorf("reader could run %q", stmt)
		}
	}
	for _, stmt := range []string{"DELETE FROM bindle.steps", "UPDATE bindle.steps SET name = 'x'", "DROP TABLE bindle.traces", "CREATE TABLE bindle.evil (x int)"} {
		if err := inTenant(writerDB, "tenant-a", stmt); err == nil {
			t.Errorf("writer could run %q", stmt)
		}
	}

	// Partition and retention guards.
	if _, err := writerDB.ExecContext(ctx, "SELECT bindle.ensure_steps_partition((current_date + 30)::date)"); err == nil {
		t.Error("writer created a partition far in the future")
	}
	if _, err := writerDB.ExecContext(ctx, "SELECT bindle.apply_retention(current_date)"); err == nil {
		t.Error("retention accepted a cutoff of today")
	}
	if _, err := owner.ExecContext(ctx, "SELECT bindle.ensure_steps_partition((current_date - 40)::date)"); err != nil {
		t.Fatal(err)
	}
	var dropped int
	if err := writerDB.QueryRowContext(ctx, "SELECT bindle.apply_retention((current_date - 30)::date)").Scan(&dropped); err != nil || dropped != 1 {
		t.Errorf("retention dropped %d partitions (%v), want 1", dropped, err)
	}

	// Steps outside the time window are dropped, not failed.
	old := mk("tenant-a", randHex(16), randHex(8), "", model.StepAgent, model.StatusSuccess, "", "", 0)
	old.StartUnixNano = uint64(now.Add(-60 * 24 * time.Hour).UnixNano())
	if err := w.Write(ctx, []model.Step{old}); err != nil {
		t.Errorf("out-of-window step should be dropped without error: %v", err)
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
