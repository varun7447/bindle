package pg

import (
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/services/internal/model"
	"github.com/bindle-ai/bindle/services/internal/query"
)

func TestRunsSQLUsesPlaceholdersOnly(t *testing.T) {
	evil := "x' OR 1=1 --"
	f := query.RunFilter{
		From: time.Unix(100, 0), To: time.Unix(200, 0), Limit: 51,
		Agent: evil, Environment: evil, Status: "failed",
		AfterStart: time.Unix(150, 5), AfterTrace: evil,
	}
	sqlText, a := runsSQL("tenant-1", f)
	if strings.Contains(sqlText, evil) || strings.Contains(sqlText, "tenant-1") {
		t.Fatalf("value spliced into SQL:\n%s", sqlText)
	}
	for _, frag := range []string{"tenant_id = $1", "agent_name = $4", "environment = $5", "failed_steps > 0", "(first_start_ns, trace_id) < ($6, $7)", "LIMIT $8"} {
		if !strings.Contains(sqlText, frag) {
			t.Errorf("missing %q in\n%s", frag, sqlText)
		}
	}
	want := []any{"tenant-1", int64(100e9), int64(200e9), evil, evil, int64(150e9 + 5), evil, 51}
	if len(a) != len(want) {
		t.Fatalf("args = %v", a)
	}
	for i := range want {
		if a[i] != want[i] {
			t.Errorf("arg %d = %v, want %v", i+1, a[i], want[i])
		}
	}

	sqlText, a = runsSQL("t", query.RunFilter{From: time.Unix(1, 0), To: time.Unix(2, 0), Limit: 5, Status: "ok"})
	if strings.Contains(sqlText, "agent_name =") || strings.Contains(sqlText, "trace_id) <") || len(a) != 4 ||
		!strings.Contains(sqlText, "failed_steps = 0 AND root_status <> 'failure'") {
		t.Errorf("unused filters leaked:\n%s %v", sqlText, a)
	}
}

func TestWindowDropsStepsWithoutPartition(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	w := &Writer{Retention: 30 * 24 * time.Hour}
	at := func(d time.Duration) model.Step { return model.Step{StartUnixNano: uint64(now.Add(d).UnixNano())} }
	kept, days, dropped := w.window([]model.Step{
		at(-31 * 24 * time.Hour), // too old
		at(-29 * 24 * time.Hour),
		at(-time.Hour),
		at(0),
		at(13 * time.Hour), // tomorrow
		at(25 * time.Hour), // too far ahead
	}, now)
	if len(kept) != 4 || dropped != 2 {
		t.Fatalf("kept=%d dropped=%d", len(kept), dropped)
	}
	if strings.Join(days, ",") != "2026-10-18,2026-11-16,2026-11-17" {
		t.Errorf("days = %v", days)
	}
}

func TestCleanMakesTextStorable(t *testing.T) {
	if got := clean("a\x00b"); got != "a�b" {
		t.Errorf("NUL: %q", got)
	}
	if got := clean("ok\xffx"); got != "ok�x" {
		t.Errorf("invalid UTF-8: %q", got)
	}
	if got := clean("plain ✓"); got != "plain ✓" {
		t.Errorf("valid text changed: %q", got)
	}
}

func TestColumnsShapesArraysAndJSON(t *testing.T) {
	cols, err := columns([]model.Step{
		{TenantID: "t", Attributes: map[string]string{"k\x00": "v"}, Links: []string{"a:b"}},
		{TenantID: "t"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 30 {
		t.Fatalf("got %d columns, insertSQL expects 30", len(cols))
	}
	if n := strings.Count(insertSQL, "::text[]") + strings.Count(insertSQL, "::bigint[]") +
		strings.Count(insertSQL, "::float8[]") + strings.Count(insertSQL, "::bool[]"); n != 30 {
		t.Fatalf("insertSQL has %d array params", n)
	}
}

func TestRenderMigrationQuotesRoles(t *testing.T) {
	out := renderMigration("GRANT x TO {{writer}}, {{reader}};", Roles{Writer: "w_role", Reader: "r_role"})
	if out != `GRANT x TO "w_role", "r_role";` {
		t.Errorf("rendered %q", out)
	}
	files, err := migrationFiles()
	if err != nil || len(files) == 0 || files[0] != "001_init.sql" {
		t.Fatalf("migrations = %v %v", files, err)
	}
}
