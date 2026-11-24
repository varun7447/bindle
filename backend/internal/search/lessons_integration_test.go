package search

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/lessons"
	"github.com/bindle-ai/bindle/backend/internal/model"
)

func TestIntegrationLessons(t *testing.T) {
	base := os.Getenv("BINDLE_OPENSEARCH_TEST_URL")
	if base == "" {
		t.Skip("BINDLE_OPENSEARCH_TEST_URL not set")
	}
	ctx := context.Background()
	admin := &Client{URL: base, User: os.Getenv("BINDLE_OPENSEARCH_TEST_USER"), Password: os.Getenv("BINDLE_OPENSEARCH_TEST_PASSWORD")}
	sfx := randHex(4)
	names := Names{Prefix: "itest-les-" + sfx}
	users := Users{Writer: "itest_lw_" + sfx, Reader: "itest_lr_" + sfx, WriterPassword: "Wr1!" + randHex(16), ReaderPassword: "Rd1!" + randHex(16)}
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
	var steps []model.Step
	for i := 0; i < 6; i++ {
		tr := randHex(16)
		steps = append(steps, model.Step{TenantID: "tenant-a", TraceID: tr, StepID: randHex(8), Name: "lookup_order", Type: model.StepTool,
			Status: model.StatusFailure, ToolName: "lookup_order", AgentName: "refund", ErrorMessage: "order 12 not found",
			ErrorPattern: "order <N> not found", Input: `{"id": 12}`, Output: "not found", StartUnixNano: start + uint64(i)})
	}
	// Another tenant's identical failures, and a successful tool call.
	steps = append(steps,
		model.Step{TenantID: "tenant-b", TraceID: randHex(16), StepID: randHex(8), Name: "lookup_order", Type: model.StepTool,
			Status: model.StatusFailure, ToolName: "lookup_order", AgentName: "refund", ErrorPattern: "order <N> not found", StartUnixNano: start},
		model.Step{TenantID: "tenant-a", TraceID: randHex(16), StepID: randHex(8), Name: "send_email", Type: model.StepTool,
			Status: model.StatusSuccess, ToolName: "send_email", AgentName: "refund", StartUnixNano: start})
	if err := w.Write(ctx, steps); err != nil {
		t.Fatal(err)
	}
	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	gs, err := store.LessonGroups(ctx, "tenant-a", from, to)
	if err != nil || len(gs) != 1 {
		t.Fatalf("groups %+v %v", gs, err)
	}
	g := gs[0]
	if g.Runs != 6 || g.Failures != 6 || g.Tool != "lookup_order" || g.StepType != model.StepTool || len(g.Traces) != 6 || len(g.Samples) != lessons.PackEvidence {
		t.Fatalf("group %+v", g)
	}
	if g.Samples[0].Input == "" || g.LastSeen.Before(g.FirstSeen) {
		t.Fatalf("samples %+v", g.Samples[0])
	}
	ls := lessons.Build(gs, 5, true)
	if len(ls) != 1 || ls[0].Cause != lessons.CauseNotFound {
		t.Fatalf("lessons %+v", ls)
	}
	if gb, _ := store.LessonGroups(ctx, "tenant-b", from, to); len(gb) != 1 || gb[0].Runs != 1 {
		t.Fatalf("tenant-b groups %+v", gb)
	}
	tools, err := store.AgentToolNames(ctx, "tenant-a", from, to)
	if err != nil || strings.Join(tools["refund"], ",") != "lookup_order,send_email" {
		t.Fatalf("tools %v %v", tools, err)
	}
}
