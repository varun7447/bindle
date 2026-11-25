package search

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/correlate"
	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/outcomes"
)

func TestIntegrationCorrelations(t *testing.T) {
	base := os.Getenv("BINDLE_OPENSEARCH_TEST_URL")
	if base == "" {
		t.Skip("BINDLE_OPENSEARCH_TEST_URL not set")
	}
	ctx := context.Background()
	admin := &Client{URL: base, User: os.Getenv("BINDLE_OPENSEARCH_TEST_USER"), Password: os.Getenv("BINDLE_OPENSEARCH_TEST_PASSWORD")}
	sfx := randHex(4)
	names := Names{Prefix: "itest-corr-" + sfx}
	users := Users{Writer: "itest_cw_" + sfx, Reader: "itest_cr_" + sfx, WriterPassword: "Wr1!" + randHex(16), ReaderPassword: "Rd1!" + randHex(16)}
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

	now := time.Now().UTC()
	start := uint64(now.Add(-time.Minute).UnixNano())
	trA, trB, trC := randHex(16), randHex(16), randHex(16)
	cA := correlate.FromValue("tenant-a", "ticket_id", "T-1")
	cB := correlate.FromValue("tenant-b", "ticket_id", "T-1") // same ticket, other workspace
	steps := []model.Step{
		{TenantID: "tenant-a", TraceID: trA, StepID: randHex(8), Name: "run", Type: model.StepAgent, Status: model.StatusSuccess,
			AgentName: "support", AgentVersion: "v12", StartUnixNano: start, Correlations: []string{cA}},
		{TenantID: "tenant-a", TraceID: trA, StepID: randHex(8), Name: "answer_quality", Type: model.StepEval, Status: model.StatusSuccess,
			AgentName: "support", AgentVersion: "v12", StartUnixNano: start + 10, ScoreName: "answer_quality", ScorePass: "pass"},
		{TenantID: "tenant-a", TraceID: trC, StepID: randHex(8), Name: "run", Type: model.StepAgent, Status: model.StatusSuccess,
			AgentName: "support", AgentVersion: "v12", StartUnixNano: start + 20},
		{TenantID: "tenant-a", TraceID: trC, StepID: randHex(8), Name: "ticket_resolved", Type: model.StepEval, Status: model.StatusSuccess,
			StartUnixNano: start + 30, ScoreName: "ticket_resolved", ScorePass: "pass", ScoreSource: "outcome"},
		{TenantID: "tenant-b", TraceID: trB, StepID: randHex(8), Name: "run", Type: model.StepAgent, Status: model.StatusSuccess,
			AgentName: "support", StartUnixNano: start, Correlations: []string{cB}},
	}
	if err := w.Write(ctx, steps); err != nil {
		t.Fatal(err)
	}
	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	got, err := store.RunsByCorrelation(ctx, "tenant-a", []string{cA}, from, to)
	if err != nil || len(got) != 1 || got[0] != trA {
		t.Fatalf("tenant-a runs = %v %v", got, err)
	}
	// tenant-a's hash never finds tenant-b's run, and vice versa.
	if got, _ := store.RunsByCorrelation(ctx, "tenant-b", []string{cA}, from, to); len(got) != 0 {
		t.Fatalf("cross-tenant match: %v", got)
	}
	if got, _ := store.RunsByCorrelation(ctx, "tenant-b", []string{cB}, from, to); len(got) != 1 || got[0] != trB {
		t.Fatalf("tenant-b runs = %v", got)
	}

	runs, inRun, truncated, err := store.GradingFacts(ctx, "tenant-a", from, to)
	if err != nil || truncated || len(runs) != 2 {
		t.Fatalf("grading facts %+v %v %v", runs, truncated, err)
	}
	if len(inRun[trC]) != 1 || inRun[trC][0].Name != "ticket_resolved" {
		t.Fatalf("in-run outcomes %+v", inRun)
	}
	grades := outcomes.GradeRuns(runs, map[string][]outcomes.OutcomeFact{trA: {{Name: "ticket_reopened"}}, trC: inRun[trC]},
		outcomes.Settings{ReversalOutcomes: []string{"ticket_reopened"}})
	if len(grades) != 1 || grades[0].Reversed != 1 || grades[0].ReversedAfterPassing != 1 || grades[0].Successes != 1 {
		t.Fatalf("grades %+v", grades)
	}
}
