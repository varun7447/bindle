package ingest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/auth"
	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/skillship"
)

type fakeShip struct {
	threshold int
	proofs    []skillship.Verdict
	changed   bool
	releases  map[string][]skillship.Release // tenant/agent
}

func (f *fakeShip) Cassettes(_ context.Context, tenant, lesson string) (json.RawMessage, error) {
	if tenant != "acme" {
		return json.RawMessage(`[]`), nil
	}
	return json.RawMessage(`[{"format":"bindle.cassette","trace_id":"` + strings.Repeat("a", 32) + `"}]`), nil
}
func (f *fakeShip) ProofThreshold(context.Context, string) (int, error) { return f.threshold, nil }
func (f *fakeShip) AddProof(_ context.Context, tenant string, r skillship.Report, v skillship.Verdict, by string) (control.ProofResult, error) {
	if f.changed {
		return control.ProofResult{}, control.ErrChanged
	}
	f.proofs = append(f.proofs, v)
	if v.Passed {
		return control.ProofResult{Proven: true, Version: 4, Status: "proven"}, nil
	}
	return control.ProofResult{Version: 3, Status: "draft"}, nil
}
func (f *fakeShip) Skills(_ context.Context, tenant, agent string) ([]skillship.Release, error) {
	return f.releases[tenant+"/"+agent], nil
}

func shipHandler(t *testing.T, ship *fakeShip) *LearningHandler {
	t.Helper()
	ingestKeys, _ := auth.ParseEnv("bd_test_good=acme,bd_test_other=globex")
	return &LearningHandler{
		Keys: fakeMgmt{
			"bd_mgmt_skills": {TenantID: "acme", KeyID: "mkey_1", Scopes: []string{control.ScopeSkillsWrite}},
			"bd_mgmt_vault":  {TenantID: "acme", KeyID: "mkey_2", Scopes: []string{control.ScopeVaultSync}},
		},
		IngestKeys: ingestKeys, Store: &fakeLearning{notes: map[string]control.VaultFile{}}, Ship: ship,
		Features: fakeFeatures{"acme/learning_loop": true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func call(h http.Handler, method, path, key, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func proofBody(fixed, total int) string {
	r := skillship.Report{Agent: "refunds", Path: "skills/retry-refund/SKILL.md", SHA256: strings.Repeat("b", 64), Lesson: "les_0123456789abcdef"}
	for i := 0; i < total; i++ {
		r.Runs = append(r.Runs, skillship.RunResult{TraceID: strings.Repeat("0", 31) + string(rune('a'+i)), Covered: true, Fixed: i < fixed})
	}
	b, _ := json.Marshal(r)
	return string(b)
}

func TestProofUpload(t *testing.T) {
	ship := &fakeShip{threshold: 80}
	h := shipHandler(t, ship)
	if w := call(h, "POST", "/v1/skills/proof", "bd_test_good", proofBody(5, 5)); w.Code != http.StatusUnauthorized {
		t.Fatalf("ingest key: %d (proofs need a management key)", w.Code)
	}
	if w := call(h, "POST", "/v1/skills/proof", "bd_mgmt_vault", proofBody(5, 5)); w.Code != http.StatusForbidden {
		t.Fatalf("vault:sync key: %d, want 403", w.Code)
	}
	w := call(h, "POST", "/v1/skills/proof", "bd_mgmt_skills", proofBody(5, 5))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"proven":true`) {
		t.Fatalf("passing proof: %d %s", w.Code, w.Body)
	}
	w = call(h, "POST", "/v1/skills/proof", "bd_mgmt_skills", proofBody(3, 5))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"proven":false`) || !strings.Contains(w.Body.String(), "80% needed") {
		t.Fatalf("failing proof: %d %s", w.Code, w.Body)
	}
	if len(ship.proofs) != 2 || ship.proofs[0].Threshold != 80 {
		t.Fatalf("verdicts %+v", ship.proofs)
	}
	if w := call(h, "POST", "/v1/skills/proof", "bd_mgmt_skills", `{"agent":"refunds"}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid report: %d", w.Code)
	}
	ship.changed = true
	if w := call(h, "POST", "/v1/skills/proof", "bd_mgmt_skills", proofBody(5, 5)); w.Code != http.StatusConflict {
		t.Fatalf("changed skill: %d, want 409", w.Code)
	}
}

func TestCassettesForProving(t *testing.T) {
	h := shipHandler(t, &fakeShip{})
	w := call(h, "GET", "/v1/lessons/les_0123456789abcdef/cassettes", "bd_mgmt_skills", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"cassettes":[{"format":"bindle.cassette"`) {
		t.Fatalf("cassettes: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/v1/lessons/les_0123456789abcdef/cassettes", "bd_test_good", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("ingest key: %d", w.Code)
	}
}

const shippedSkill = "---\nname: retry-refund\ndescription: Retry a refund once on a timeout. Use when refund_payment times out.\n---\n\nRetry once.\n"

func TestServeSkillsToAgents(t *testing.T) {
	ship := &fakeShip{releases: map[string][]skillship.Release{
		"acme/refunds": {{Agent: "refunds", Path: "skills/retry-refund/SKILL.md", Release: 2, SHA256: "s", Body: shippedSkill}},
	}}
	h := shipHandler(t, ship)
	if w := call(h, "GET", "/v1/skills?agent=refunds", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", w.Code)
	}
	if w := call(h, "GET", "/v1/skills?agent=refunds", "bd_mgmt_skills", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("management key: %d (the SDKs use ingest keys)", w.Code)
	}
	w := call(h, "GET", "/v1/skills?agent=refunds&version=v12", "bd_test_good", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"retry-refund"`) || !strings.Contains(w.Body.String(), `"release":2`) {
		t.Fatalf("skills: %d %s", w.Code, w.Body)
	}
	etag := w.Header().Get("ETag")
	if w := call(h, "GET", "/v1/skills?agent=refunds&version=v12", "bd_test_good", "", "If-None-Match", etag); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("unchanged set: %d", w.Code)
	}
	// Another workspace with the same agent name gets nothing, and without
	// the feature the endpoint doesn't exist.
	if w := call(h, "GET", "/v1/skills?agent=refunds", "bd_test_other", ""); w.Code != http.StatusNotFound {
		t.Fatalf("feature off: %d", w.Code)
	}
	h.Features = fakeFeatures{"acme/learning_loop": true, "globex/learning_loop": true}
	if w := call(h, "GET", "/v1/skills?agent=refunds", "bd_test_other", ""); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "retry-refund") {
		t.Fatalf("other tenant: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/v1/skills", "bd_test_good", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("no agent: %d", w.Code)
	}
}
