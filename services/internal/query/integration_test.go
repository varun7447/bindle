package query

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/services/internal/model"
	"github.com/bindle-ai/bindle/services/internal/sink"
)

// Runs only when BINDLE_CLICKHOUSE_TEST_URL is set, e.g. against the
// docker-compose stack:
//
//	BINDLE_CLICKHOUSE_TEST_URL=http://localhost:8123 \
//	CLICKHOUSE_USER=bindle CLICKHOUSE_PASSWORD=... go test ./internal/query/
//
// It writes rows under two random tenants and never deletes anything.
func TestIntegrationClickHouse(t *testing.T) {
	chURL := os.Getenv("BINDLE_CLICKHOUSE_TEST_URL")
	if chURL == "" {
		t.Skip("BINDLE_CLICKHOUSE_TEST_URL not set")
	}
	user, pass := os.Getenv("CLICKHOUSE_USER"), os.Getenv("CLICKHOUSE_PASSWORD")
	ctx := context.Background()
	tenantA, tenantB := "itest-a-"+randHex(4), "itest-b-"+randHex(4)
	traceA, traceB := randHex(16), randHex(16)
	now := time.Now().UTC()
	base := uint64(now.Add(-time.Minute).UnixNano())

	mk := func(tenant, trace, id, parent, typ, status, tool, input string, off uint64) model.Step {
		return model.Step{
			TenantID: tenant, TraceID: trace, StepID: id, ParentStepID: parent, Name: id,
			Type: typ, Status: status, ToolName: tool, Input: input, Framework: "test",
			AgentName: "itest-agent", Environment: "test", StartUnixNano: base + off, DurationMs: 5,
			Model: map[bool]string{true: "unpriced-model"}[typ == model.StepLLM],
		}
	}
	root := randHex(8)
	steps := []model.Step{
		mk(tenantA, traceA, root, "", model.StepAgent, model.StatusSuccess, "", "", 0),
		mk(tenantA, traceA, randHex(8), root, model.StepLLM, model.StatusSuccess, "", "hi [REDACTED:EMAIL]", 1000),
	}
	for i := 0; i < 5; i++ {
		steps = append(steps, mk(tenantA, traceA, randHex(8), root, model.StepTool, model.StatusFailure, "search", `{"q":"same"}`, uint64(2000+i)))
	}
	steps = append(steps, mk(tenantB, traceB, randHex(8), "", model.StepAgent, model.StatusFailure, "", "", 0))

	w := &sink.ClickHouse{URL: chURL, User: user, Password: pass}
	if err := w.Write(ctx, steps); err != nil {
		t.Fatal(err)
	}
	c := &Client{URL: chURL, User: user, Password: pass}
	from, to := now.Add(-time.Hour), now.Add(time.Minute)

	runs, err := c.ListRuns(ctx, tenantA, RunFilter{From: from, To: to, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].TraceID != traceA {
		t.Fatalf("tenant A runs = %+v", runs)
	}
	r := runs[0]
	if r.Steps != 7 || r.FailedSteps != 5 || r.RootStatus != "success" || r.Environment != "test" || r.CostUnknownSteps != 1 {
		t.Errorf("run rollup = %+v", r)
	}
	if got, _ := c.ListRuns(ctx, tenantA, RunFilter{From: from, To: to, Limit: 10, Status: "ok"}); len(got) != 0 {
		t.Errorf("status=ok should exclude run with failed steps: %+v", got)
	}
	if got, _ := c.ListRuns(ctx, tenantA, RunFilter{From: from, To: to, Limit: 10, Agent: "other"}); len(got) != 0 {
		t.Errorf("agent filter: %+v", got)
	}
	if got, _ := c.ListRuns(ctx, tenantA, RunFilter{From: from, To: to, Limit: 10, AfterStart: r.Started.Time, AfterTrace: r.TraceID}); len(got) != 0 {
		t.Errorf("cursor past last run should be empty: %+v", got)
	}

	// Cross-tenant: B cannot see A's trace or steps.
	if tr, err := c.GetTrace(ctx, tenantB, traceA, 100); err != nil || tr != nil {
		t.Fatalf("tenant B saw tenant A trace: %+v %v", tr, err)
	}
	tr, err := c.GetTrace(ctx, tenantA, traceA, 3)
	if err != nil || tr == nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if len(tr.Steps) != 3 || !tr.Truncated || tr.Run.Steps != 7 {
		t.Errorf("truncation: steps=%d truncated=%v run.steps=%d", len(tr.Steps), tr.Truncated, tr.Run.Steps)
	}
	if !tr.Steps[1].Redacted || tr.Steps[1].InputBytes == 0 {
		t.Errorf("redaction flag: %+v", tr.Steps[1])
	}

	sd, err := c.GetStep(ctx, tenantA, traceA, tr.Steps[1].StepID)
	if err != nil || sd == nil {
		t.Fatalf("GetStep: %v", err)
	}
	if sd.Input != "hi [REDACTED:EMAIL]" || len(sd.RedactedKinds) != 1 || sd.RedactedKinds[0] != "EMAIL" {
		t.Errorf("step detail = %+v", sd)
	}
	if sd, err := c.GetStep(ctx, tenantB, traceA, tr.Steps[1].StepID); err != nil || sd != nil {
		t.Fatalf("tenant B saw tenant A step: %+v %v", sd, err)
	}

	ov, err := c.Overview(ctx, tenantA, from, to, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Runs != 1 || ov.FailedRuns != 1 || ov.Steps != 7 || len(ov.Series) == 0 ||
		len(ov.TopFailingTools) != 1 || ov.TopFailingTools[0].Failures != 5 {
		t.Errorf("overview = %+v", ov)
	}

	loops, err := c.Loops(ctx, tenantA, from, to, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(loops) != 1 || loops[0].Repeats != 5 || loops[0].ToolName != "search" {
		t.Errorf("loops = %+v", loops)
	}
	if loops, _ := c.Loops(ctx, tenantB, from, to, 1); len(loops) != 0 {
		t.Errorf("tenant B loops = %+v", loops)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
