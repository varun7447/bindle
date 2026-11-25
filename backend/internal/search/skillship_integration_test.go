package search

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/outcomes"
)

// Measuring a shipped skill against real OpenSearch: skills_used is
// matched exactly and by prefix, recurrence by tool and error pattern,
// grading counts failed steps and reversals, and tenants stay apart.
func TestIntegrationSkillRunFacts(t *testing.T) {
	base := os.Getenv("BINDLE_OPENSEARCH_TEST_URL")
	if base == "" {
		t.Skip("BINDLE_OPENSEARCH_TEST_URL not set")
	}
	ctx := context.Background()
	admin := &Client{URL: base, User: os.Getenv("BINDLE_OPENSEARCH_TEST_USER"), Password: os.Getenv("BINDLE_OPENSEARCH_TEST_PASSWORD")}
	sfx := randHex(4)
	names := Names{Prefix: "itest-ship-" + sfx}
	users := Users{Writer: "itest_sw_" + sfx, Reader: "itest_sr_" + sfx, WriterPassword: "Wr1!" + randHex(16), ReaderPassword: "Rd1!" + randHex(16)}
	t.Cleanup(func() {
		_ = admin.Do(ctx, http.MethodDelete, "/"+names.p()+"-*", nil, nil, nil)
		_ = admin.Do(ctx, http.MethodDelete, "/_index_template/"+names.template(), nil, nil, nil)
		_ = admin.Do(ctx, http.MethodDelete, "/_plugins/_ism/policies/"+names.retentionPolicy(), nil, nil, nil)
		for _, u := range []string{users.Writer, users.Reader} {
			for _, kind := range []string{"rolesmapping", "roles", "internalusers"} {
				_ = admin.Do(ctx, http.MethodDelete, "/_plugins/_security/api/"+kind+"/"+u, nil, nil, nil)
			}
		}
	})
	if err := Setup(ctx, admin, SetupOptions{Names: names, Users: users, Replicas: 0}); err != nil {
		t.Fatal(err)
	}
	w := &Writer{Client: &Client{URL: base, User: users.Writer, Password: users.WriterPassword}, Names: names}
	store := &Store{Client: &Client{URL: base, User: users.Reader, Password: users.ReaderPassword}, Names: names}

	const path = "skills/retry-refund/SKILL.md"
	now := time.Now().UTC()
	start := uint64(now.Add(-time.Minute).UnixNano())
	var steps []model.Step
	run := func(tenant string, loaded []string, fail bool, outcome string) string {
		tr := randHex(16)
		root := model.Step{TenantID: tenant, TraceID: tr, StepID: randHex(8), Name: "refunds run", Type: model.StepAgent, Status: model.StatusSuccess,
			AgentName: "refunds", StartUnixNano: start, DurationMs: 100}
		llm := model.Step{TenantID: tenant, TraceID: tr, StepID: randHex(8), ParentStepID: root.StepID, Name: "plan", Type: model.StepLLM,
			Status: model.StatusSuccess, AgentName: "refunds", InputTokens: 100, OutputTokens: 20, SkillsUsed: loaded, StartUnixNano: start + 1}
		steps = append(steps, root, llm)
		if fail {
			steps = append(steps, model.Step{TenantID: tenant, TraceID: tr, StepID: randHex(8), ParentStepID: root.StepID, Name: "refund_payment",
				Type: model.StepTool, ToolName: "refund_payment", Status: model.StatusFailure, AgentName: "refunds",
				ErrorPattern: "timed out", ErrorMessage: "timed out", StartUnixNano: start + 2})
		}
		if outcome != "" {
			v := 0.0
			steps = append(steps, model.Step{TenantID: tenant, TraceID: tr, StepID: randHex(8), ParentStepID: root.StepID, Name: outcome,
				Type: model.StepEval, Status: model.StatusSuccess, AgentName: "refunds", ScoreName: outcome, ScoreValue: &v, ScorePass: "pass",
				ScoreSource: "outcome", StartUnixNano: start + 3})
		}
		return tr
	}
	with := run("tenant-a", []string{path + "@2"}, false, "")
	withReversed := run("tenant-a", []string{path + "@2"}, false, "ticket_reopened")
	other := run("tenant-a", []string{path + "@1"}, false, "")
	without := run("tenant-a", nil, true, "")
	run("tenant-b", []string{path + "@2"}, true, "") // another workspace
	if err := w.Write(ctx, steps); err != nil {
		t.Fatal(err)
	}
	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	settings := outcomes.Settings{ReversalOutcomes: []string{"ticket_reopened"}}
	facts, err := store.SkillRunFacts(ctx, "tenant-a", "refunds", path, 2, SkillLesson{Tool: "refund_payment", Pattern: "timed out"}, from, to, nil, settings)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]bool{}
	for _, f := range facts {
		by[f.TraceID] = true
		switch f.TraceID {
		case with:
			if !f.Loaded || f.Other || !f.Success || f.Recurred || f.Tokens != 120 {
				t.Errorf("with: %+v", f)
			}
		case withReversed:
			if !f.Loaded || f.Success {
				t.Errorf("reversed run counted as a success: %+v", f)
			}
		case other:
			if f.Loaded || !f.Other {
				t.Errorf("another release: %+v", f)
			}
		case without:
			if f.Loaded || f.Other || f.Success || !f.Recurred {
				t.Errorf("without: %+v", f)
			}
		default:
			t.Errorf("a run from another workspace: %+v", f)
		}
	}
	if len(facts) != 4 || !by[with] || !by[without] {
		t.Fatalf("facts %+v", facts)
	}
}
