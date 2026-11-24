package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/lessons"
	"github.com/bindle-ai/bindle/backend/internal/skills"
)

func TestIntegrationLearningLoop(t *testing.T) {
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
	ta, ua := workspace("sub-learn-a")
	tb, _ := workspace("sub-learn-b")
	if _, err := owner.ExecContext(ctx, `INSERT INTO control.tenant_features (tenant_id, feature, enabled_by) VALUES ($1, 'learning_loop', 'test')`, ta); err != nil {
		t.Fatal(err)
	}

	// Lessons: the alerts role replaces them; ingest and the API read them.
	lw := &LessonWriter{DB: open(t, lastAlertsDSN)}
	ts, err := lw.Tenants(ctx)
	if err != nil || len(ts) != 1 || ts[0].TenantID != ta || ts[0].MinRuns != 5 || !ts[0].Capture {
		t.Fatalf("tenants %+v %v", ts, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	l := lessons.Lesson{ID: lessons.ID("refund", "lookup_order", "lookup_order", "order <N> not found"), Agent: "refund",
		StepName: "lookup_order", StepType: "tool_call", Tool: "lookup_order", Pattern: "order <N> not found", Cause: lessons.CauseNotFound,
		Title: "lookup_order failed in 6 runs", Runs: 6, Failures: 7, FirstSeen: now.Add(-time.Hour), LastSeen: now,
		Evidence: []string{"0123456789abcdef0123456789abcdef"}}
	l.Pack = lessons.BuildPack(l, nil, true)
	if n, err := lw.Replace(ctx, ta, []lessons.Lesson{l}, map[string][]string{"refund": {"lookup_order", "send_email"}}); err != nil || n != 1 {
		t.Fatalf("replace %d %v", n, err)
	}
	li := &LearningIngest{DB: ingestDB}
	if got, err := li.Lessons(ctx, ta, "refund"); err != nil || len(got) != 1 || got[0].Runs != 6 {
		t.Fatalf("ingest lessons %+v %v", got, err)
	}
	if pk, ok, err := li.Pack(ctx, ta, l.ID); err != nil || !ok || !strings.Contains(string(pk), lessons.PackFormat) {
		t.Fatalf("pack %s %v %v", pk, ok, err)
	}
	if _, ok, _ := li.Pack(ctx, tb, l.ID); ok {
		t.Fatal("tenant B read tenant A's pack")
	}
	if tools, err := li.AgentTools(ctx, ta, "refund"); err != nil || strings.Join(tools, ",") != "lookup_order,send_email" {
		t.Fatalf("tools %v %v", tools, err)
	}
	if got, err := store.ListLessons(ctx, tb, ""); err != nil || len(got) != 0 {
		t.Fatalf("tenant B sees lessons: %v %v", got, err)
	}
	if n, err := lw.Replace(ctx, ta, nil, nil); err != nil || n != 0 {
		t.Fatalf("replace empty %d %v", n, err)
	}
	if got, _ := store.ListLessons(ctx, ta, ""); len(got) != 0 {
		t.Fatal("a lesson no longer seen was kept")
	}

	// Vault: create, conflict, unchanged, edit resets status; transitions enforced.
	put := func(db queryRower, tenant, path, kind, body, base string) VaultPutResult {
		t.Helper()
		r, err := vaultPut(ctx, db, VaultPutInput{Tenant: tenant, Agent: "refund", Path: path, Kind: kind, Body: body,
			SHA256: sha(body), BaseSHA256: base, Scan: map[string]any{}, Check: map[string]any{}, Links: skills.Links(body), By: "key:mkey_1"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	p := "skills/retry/SKILL.md"
	if r := put(ingestDB, ta, p, "skill", "v1", ""); r.Version != 1 || r.Conflict {
		t.Fatalf("create %+v", r)
	}
	if r := put(ingestDB, ta, p, "skill", "v2", ""); !r.Conflict || r.CurrentSHA != sha("v1") {
		t.Fatalf("stale base %+v", r)
	}
	if r := put(ingestDB, ta, p, "skill", "v1", sha("v1")); !r.Unchanged {
		t.Fatalf("same text %+v", r)
	}
	if _, ok, err := store.VaultSetStatus(ctx, ta, "refund", p, "draft", "approved", sha("v1"), "approve", "user:x"); err == nil && ok {
		t.Fatal("draft -> approved allowed")
	}
	// Proven needs a passing proof report for this text (phase 2; 017).
	if _, ok, err := store.VaultSetStatus(ctx, ta, "refund", p, "draft", "proven", sha("v1"), "prove", "proof"); err == nil && ok {
		t.Fatal("proven without a proof report")
	}
	if _, err := owner.ExecContext(ctx, `INSERT INTO control.skill_proofs (tenant_id, agent, path, sha256, report, verdict, passed, uploaded_by)
		VALUES ($1, 'refund', $2, $3, '{}', '{}', true, 'test')`, ta, p, sha("v1")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.VaultSetStatus(ctx, ta, "refund", p, "draft", "proven", sha("v1"), "prove", "proof"); err != nil || !ok {
		t.Fatalf("prove %v %v", ok, err)
	}
	if _, ok, _ := store.VaultSetStatus(ctx, ta, "refund", p, "proven", "approved", sha("stale"), "approve", "user:x"); ok {
		t.Fatal("approved a text the caller didn't see")
	}
	if ok, err := store.VaultReview(ctx, ta, "refund", p, sha("v1"), "user:r"); err != nil || !ok {
		t.Fatalf("review %v %v", ok, err)
	}
	if v, ok, err := store.VaultSetStatus(ctx, ta, "refund", p, "proven", "approved", sha("v1"), "approve", "user:"+ua); err != nil || !ok || v != 3 {
		t.Fatalf("approve %d %v %v", v, ok, err)
	}
	if r := put(apiDB, ta, p, "skill", "v2 links [[notes/a]]", sha("v1")); r.Version != 4 {
		t.Fatalf("edit %+v", r)
	}
	n, err := store.GetVaultNote(ctx, ta, "refund", p)
	if err != nil || n.Status != "draft" || n.ReviewedSHA != "" || len(n.Versions) != 4 || strings.Join(n.Links, ",") != "notes/a.md" {
		t.Fatalf("note after edit %+v %v", n, err)
	}
	put(apiDB, ta, "notes/a.md", "note", "see [[skills/retry/SKILL.md]]", "")
	if n, _ := store.GetVaultNote(ctx, ta, "refund", "notes/a.md"); n.Status != "" || strings.Join(n.Backlinks, ",") != p {
		t.Fatalf("note %+v", n)
	}

	// History is append-only, even for the owner; other tenants see nothing.
	if _, err := owner.ExecContext(ctx, `UPDATE control.vault_versions SET body = 'x'`); err == nil {
		t.Fatal("history updated")
	}
	if _, err := owner.ExecContext(ctx, `DELETE FROM control.vault_versions`); err == nil {
		t.Fatal("history deleted")
	}
	if notes, links, err := store.VaultNotes(ctx, tb, "refund"); err != nil || len(notes) != 0 || len(links) != 0 {
		t.Fatalf("tenant B sees notes: %v %v %v", notes, links, err)
	}
	if files, err := li.VaultList(ctx, tb, "refund"); err != nil || len(files) != 0 {
		t.Fatalf("tenant B vault list %v %v", files, err)
	}
	if _, err := apiDB.ExecContext(ctx, `INSERT INTO control.vault_notes (tenant_id, agent, path, kind, version, sha256, body, updated_by)
		VALUES ($1, 'refund', 'notes/z.md', 'note', 1, $2, 'x', 'me')`, ta, sha("x")); err == nil {
		t.Fatal("the API role wrote vault_notes directly")
	}

	// Settings and security reviewers.
	if _, err := store.PutLearningSettings(ctx, ta, ua, LearningSettings{MinRuns: 1}); err == nil {
		t.Fatal("min_runs 1 accepted")
	}
	if s, err := store.PutLearningSettings(ctx, ta, ua, LearningSettings{MinRuns: 3}); err != nil || s.MinRuns != 3 {
		t.Fatalf("settings %+v %v", s, err)
	}
	if got, _ := store.GetLearningSettings(ctx, tb); got.MinRuns != lessons.DefaultMinRuns {
		t.Fatalf("tenant B settings %+v", got)
	}
	if _, err := store.AddSecurityReviewer(ctx, ta, ua, "sub-learn-b@example.com"); err != ErrNotFound {
		t.Fatalf("added a member of another workspace: %v", err)
	}
	rv, err := store.AddSecurityReviewer(ctx, ta, ua, "SUB-LEARN-A@example.com")
	if err != nil || rv.UserID != ua {
		t.Fatalf("add reviewer %+v %v", rv, err)
	}
	if ok, _ := store.IsSecurityReviewer(ctx, ta, ua); !ok {
		t.Fatal("not a reviewer")
	}
	if ok, _ := store.IsSecurityReviewer(ctx, tb, ua); ok {
		t.Fatal("reviewer in another workspace")
	}
	if email, _ := store.UserEmail(ctx, tb, ua); email != "" {
		t.Fatal("read another workspace's user email")
	}
	if err := store.RemoveSecurityReviewer(ctx, ta, ua); err != nil {
		t.Fatal(err)
	}

	// The API role may append to ledgers (console vault changes), through the functions only.
	tx, err := apiDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT seq FROM control.ledger_lock_head($1, 'refund')`, ta).Scan(&seq); err != nil {
		t.Fatalf("api role can't lock a ledger head: %v", err)
	}
	_ = tx.Rollback()
	if _, err := apiDB.ExecContext(ctx, `INSERT INTO control.ledger_entries (tenant_id, agent, seq, day, at, kind, prev_hash, hash)
		VALUES ($1, 'refund', 1, current_date, now(), 'vault_change', '\\x00', '\\x00')`, ta); err == nil {
		t.Fatal("the API role wrote ledger_entries directly")
	}
}

func sha(s string) string { return skills.Digest(s) }
