package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/lessons"
	"github.com/bindle-ai/bindle/backend/internal/skills"
	"github.com/bindle-ai/bindle/backend/internal/skillship"
)

const shipSkillV1 = "---\nname: retry-refund\ndescription: Retry a refund once on a timeout. Use when refund_payment times out.\n---\n\nRetry once.\n"
const shipSkillV2 = "---\nname: retry-refund\ndescription: Retry a refund once on a timeout. Use when refund_payment times out.\n---\n\nWait a second, then retry once.\n"

func TestIntegrationSkillShipping(t *testing.T) {
	ctx := context.Background()
	owner, apiDB, ingestDB := newTestDB(t)
	store := &Store{DB: apiDB, Secrets: testSecrets}
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
	ta, ua := workspace("sub-ship-a")
	tb, _ := workspace("sub-ship-b")
	for _, tn := range []string{ta, tb} {
		if _, err := owner.ExecContext(ctx, `INSERT INTO control.tenant_features (tenant_id, feature, enabled_by) VALUES ($1, 'learning_loop', 'test')`, tn); err != nil {
			t.Fatal(err)
		}
	}
	alertsDB := open(t, lastAlertsDSN)
	li := &LearningIngest{DB: ingestDB}
	const agent, path = "refunds", "skills/retry-refund/SKILL.md"
	tr := "0123456789abcdef0123456789abcdef"

	// Cassettes: the alerts worker records a lesson's evidence; ingest serves
	// them; they go with the lesson.
	lw := &LessonWriter{DB: alertsDB}
	now := time.Now().UTC().Truncate(time.Second)
	l := lessons.Lesson{ID: lessons.ID(agent, "refund_payment", "refund_payment", "timed out"), Agent: agent, StepName: "refund_payment",
		StepType: "tool_call", Tool: "refund_payment", Pattern: "timed out", Cause: lessons.CauseTimeout, Title: "refund_payment failed in 6 runs",
		Runs: 6, Failures: 6, FirstSeen: now.Add(-time.Hour), LastSeen: now, Evidence: []string{tr}}
	l.Pack = lessons.BuildPack(l, nil, true)
	if _, err := lw.Replace(ctx, ta, []lessons.Lesson{l}, nil); err != nil {
		t.Fatal(err)
	}
	cw := &CassetteWriter{DB: alertsDB}
	missing, err := cw.Missing(ctx, ta, skillship.MaxCassettes)
	if err != nil || len(missing) != 1 || missing[0].TraceID != tr {
		t.Fatalf("missing %+v %v", missing, err)
	}
	if err := cw.Put(ctx, ta, l.ID, tr, []byte(`{"format":"bindle.cassette","trace_id":"`+tr+`"}`)); err != nil {
		t.Fatal(err)
	}
	if m, _ := cw.Missing(ctx, ta, skillship.MaxCassettes); len(m) != 0 {
		t.Fatalf("still missing %+v", m)
	}
	if cs, err := li.Cassettes(ctx, ta, l.ID); err != nil || !strings.Contains(string(cs), tr) {
		t.Fatalf("cassettes %s %v", cs, err)
	}
	if cs, _ := li.Cassettes(ctx, tb, l.ID); string(cs) != "[]" {
		t.Fatalf("tenant B read tenant A's cassettes: %s", cs)
	}

	// A draft; a failing proof leaves it a draft, a passing one proves it.
	put := func(body, base string) {
		t.Helper()
		if _, err := vaultPut(ctx, ingestDB, VaultPutInput{Tenant: ta, Agent: agent, Path: path, Kind: skills.KindSkill, Body: body, SHA256: sha(body),
			BaseSHA256: base, Scan: map[string]any{}, Check: map[string]any{}, By: "key:mkey_1"}); err != nil {
			t.Fatal(err)
		}
	}
	put(shipSkillV1, "")
	rep := skillship.Report{Agent: agent, Path: path, SHA256: sha(shipSkillV1), Runs: []skillship.RunResult{{TraceID: tr, Covered: true}}}
	if thr, err := li.ProofThreshold(ctx, ta); err != nil || thr != 80 {
		t.Fatalf("threshold %d %v", thr, err)
	}
	res, err := li.AddProof(ctx, ta, rep, skillship.Judge(rep, 80), "key:mkey_1")
	if err != nil || res.Proven || res.Status != "draft" {
		t.Fatalf("failing proof %+v %v", res, err)
	}
	rep.Runs[0].Fixed = true
	if res, err = li.AddProof(ctx, ta, rep, skillship.Judge(rep, 80), "key:mkey_1"); err != nil || !res.Proven || res.Status != "proven" {
		t.Fatalf("passing proof %+v %v", res, err)
	}
	stale := rep
	stale.SHA256 = sha("other text")
	if _, err := li.AddProof(ctx, ta, stale, skillship.Judge(stale, 80), "key:mkey_1"); err != ErrChanged {
		t.Fatalf("proof for another text: %v", err)
	}
	if _, err := li.AddProof(ctx, tb, rep, skillship.Judge(rep, 80), "key:mkey_1"); err != ErrNotFound {
		t.Fatalf("tenant B proved tenant A's skill: %v", err)
	}
	if ps, err := store.SkillProofs(ctx, ta, agent, path); err != nil || len(ps) != 2 || !ps[0].Passed {
		t.Fatalf("proofs %+v %v", ps, err)
	}
	if _, err := owner.ExecContext(ctx, `DELETE FROM control.skill_proofs WHERE tenant_id = $1`, ta); err == nil {
		t.Fatal("proof reports could be deleted")
	}

	// Approving ships release 1; nothing reaches the agent until it opts in.
	if _, ok, err := store.VaultSetStatus(ctx, ta, agent, path, "proven", "approved", sha(shipSkillV1), "approve", "user:"+ua); err != nil || !ok {
		t.Fatalf("approve %v %v", ok, err)
	}
	if got, err := li.Skills(ctx, ta, agent); err != nil || len(got) != 0 {
		t.Fatalf("shipped before opt-in: %+v %v", got, err)
	}
	if err := store.PutAgentSkillSettings(ctx, ta, ua, AgentSkillSettings{Agent: agent, Ship: true}); err != nil {
		t.Fatal(err)
	}
	got, err := li.Skills(ctx, ta, agent)
	if err != nil || len(got) != 1 || got[0].Release != 1 || got[0].Body != shipSkillV1 {
		t.Fatalf("shipped %+v %v", got, err)
	}
	if got, _ := li.Skills(ctx, tb, agent); len(got) != 0 {
		t.Fatal("tenant B got tenant A's skills")
	}

	// A new text needs its own proof, and ships as release 2.
	put(shipSkillV2, sha(shipSkillV1))
	if _, ok, err := store.VaultSetStatus(ctx, ta, agent, path, "draft", "proven", sha(shipSkillV2), "prove", "x"); err == nil && ok {
		t.Fatal("proven without a proof for the new text")
	}
	rep2 := rep
	rep2.SHA256 = sha(shipSkillV2)
	if res, err := li.AddProof(ctx, ta, rep2, skillship.Judge(rep2, 80), "key:mkey_1"); err != nil || !res.Proven {
		t.Fatalf("prove v2 %+v %v", res, err)
	}
	if got, _ := li.Skills(ctx, ta, agent); len(got) != 1 || got[0].Release != 1 {
		t.Fatalf("v1 should keep shipping until v2 is approved: %+v", got)
	}
	if _, ok, err := store.VaultSetStatus(ctx, ta, agent, path, "proven", "approved", sha(shipSkillV2), "approve", "user:"+ua); err != nil || !ok {
		t.Fatalf("approve v2 %v %v", ok, err)
	}
	if got, _ := li.Skills(ctx, ta, agent); len(got) != 1 || got[0].Release != 2 {
		t.Fatalf("release 2 not served: %+v", got)
	}

	// The measurement job sees release 2; agents loading it make it live;
	// a rollback serves release 1 again.
	ss := &ShippingStore{DB: alertsDB}
	rs, err := ss.Releases(ctx)
	if err != nil || len(rs) != 1 || rs[0].Tenant != ta || rs[0].Release.Release != 2 || rs[0].NoteStatus != "approved" {
		t.Fatalf("releases %+v %v", rs, err)
	}
	if lb, err := ss.Lessons(ctx, ta); err != nil || lb[l.ID].Tool != "refund_payment" {
		t.Fatalf("lessons %+v %v", lb, err)
	}
	if v, err := ss.GoLive(ctx, ta, agent, path, sha(shipSkillV2)); err != nil || v == 0 {
		t.Fatalf("go live %d %v", v, err)
	}
	m := skillship.Measurement{Agent: agent, Path: path, Release: 2, Decision: "rollback", Reason: "success fell"}
	if err := ss.PutMeasurement(ctx, ta, m); err != nil {
		t.Fatal(err)
	}
	if ok, err := ss.Rollback(ctx, ta, agent, path, 2, "success fell"); err != nil || !ok {
		t.Fatalf("rollback %v %v", ok, err)
	}
	if ok, _ := ss.Rollback(ctx, ta, agent, path, 2, "again"); ok {
		t.Fatal("rolled back twice")
	}
	if got, _ := li.Skills(ctx, ta, agent); len(got) != 1 || got[0].Release != 1 {
		t.Fatalf("after rollback, release 1 should be served: %+v", got)
	}
	ri, err := store.SkillReleases(ctx, ta, agent, "")
	if err != nil || len(ri) != 2 || ri[0].State != "rolled_back" || ri[0].Measurement == nil || !ri[1].Serving {
		t.Fatalf("release info %+v %v", ri, err)
	}
	if rb, err := store.RecentRollbacks(ctx, ta, now.Add(-time.Hour)); err != nil || len(rb) != 1 || rb[0].Release != 2 {
		t.Fatalf("rollbacks %+v %v", rb, err)
	}
	if rb, _ := store.RecentRollbacks(ctx, tb, now.Add(-time.Hour)); len(rb) != 0 {
		t.Fatal("tenant B sees tenant A's rollbacks")
	}

	// Kill switches reach the SDK config; retiring withdraws every release.
	cfg := &ConfigResolver{DB: ingestDB, TTL: time.Nanosecond}
	killed := func() string {
		t.Helper()
		pol, err := cfg.Policy(ctx, ta)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(pol.Config.KilledSkills, ",")
	}
	if k := killed(); k != "" {
		t.Fatalf("killed %q before any kill", k)
	}
	if err := store.SetSkillKilled(ctx, ta, ua, agent, path, true); err != nil {
		t.Fatal(err)
	}
	if k := killed(); k != agent+"/"+path {
		t.Fatalf("killed = %q", k)
	}
	if got, _ := li.Skills(ctx, ta, agent); len(got) != 0 {
		t.Fatal("a killed skill is still served")
	}
	if err := store.SetSkillKilled(ctx, ta, ua, agent, path, false); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgentSkillSettings(ctx, ta, ua, AgentSkillSettings{Agent: agent, Ship: true, KillAll: true}); err != nil {
		t.Fatal(err)
	}
	if k := killed(); k != agent+"/*" {
		t.Fatalf("kill all = %q", k)
	}
	if err := store.PutAgentSkillSettings(ctx, ta, ua, AgentSkillSettings{Agent: agent, Ship: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := li.Skills(ctx, ta, agent); len(got) != 1 {
		t.Fatal("unkilled skill not served")
	}
	note, err := store.GetVaultNote(ctx, ta, agent, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.VaultSetStatus(ctx, ta, agent, path, note.Status, "retired", note.SHA256, "retire", "user:"+ua); err != nil || !ok {
		t.Fatalf("retire %v %v", ok, err)
	}
	if got, _ := li.Skills(ctx, ta, agent); len(got) != 0 {
		t.Fatal("a retired skill is still served")
	}
	if set, err := store.GetLearningSettings(ctx, ta); err != nil || set.ProofThreshold != 80 {
		t.Fatalf("settings %+v %v", set, err)
	}
	if _, err := store.PutLearningSettings(ctx, ta, ua, LearningSettings{MinRuns: 5, ProofThreshold: 90}); err != nil {
		t.Fatal(err)
	}
	if thr, _ := li.ProofThreshold(ctx, ta); thr != 90 {
		t.Fatalf("threshold after saving: %d", thr)
	}
}
