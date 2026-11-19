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
		st := mk("tenant-a", traceA, randHex(8), root, model.StepTool, model.StatusFailure, "search", `{"q":"same"}`, uint64(2000+i))
		st.Name, st.ErrorMessage, st.ErrorPattern = "search", "timeout", "timeout after <n>s"
		if i == 4 {
			st.ErrorPattern = "HTTP <n>"
		}
		batch2 = append(batch2, st)
	}
	batch1[1].InputTokens, batch1[1].CachedTokens = 900, 600
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
	if r.LLMCalls != 1 || r.ToolCalls != 5 || r.CachedTokens != 600 || r.PeakInputTokens != 900 {
		t.Errorf("rollup counters = %+v", r)
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

	ms, err := store.Models(ctx, "tenant-a", from, to)
	if err != nil || len(ms) != 1 || ms[0].Model != "unpriced-model" || ms[0].Calls != 1 || ms[0].Unpriced != 1 || ms[0].CachedTokens != 600 {
		t.Errorf("models = %+v %v", ms, err)
	}
	if spend, err := store.CostByAgent(ctx, "tenant-a", from, to); err != nil || len(spend) != 1 {
		t.Errorf("cost by agent = %v %v", spend, err)
	}

	tools, err := store.Tools(ctx, "tenant-a", from, to)
	if err != nil || len(tools) != 1 || tools[0].ToolName != "search" || tools[0].Calls != 5 || tools[0].Failures != 5 ||
		tools[0].TopError != "timeout after <n>s" || tools[0].Runs != 1 || tools[0].SampleTrace != traceA {
		t.Errorf("tools = %+v %v", tools, err)
	}
	vs, err := store.AgentVersions(ctx, "tenant-a", "agent-a", from, to)
	if err != nil || len(vs) != 1 || vs[0].Runs != 1 || vs[0].ToolCalls != 5 || vs[0].ToolFailures != 5 || vs[0].AvgSteps != 7 {
		t.Errorf("versions = %+v %v", vs, err)
	}
	tu, err := store.ToolUsage(ctx, "tenant-a", "agent-a", from, to)
	if err != nil || len(tu) != 1 || tu[0].Runs != 1 || len(tu[0].Tools) != 1 || tu[0].Tools[0].ToolName != "search" || tu[0].Tools[0].Calls != 5 ||
		tu[0].Tools[0].Runs != 1 || tu[0].Tools[0].Failures != 5 {
		t.Errorf("tool usage = %+v %v", tu, err)
	}
	if got, _ := store.ToolUsage(ctx, "tenant-b", "agent-a", from, to); len(got) != 0 {
		t.Errorf("tenant-b saw tenant-a's tool usage: %+v", got)
	}

	er, err := store.RunsForEval(ctx, "tenant-a", []string{traceA, randHex(16)})
	if err != nil || len(er) != 1 || er[0].TraceID != traceA || len(er[0].Tools) != 1 || er[0].Tools[0] != "search" || er[0].Steps != 7 ||
		!er[0].Failed || er[0].CostKnown || er[0].Input == "" {
		t.Errorf("runs for eval = %+v %v", er, err)
	}
	if got, _ := store.RunsForEval(ctx, "tenant-b", []string{traceA}); len(got) != 0 {
		t.Errorf("tenant-b loaded tenant-a's run: %+v", got)
	}
	if got, _ := store.ListRuns(ctx, "tenant-a", query.RunFilter{From: from, To: to, Limit: 10, Version: "nope"}); len(got) != 0 {
		t.Errorf("version filter: %+v", got)
	}
	v1, v0 := 0.9, 0.2
	scoreSteps := []model.Step{
		{TenantID: "tenant-a", TraceID: traceA, StepID: randHex(8), ParentStepID: randHex(8), Name: "score helpful", Type: model.StepEval, Status: model.StatusSuccess,
			StartUnixNano: start + 9000, ScoreName: "helpful", ScoreValue: &v1, ScorePass: "pass"},
		{TenantID: "tenant-a", TraceID: traceA, StepID: randHex(8), ParentStepID: randHex(8), Name: "score helpful", Type: model.StepEval, Status: model.StatusSuccess,
			StartUnixNano: start + 9001, ScoreName: "helpful", ScoreValue: &v0, ScorePass: "fail"},
	}
	if err := w.Write(ctx, scoreSteps); err != nil {
		t.Fatal(err)
	}
	scores, err := store.Scores(ctx, "tenant-a", "agent-a", from, to)
	if err != nil || len(scores) != 1 || scores[0].Name != "helpful" || scores[0].Count != 2 || scores[0].Passed != 1 || scores[0].Failed != 1 ||
		scores[0].Valued != 2 || scores[0].Average < 0.54 || scores[0].Average > 0.56 {
		t.Errorf("scores = %+v %v", scores, err)
	}

	// Sessions and user feedback.
	var conv []model.Step
	t1, t2 := randHex(16), randHex(16)
	for i, tr := range []string{t1, t2} {
		st := mk("tenant-d", tr, randHex(8), "", model.StepAgent, model.StatusSuccess, "", "hi", uint64(i*1000))
		st.SessionID, st.AgentName = "conv-1", "chat"
		if i == 1 {
			st.Status = model.StatusFailure
		}
		conv = append(conv, st)
	}
	bad := 0.0
	conv = append(conv, model.Step{TenantID: "tenant-d", TraceID: t2, StepID: randHex(8), ParentStepID: randHex(8), Name: "score feedback", Type: model.StepEval,
		Status: model.StatusSuccess, StartUnixNano: start + 5000, ScoreName: "feedback", ScoreValue: &bad, ScorePass: "fail", ScoreSource: "user"})
	if err := w.Write(ctx, conv); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx, "tenant-d", query.SessionFilter{From: from, To: to})
	if err != nil || len(sessions) != 1 || sessions[0].SessionID != "conv-1" || sessions[0].Runs != 2 || sessions[0].FailedRuns != 1 ||
		!sessions[0].LastFailed || sessions[0].Agent != "chat" {
		t.Errorf("sessions = %+v %v", sessions, err)
	}
	if got, _ := store.ListSessions(ctx, "tenant-a", query.SessionFilter{From: from, To: to}); len(got) != 0 {
		t.Errorf("tenant-a sees tenant-d sessions: %+v", got)
	}
	fb, err := store.Feedback(ctx, "tenant-d", []string{t1, t2})
	if err != nil || fb[t2].Negative != 1 || fb[t2].Count != 1 || fb[t2].Average == nil || len(fb) != 1 {
		t.Errorf("feedback = %+v %v", fb, err)
	}
	if rf, _ := store.RecentFeedback(ctx, "tenant-d", from, to); rf[t2].Negative != 1 {
		t.Errorf("recent feedback = %+v", rf)
	}
	if rs, _ := store.RunsByIDs(ctx, "tenant-d", []string{t1, t2}); len(rs) != 2 {
		t.Errorf("runs by IDs = %+v", rs)
	}

	// Full-text search: one hit per run, with a highlighted snippet.
	hitsA, err := store.Search(ctx, "tenant-a", query.SearchFilter{Query: "timeout", From: from, To: to})
	if err != nil || len(hitsA) != 1 || hitsA[0].TraceID != traceA || hitsA[0].Field != "error" || !strings.Contains(hitsA[0].Snippet, "\ue000timeout\ue001") {
		t.Errorf("search = %+v %q %v", hitsA, func() string {
			if len(hitsA) > 0 {
				return hitsA[0].Snippet
			}
			return ""
		}(), err)
	}
	if got, _ := store.Search(ctx, "tenant-b", query.SearchFilter{Query: "timeout", From: from, To: to}); len(got) != 0 {
		t.Errorf("tenant-b found tenant-a content: %+v", got)
	}
	if got, _ := store.Search(ctx, "tenant-a", query.SearchFilter{Query: "\"no such phrase anywhere\"", From: from, To: to}); len(got) != 0 {
		t.Errorf("phrase search = %+v", got)
	}
	if got, err := store.Search(ctx, "tenant-a", query.SearchFilter{Query: "((unbalanced", From: from, To: to}); err != nil {
		t.Errorf("odd syntax must not error: %v %v", got, err)
	}

	// Insights: error patterns within an issue, and unusual runs.
	ins, err := store.Insights(ctx, "tenant-a", from, to)
	if err != nil {
		t.Fatal(err)
	}
	var search *query.Issue
	for i := range ins.Issues {
		if ins.Issues[i].StepName == "search" {
			search = &ins.Issues[i]
		}
	}
	if search == nil || search.Failures != 5 || len(search.Patterns) != 2 ||
		search.Patterns[0].Pattern != "timeout after <n>s" || search.Patterns[0].Failures != 4 || search.Patterns[0].SampleTrace != traceA {
		t.Errorf("issues = %+v", ins.Issues)
	}
	var many []model.Step
	var bigTrace string
	for i := 0; i < 11; i++ {
		tr := randHex(16)
		n := 2
		if i == 10 {
			n, bigTrace = 30, tr
		}
		r := randHex(8)
		for j := 0; j < n; j++ {
			parent := r
			id := randHex(8)
			if j == 0 {
				id, parent = r, ""
			}
			st := mk("tenant-c", tr, id, parent, model.StepTool, model.StatusSuccess, "t", "", uint64(j))
			st.AgentName = "busy"
			many = append(many, st)
		}
	}
	if err := w.Write(ctx, many); err != nil {
		t.Fatal(err)
	}
	ins, err = store.Insights(ctx, "tenant-c", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins.Outliers) != 1 || ins.Outliers[0].Run.TraceID != bigTrace || ins.Outliers[0].Reason != "steps" ||
		ins.Outliers[0].Value != 30 || ins.Outliers[0].Typical != 2 {
		t.Errorf("outliers = %+v (agents %+v)", ins.Outliers, ins.Agents)
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
	if n, err := future.ExpireRollups(ctx); err != nil || n != 15 {
		t.Errorf("retention 31 days later: deleted=%d err=%v, want 15", n, err)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
