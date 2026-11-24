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

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/passport"
	"github.com/bindle-ai/bindle/backend/internal/vault"
)

type fakeLearning struct {
	notes map[string]control.VaultFile // tenant/agent/path
}

func (f *fakeLearning) Lessons(_ context.Context, tenant, agent string) ([]control.LessonSummary, error) {
	if tenant != "t1" {
		return []control.LessonSummary{}, nil
	}
	return []control.LessonSummary{{ID: "les_0123456789abcdef", Agent: "refund", Title: "lookup_order failed in 6 runs"}}, nil
}

func (f *fakeLearning) Pack(_ context.Context, tenant, id string) (json.RawMessage, bool, error) {
	if tenant != "t1" || id != "les_0123456789abcdef" {
		return nil, false, nil
	}
	return json.RawMessage(`{"format":"bindle.lesson-pack/v1","lesson":"les_0123456789abcdef"}`), true, nil
}

func (f *fakeLearning) VaultList(_ context.Context, tenant, agent string) ([]control.VaultFile, error) {
	out := []control.VaultFile{}
	for k, v := range f.notes {
		if strings.HasPrefix(k, tenant+"/"+agent+"/") {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeLearning) VaultPut(_ context.Context, in control.VaultPutInput) (control.VaultPutResult, error) {
	k := in.Tenant + "/" + in.Agent + "/" + in.Path
	cur, ok := f.notes[k]
	if ok && cur.SHA256 == in.SHA256 {
		return control.VaultPutResult{Version: cur.Version, Unchanged: true, CurrentSHA: cur.SHA256}, nil
	}
	if (ok && cur.SHA256 != in.BaseSHA256) || (!ok && in.BaseSHA256 != "") {
		return control.VaultPutResult{Version: cur.Version, Conflict: true, CurrentSHA: cur.SHA256}, nil
	}
	f.notes[k] = control.VaultFile{Path: in.Path, Kind: in.Kind, Version: cur.Version + 1, SHA256: in.SHA256, Body: in.Body}
	return control.VaultPutResult{Version: cur.Version + 1, CurrentSHA: in.SHA256}, nil
}

const skillMD = `---
name: retry-lookup
description: Retry lookup_order once with a trimmed ID when it returns nothing.
---
Call lookup_order again.
`

func TestLearningHandler(t *testing.T) {
	store := &fakeLearning{notes: map[string]control.VaultFile{}}
	keys := fakeMgmt{
		"bd_mgmt_skills": {TenantID: "t1", KeyID: "mkey_1", Scopes: []string{control.ScopeSkillsWrite}},
		"bd_mgmt_vault":  {TenantID: "t1", KeyID: "mkey_2", Scopes: []string{control.ScopeVaultSync}},
		"bd_mgmt_t2":     {TenantID: "t2", KeyID: "mkey_3", Scopes: []string{control.ScopeSkillsWrite, control.ScopeVaultSync}},
	}
	pp := &passport.Passport{Agent: "refund", Owner: "o@example.com", Tools: &passport.Tools{Allow: []string{"lookup_*"}}}
	h := &LearningHandler{Keys: keys, Store: store, Features: fakeFeatures{"t1/learning_loop": true},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Saver: &vault.Saver{Store: store,
			Passport: func(_ context.Context, tenant, agent string) (*passport.Passport, error) {
				if tenant == "t1" && agent == "refund" {
					return pp, nil
				}
				return nil, nil
			},
			Tools: func(context.Context, string, string) ([]string, error) {
				return []string{"lookup_order", "send_email"}, nil
			}}}
	do := func(method, path, key, body string) (int, map[string]any) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	body := func(v map[string]any) string { b, _ := json.Marshal(v); return string(b) }

	// Auth, scope and feature gating.
	if c, _ := do("GET", "/v1/lessons", "", ""); c != 401 {
		t.Fatalf("no key: %d", c)
	}
	if c, _ := do("GET", "/v1/lessons", "bd_live_aaaaaaaaaaaaaaaaaaaaaaaa", ""); c != 401 {
		t.Fatalf("ingest key: %d", c)
	}
	if c, _ := do("GET", "/v1/lessons", "bd_mgmt_t2", ""); c != 404 {
		t.Fatalf("feature off: %d", c)
	}
	if c, _ := do("GET", "/v1/lessons", "bd_mgmt_vault", ""); c != 403 {
		t.Fatalf("wrong scope: %d", c)
	}
	if c, _ := do("DELETE", "/v1/vault", "bd_mgmt_vault", ""); c != 405 {
		t.Fatalf("method: %d", c)
	}

	// Lessons and packs.
	if c, out := do("GET", "/v1/lessons?agent=refund", "bd_mgmt_skills", ""); c != 200 || len(out["lessons"].([]any)) != 1 {
		t.Fatalf("lessons %d %v", c, out)
	}
	if c, out := do("GET", "/v1/lessons/les_0123456789abcdef/pack", "bd_mgmt_skills", ""); c != 200 || out["format"] != "bindle.lesson-pack/v1" {
		t.Fatalf("pack %d %v", c, out)
	}
	if c, _ := do("GET", "/v1/lessons/les_ffffffffffffffff/pack", "bd_mgmt_skills", ""); c != 404 {
		t.Fatalf("unknown pack %d", c)
	}

	// Skill push: a draft; pushing again replaces it; outside the passport is rejected.
	c, out := do("POST", "/v1/skills", "bd_mgmt_skills", body(map[string]any{"agent": "refund", "content": skillMD}))
	if c != 201 || out["path"] != "skills/retry-lookup/SKILL.md" || out["status"] != "draft" {
		t.Fatalf("push %d %v", c, out)
	}
	c, out = do("POST", "/v1/skills", "bd_mgmt_skills", body(map[string]any{"agent": "refund", "content": skillMD + "\nAnd once more.\n"}))
	if c != 201 || out["version"].(float64) != 2 {
		t.Fatalf("re-push %d %v", c, out)
	}
	bad := strings.Replace(skillMD, "Call lookup_order again.", "Then call send_email.", 1)
	if c, out := do("POST", "/v1/skills", "bd_mgmt_skills", body(map[string]any{"agent": "refund", "content": bad})); c != 422 || out["passport_check"] == nil {
		t.Fatalf("passport reject %d %v", c, out)
	}
	if c, _ := do("POST", "/v1/skills", "bd_mgmt_skills", body(map[string]any{"agent": "refund", "content": "no frontmatter"})); c != 422 {
		t.Fatalf("invalid skill %d", c)
	}

	// Vault sync: base required, conflicts reported, tenant from the key.
	if c, _ := do("PUT", "/v1/vault", "bd_mgmt_vault", body(map[string]any{"agent": "refund", "path": "notes/a.md", "content": "x"})); c != 400 {
		t.Fatalf("missing base %d", c)
	}
	c, out = do("PUT", "/v1/vault", "bd_mgmt_vault", body(map[string]any{"agent": "refund", "path": "notes/a.md", "content": "first", "base_sha256": ""}))
	if c != 201 {
		t.Fatalf("vault put %d %v", c, out)
	}
	sha := out["sha256"].(string)
	if c, _ := do("PUT", "/v1/vault", "bd_mgmt_vault", body(map[string]any{"agent": "refund", "path": "notes/a.md", "content": "second", "base_sha256": ""})); c != 409 {
		t.Fatalf("conflict %d", c)
	}
	if c, _ := do("PUT", "/v1/vault", "bd_mgmt_vault", body(map[string]any{"agent": "refund", "path": "notes/a.md", "content": "second", "base_sha256": sha})); c != 201 {
		t.Fatalf("update %d", c)
	}
	c, out = do("GET", "/v1/vault?agent=refund", "bd_mgmt_vault", "")
	if c != 200 || len(out["files"].([]any)) != 2 {
		t.Fatalf("list %d %v", c, out)
	}
	if c, out := do("GET", "/v1/vault?agent=refund", "bd_mgmt_t2", ""); c != 404 || out["files"] != nil {
		t.Fatalf("other tenant %d %v", c, out)
	}
	for k := range store.notes {
		if !strings.HasPrefix(k, "t1/") {
			t.Fatalf("note stored for the wrong tenant: %s", k)
		}
	}
	_ = http.StatusOK
}
