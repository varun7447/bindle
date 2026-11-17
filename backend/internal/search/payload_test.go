package search

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/payload"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

func TestUTF8Prefix(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	for n := 0; n <= 21; n++ {
		p := utf8Prefix(s, n)
		if !utf8.ValidString(p) || len(p) > n {
			t.Fatalf("n=%d: %q", n, p)
		}
	}
}

// Needs the same environment as TestIntegrationOpenSearch.
func TestIntegrationPayloadOffload(t *testing.T) {
	base := os.Getenv("BINDLE_OPENSEARCH_TEST_URL")
	if base == "" {
		t.Skip("BINDLE_OPENSEARCH_TEST_URL not set")
	}
	ctx := context.Background()
	admin := &Client{URL: base, User: os.Getenv("BINDLE_OPENSEARCH_TEST_USER"), Password: os.Getenv("BINDLE_OPENSEARCH_TEST_PASSWORD")}
	sfx := randHex(4)
	names := Names{Prefix: "ptest-" + sfx}
	users := Users{Writer: "ptest_writer_" + sfx, Reader: "ptest_reader_" + sfx, WriterPassword: "Wr1!" + randHex(16), ReaderPassword: "Rd1!" + randHex(16)}
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
	blobs := &payload.FileStore{Dir: t.TempDir()}
	w := &Writer{Client: &Client{URL: base, User: users.Writer, Password: users.WriterPassword}, Names: names, Payloads: blobs}
	store := &Store{Client: &Client{URL: base, User: users.Reader, Password: users.ReaderPassword}, Names: names, Payloads: blobs, MaxPayloadBytes: 200 << 10}

	big := strings.Repeat("All work and no play. ", 5000) // ~110 KB
	huge := strings.Repeat("x", 300<<10)                  // over the read cap
	trace, step, step2 := randHex(16), randHex(8), randHex(8)
	start := uint64(time.Now().Add(-time.Minute).UnixNano())
	steps := []model.Step{
		{TenantID: "tenant-a", TraceID: trace, StepID: step, Type: model.StepLLM, Status: model.StatusSuccess,
			StartUnixNano: start, Input: big, Output: "short"},
		{TenantID: "tenant-a", TraceID: trace, StepID: step2, ParentStepID: step, Type: model.StepTool, Status: model.StatusSuccess,
			StartUnixNano: start + 1, Output: huge},
		// Same trace and step IDs in another tenant: must not collide.
		{TenantID: "tenant-b", TraceID: trace, StepID: step, Type: model.StepLLM, Status: model.StatusSuccess,
			StartUnixNano: start, Input: strings.Repeat("tenant b secret ", 5000)},
	}
	if err := w.Write(ctx, steps); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(ctx, steps); err != nil { // retry: same keys, no duplicates
		t.Fatal(err)
	}

	// The index holds only a preview and a reference.
	var raw hits[stepDoc]
	if err := store.search(ctx, "tenant-a", names.StepsPattern(), []map[string]any{term("step_id", step)}, map[string]any{"size": 1}, &raw); err != nil || len(raw.Hits.Hits) != 1 {
		t.Fatalf("raw: %v", err)
	}
	doc := raw.Hits.Hits[0].Source
	if len(doc.Input) != 4<<10 || doc.PayloadRefs["input"].Bytes != int64(len(big)) || doc.InputBytes != len(big) || doc.Output != "short" {
		t.Fatalf("preview %d bytes, ref %+v, input_bytes %d", len(doc.Input), doc.PayloadRefs, doc.InputBytes)
	}

	sd, err := store.GetStep(ctx, "tenant-a", trace, step)
	if err != nil || sd.Input != big || len(sd.TruncatedFields) != 0 {
		t.Fatalf("full input not restored: %d bytes, truncated %v, err %v", len(sd.Input), sd.TruncatedFields, err)
	}
	sd2, _ := store.GetStep(ctx, "tenant-a", trace, step2)
	if len(sd2.Output) != 200<<10 || len(sd2.TruncatedFields) != 1 || sd2.TruncatedFields[0] != "output" {
		t.Fatalf("read cap: %d bytes, %v", len(sd2.Output), sd2.TruncatedFields)
	}
	sdb, _ := store.GetStep(ctx, "tenant-b", trace, step)
	if !strings.HasPrefix(sdb.Input, "tenant b secret") || strings.Contains(sdb.Input, "All work") {
		t.Fatal("tenant isolation of payloads broken")
	}

	// A reference that doesn't match the tenant-derived key is not followed.
	doc.PayloadRefs["input"] = payload.Ref{Key: "tenants/tenant-b/x/y/z/input", Bytes: 1}
	tampered := &query.StepDetail{Input: doc.Input}
	store.loadPayloads(ctx, "tenant-a", doc, tampered)
	if tampered.Input != doc.Input || len(tampered.TruncatedFields) != 1 {
		t.Fatal("tampered reference was followed")
	}
}
