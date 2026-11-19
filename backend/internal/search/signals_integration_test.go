package search

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
)

// Runs against a real cluster (see TestIntegrationOpenSearch): security
// signal and topic aggregations, with the tenant pinned in the background.
func TestIntegrationSignalsAndTopics(t *testing.T) {
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
	if err := Setup(ctx, admin, SetupOptions{Names: names, Users: users}); err != nil {
		t.Fatal(err)
	}
	w := &Writer{Client: &Client{URL: base, User: users.Writer, Password: users.WriterPassword}, Names: names}
	store := &Store{Client: &Client{URL: base, User: users.Reader, Password: users.ReaderPassword}, Names: names}

	now := time.Now().UTC().Truncate(time.Second)
	day := 24 * time.Hour
	var steps []model.Step
	n := 0
	add := func(tenant, input string, at time.Time, llmCalls int) {
		trace := randHex(16)
		for i := 0; i < llmCalls; i++ { // each run counts once, however many calls it makes
			n++
			steps = append(steps, model.Step{TenantID: tenant, TraceID: trace, StepID: randHex(8), Name: "chat", Type: model.StepLLM,
				Status: model.StatusSuccess, AgentName: "support", UserInput: input, StartUnixNano: uint64(at.Add(time.Duration(n) * time.Millisecond).UnixNano())})
		}
	}
	for i := 0; i < 20; i++ {
		add("tenant-a", fmt.Sprintf("where is my order %d", i), now.Add(-30*time.Hour), 2) // the day before
		add("tenant-a", fmt.Sprintf("where is my order %d", i), now.Add(-2*time.Hour), 1)
	}
	for i := 0; i < 6; i++ {
		add("tenant-a", "the checkout outage blocks my refund", now.Add(-time.Hour), 3)
		add("tenant-a", "refund for a late parcel", now.Add(-28*time.Hour), 1)
	}
	for i := 0; i < 8; i++ {
		add("tenant-a", "refund please", now.Add(-90*time.Minute), 1)
	}
	// Another tenant talked about "outage" the day before: that must not
	// make it look old to tenant-a.
	for i := 0; i < 10; i++ {
		add("tenant-b", "outage outage again", now.Add(-26*time.Hour), 1)
	}
	flag := model.Step{TenantID: "tenant-a", TraceID: randHex(16), StepID: randHex(8), Name: "execute_tool fetch", Type: model.StepTool, ToolName: "fetch",
		Status: model.StatusSuccess, AgentName: "support", StartUnixNano: uint64(now.Add(-time.Hour).UnixNano()),
		SecurityFlags: []string{"prompt_injection", "secret_exposed"}, SecurityReasons: []string{"prompt_injection:ignore_instructions", "secret_exposed:credential_in_output"}}
	other := flag
	other.TenantID, other.TraceID, other.StepID = "tenant-b", randHex(16), randHex(8)
	steps = append(steps, flag, other)
	if err := w.Write(ctx, steps); err != nil {
		t.Fatal(err)
	}

	sig, err := store.SecuritySignals(ctx, "tenant-a", now.Add(-day), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 2 || sig[0].Steps != 1 || sig[0].Runs != 1 || sig[0].Agent != "support" || len(sig[0].Examples) != 1 ||
		sig[0].Examples[0].TraceID != flag.TraceID || len(sig[0].Reasons) != 1 {
		t.Fatalf("signals = %+v", sig)
	}

	top, err := store.Topics(ctx, "tenant-a", "support", now.Add(-day), now)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("topics: %+v", top)
	if top.Runs != 34 || top.PreviousRuns != 26 {
		t.Fatalf("runs %d previous %d", top.Runs, top.PreviousRuns)
	}
	isNew := map[string]bool{}
	for _, tp := range top.New {
		isNew[tp.Term] = true
	}
	if !isNew["outage"] || !isNew["checkout"] {
		t.Fatalf("new = %+v", top.New)
	}
	rising := map[string]bool{}
	for _, tp := range top.Rising {
		rising[tp.Term] = true
	}
	for _, steady := range []string{"where", "order", "the", "please"} {
		if rising[steady] || isNew[steady] {
			t.Errorf("steady term %q reported", steady)
		}
	}
	if !rising["refund"] {
		t.Fatalf("rising = %+v", top.Rising)
	}
}
