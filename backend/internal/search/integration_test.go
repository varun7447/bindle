package search

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

// Runs only when BINDLE_OPENSEARCH_TEST_URL is set, with an admin user in
// BINDLE_OPENSEARCH_TEST_USER / BINDLE_OPENSEARCH_TEST_PASSWORD. It uses a
// random index prefix and random users, and deletes them at the end.
func TestIntegrationOpenSearch(t *testing.T) {
	base := os.Getenv("BINDLE_OPENSEARCH_TEST_URL")
	if base == "" {
		t.Skip("BINDLE_OPENSEARCH_TEST_URL not set")
	}
	ctx := context.Background()
	admin := &Client{URL: base, User: os.Getenv("BINDLE_OPENSEARCH_TEST_USER"), Password: os.Getenv("BINDLE_OPENSEARCH_TEST_PASSWORD")}
	sfx := randHex(4)
	names := Names{Prefix: "itest-" + sfx}
	users := Users{Writer: "itest_writer_" + sfx, Reader: "itest_reader_" + sfx,
		WriterPassword: "Wr1!" + randHex(16), ReaderPassword: "Rd1!" + randHex(16)}
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

	opts := SetupOptions{Names: names, Users: users, Replicas: 0}
	if err := Setup(ctx, admin, opts); err != nil {
		t.Fatal(err)
	}
	if err := Setup(ctx, admin, opts); err != nil {
		t.Fatalf("Setup must be idempotent: %v", err)
	}

	writerClient := &Client{URL: base, User: users.Writer, Password: users.WriterPassword}
	readerClient := &Client{URL: base, User: users.Reader, Password: users.ReaderPassword}
	w := &Writer{Client: writerClient, Names: names}
	store := &Store{Client: readerClient, Names: names}

	now := time.Now().UTC()
	start := uint64(now.Add(-time.Minute).UnixNano()) + 123 // exact nanos must survive
	traceA, traceB := randHex(16), randHex(16)
	root := randHex(8)
	mk := func(tenant, trace, id, parent, typ, status, tool, input string, off uint64) model.Step {
		return model.Step{
			TenantID: tenant, TraceID: trace, StepID: id, ParentStepID: parent, Name: id, Type: typ, Status: status,
			ToolName: tool, Input: input, AgentName: "agent-a", Environment: "test", Framework: "test",
			StartUnixNano: start + off, DurationMs: 5, Attributes: map[string]string{"k": "v"}, Links: []string{"t:s"},
		}
	}
	batch1 := []model.Step{
		mk("tenant-a", traceA, root, "", model.StepAgent, model.StatusSuccess, "", "", 0),
		mk("tenant-a", traceA, randHex(8), root, model.StepLLM, model.StatusSuccess, "", "hi [REDACTED:EMAIL] [REDACTED:SECRET]", 1000),
		mk("tenant-b", traceB, randHex(8), "", model.StepAgent, model.StatusFailure, "", "", 0),
	}
	batch1[1].Model = "unpriced-model"
	var batch2 []model.Step
	for i := 0; i < 5; i++ {
		batch2 = append(batch2, mk("tenant-a", traceA, randHex(8), root, model.StepTool, model.StatusFailure, "search", `{"q":"same"}`, uint64(2000+i)))
	}
	for _, b := range [][]model.Step{batch1, batch2, batch1} { // batch1 twice: a retry must not double-count
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
		r.Agent != "agent-a" || r.Environment != "test" || r.Started.UnixNano() != int64(start) {
		t.Errorf("rollup = %+v (want 7 steps, 5 failed, started %d)", r, start)
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
	if got, err := store.ListRuns(ctx, "tenant-a", query.RunFilter{From: from, To: to, Limit: 10, Status: "failed"}); err != nil || len(got) != 1 {
		t.Errorf("status=failed: %v %v", got, err)
	}

	if tr, err := store.GetTrace(ctx, "tenant-b", traceA, 100); err != nil || tr != nil {
		t.Fatalf("tenant-b saw tenant-a trace: %+v %v", tr, err)
	}
	tr, err := store.GetTrace(ctx, "tenant-a", traceA, 3)
	if err != nil || tr == nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if len(tr.Steps) != 3 || !tr.Truncated || !tr.Steps[1].Redacted || tr.Steps[1].InputBytes == 0 || tr.Steps[0].StartTime.UnixNano() != int64(start) {
		t.Errorf("trace = %+v", tr)
	}
	sd, err := store.GetStep(ctx, "tenant-a", traceA, tr.Steps[1].StepID)
	if err != nil || sd == nil {
		t.Fatalf("GetStep: %v", err)
	}
	if strings.Join(sd.RedactedKinds, ",") != "EMAIL,SECRET" || sd.Attributes["k"] != "v" || len(sd.Links) != 1 || !strings.Contains(sd.Input, "[REDACTED:EMAIL]") {
		t.Errorf("step = %+v", sd)
	}
	if sd, err := store.GetStep(ctx, "tenant-b", traceA, tr.Steps[1].StepID); err != nil || sd != nil {
		t.Fatalf("tenant-b saw tenant-a step: %+v %v", sd, err)
	}

	ov, err := store.Overview(ctx, "tenant-a", from, to, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Runs != 1 || ov.FailedRuns != 1 || ov.Steps != 7 || ov.CostUnknownSteps != 1 || len(ov.Series) == 0 ||
		len(ov.TopFailingTools) != 1 || ov.TopFailingTools[0].Failures != 5 || ov.TopFailingTools[0].Calls != 5 {
		t.Errorf("overview = %+v", ov)
	}
	loops, err := store.Loops(ctx, "tenant-a", from, to, 5)
	if err != nil || len(loops) != 1 || loops[0].Repeats != 5 || loops[0].ToolName != "search" || loops[0].TraceID != traceA {
		t.Errorf("loops = %+v %v", loops, err)
	}
	if loops, _ := store.Loops(ctx, "tenant-b", from, to, 1); len(loops) != 0 {
		t.Errorf("tenant-b loops = %+v", loops)
	}

	// Privileges.
	day := now.Format("2006.01.02")
	denied := func(c *Client, method, path string, body any) bool {
		err := c.Do(ctx, method, path, nil, body, nil)
		return IsStatus(err, http.StatusForbidden)
	}
	doc := map[string]any{"tenant_id": "tenant-a", "trace_id": "x"}
	if !denied(readerClient, http.MethodPost, "/"+names.StepsIndex(day)+"/_doc", doc) {
		t.Error("reader could write a step")
	}
	if !denied(readerClient, http.MethodPost, "/"+names.Traces()+"/_delete_by_query", map[string]any{"query": map[string]any{"match_all": map[string]any{}}}) {
		t.Error("reader could delete rollups")
	}
	if !denied(writerClient, http.MethodDelete, "/"+names.StepsIndex(day), nil) {
		t.Error("writer could delete a step index")
	}
	if !denied(writerClient, http.MethodPost, "/"+names.StepsIndex(day)+"/_delete_by_query", map[string]any{"query": map[string]any{"match_all": map[string]any{}}}) {
		t.Error("writer could delete steps")
	}
	if !denied(writerClient, http.MethodPut, "/_plugins/_security/api/internalusers/evil_"+sfx, map[string]any{"password": "Ev1!" + randHex(16)}) {
		t.Error("writer could create users")
	}
	if !denied(readerClient, http.MethodGet, "/"+names.p()+"-other/_search", nil) && !IsStatus(readerClient.Do(ctx, http.MethodGet, "/"+names.p()+"-other/_search", nil, nil, nil), http.StatusNotFound) {
		t.Error("reader could read outside its indices")
	}

	// Out-of-window steps are dropped, not failed.
	old := mk("tenant-a", randHex(16), randHex(8), "", model.StepAgent, model.StatusSuccess, "", "", 0)
	old.StartUnixNano = uint64(now.Add(-60 * 24 * time.Hour).UnixNano())
	if err := w.Write(ctx, []model.Step{old}); err != nil {
		t.Errorf("out-of-window step should be dropped without error: %v", err)
	}

	// Rollup retention runs as the writer and removes only expired rollups.
	if n, err := w.ExpireRollups(ctx); err != nil || n != 0 {
		t.Errorf("retention with nothing expired: deleted=%d err=%v", n, err)
	}
	future := &Writer{Client: writerClient, Names: names, Now: func() time.Time { return now.Add(31 * 24 * time.Hour) }}
	if n, err := future.ExpireRollups(ctx); err != nil || n != 2 {
		t.Errorf("retention 31 days later: deleted=%d err=%v, want 2", n, err)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
