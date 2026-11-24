package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/bindle-ai/bindle/backend/internal/auth"
	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/skills"
	"github.com/bindle-ai/bindle/backend/internal/vault"
)

// LearningStore is the learning loop's data for the CLI (control.LearningIngest).
type LearningStore interface {
	Lessons(ctx context.Context, tenant, agent string) ([]control.LessonSummary, error)
	Pack(ctx context.Context, tenant, id string) (json.RawMessage, bool, error)
	VaultList(ctx context.Context, tenant, agent string) ([]control.VaultFile, error)
}

// LearningHandler serves the CLI's learning-loop endpoints on ingest:
//
//	GET  /v1/lessons?agent=          lessons (skills:write)
//	GET  /v1/lessons/{id}/pack       a lesson pack for drafting (skills:write)
//	POST /v1/skills                  push a SKILL.md as a draft (skills:write)
//	GET  /v1/vault?agent=            every note of an agent (vault:sync)
//	PUT  /v1/vault                   save one note (vault:sync)
//	GET  /v1/lessons/{id}/cassettes  recorded evidence runs, to prove a skill (skills:write)
//	POST /v1/skills/proof            a proof report from bindle skills prove (skills:write)
//	GET  /v1/skills?agent=&version=  the skills shipped to an agent (ingest key; the SDKs)
//
// All but GET /v1/skills need a management key; the tenant comes from the key. Without the
// learning_loop feature every route answers 404. Packs hold only redacted
// normalized data: drafting runs in the customer's environment, with the
// customer's own model, and Bindle never sees a model key.
type LearningHandler struct {
	Keys       MgmtKeys
	IngestKeys auth.Resolver // GET /v1/skills: the SDKs' ingest keys
	Store      LearningStore
	Ship       ShipStore
	Saver      *vault.Saver
	Features   FeatureChecker
	Limiter    interface{ Allow(key string, n int) bool }
	Log        *slog.Logger
}

var lessonPathRe = regexp.MustCompile(`^/v1/lessons/(les_[0-9a-f]{16})/pack$`)

const maxLearningBody = skills.MaxBytes + 4<<10

func (h *LearningHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/v1/skills" && r.Method == http.MethodGet {
		h.serveSkills(w, r)
		return
	}
	var scope string
	switch {
	case path == "/v1/lessons" && r.Method == http.MethodGet, lessonPathRe.MatchString(path) && r.Method == http.MethodGet,
		cassettesPathRe.MatchString(path) && r.Method == http.MethodGet,
		path == "/v1/skills" && r.Method == http.MethodPost, path == "/v1/skills/proof" && r.Method == http.MethodPost:
		scope = control.ScopeSkillsWrite
	case path == "/v1/vault" && (r.Method == http.MethodGet || r.Method == http.MethodPut):
		scope = control.ScopeVaultSync
	case path == "/v1/lessons", path == "/v1/skills", path == "/v1/skills/proof", path == "/v1/vault", lessonPathRe.MatchString(path),
		cassettesPathRe.MatchString(path):
		jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	default:
		jsonErr(w, http.StatusNotFound, "no such endpoint")
		return
	}
	p, ok, err := h.Keys.Resolve(r.Context(), auth.KeyFromRequest(r))
	if err != nil {
		h.Log.Error("management key lookup failed", "err", err)
		w.Header().Set("Retry-After", "5")
		jsonErr(w, http.StatusServiceUnavailable, "temporarily unavailable, retry")
		return
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="bindle"`)
		jsonErr(w, http.StatusUnauthorized, "a management key (bd_mgmt_...) is required; ingest keys can't read lessons or change the vault")
		return
	}
	if h.Features == nil || !h.Features.Enabled(r.Context(), p.TenantID, skills.Feature) {
		jsonErr(w, http.StatusNotFound, "no such endpoint")
		return
	}
	if !p.Has(scope) {
		jsonErr(w, http.StatusForbidden, "this management key doesn't have the "+scope+" scope")
		return
	}
	if h.Limiter != nil && !h.Limiter.Allow("learn:"+p.TenantID, 1) {
		w.Header().Set("Retry-After", "2")
		jsonErr(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	by := "key:" + p.KeyID
	switch {
	case path == "/v1/lessons":
		h.lessons(w, r, p.TenantID)
	case cassettesPathRe.MatchString(path):
		if h.Ship == nil {
			jsonErr(w, http.StatusNotFound, "no such endpoint")
			return
		}
		h.cassettes(w, r, p.TenantID, cassettesPathRe.FindStringSubmatch(path)[1])
	case path == "/v1/skills/proof":
		if h.Ship == nil {
			jsonErr(w, http.StatusNotFound, "no such endpoint")
			return
		}
		h.proof(w, r, p.TenantID, by)
	case strings.HasPrefix(path, "/v1/lessons/"):
		h.pack(w, r, p.TenantID, lessonPathRe.FindStringSubmatch(path)[1])
	case path == "/v1/skills":
		h.pushSkill(w, r, p.TenantID, by)
	case r.Method == http.MethodGet:
		h.vaultList(w, r, p.TenantID)
	default:
		h.vaultPut(w, r, p.TenantID, by)
	}
}

func (h *LearningHandler) fail(w http.ResponseWriter, what string, err error) {
	h.Log.Error("learning: "+what, "err", err)
	jsonErr(w, http.StatusInternalServerError, "internal error")
}

func (h *LearningHandler) lessons(w http.ResponseWriter, r *http.Request, tenant string) {
	agent := r.URL.Query().Get("agent")
	if len(agent) > vault.MaxAgent {
		jsonErr(w, http.StatusBadRequest, "agent is too long")
		return
	}
	ls, err := h.Store.Lessons(r.Context(), tenant, agent)
	if err != nil {
		h.fail(w, "lessons", err)
		return
	}
	writeJSONBody(w, http.StatusOK, map[string]any{"lessons": ls})
}

func (h *LearningHandler) pack(w http.ResponseWriter, r *http.Request, tenant, id string) {
	pk, ok, err := h.Store.Pack(r.Context(), tenant, id)
	if err != nil {
		h.fail(w, "pack", err)
		return
	}
	if !ok {
		jsonErr(w, http.StatusNotFound, "no such lesson")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(pk)
}

type noteBody struct {
	Agent   string  `json:"agent"`
	Path    string  `json:"path"`
	Content string  `json:"content"`
	Base    *string `json:"base_sha256"`
}

func readNote(w http.ResponseWriter, r *http.Request) (noteBody, bool) {
	var b noteBody
	dec := json.NewDecoder(io.LimitReader(http.MaxBytesReader(w, r.Body, maxLearningBody), maxLearningBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		jsonErr(w, http.StatusBadRequest, "body must be JSON {agent, path, content, base_sha256} of at most 68 KiB")
		return b, false
	}
	return b, true
}

// pushSkill stores a SKILL.md as a draft at skills/<name>/SKILL.md. Without
// base_sha256 it replaces the current version (a new draft); with it, a
// different current text is a conflict.
func (h *LearningHandler) pushSkill(w http.ResponseWriter, r *http.Request, tenant, by string) {
	b, ok := readNote(w, r)
	if !ok {
		return
	}
	sk, err := skills.Parse(b.Content)
	if err != nil {
		h.saveError(w, err)
		return
	}
	path := skills.Path(sk.Name)
	if b.Path != "" && b.Path != path {
		jsonErr(w, http.StatusUnprocessableEntity, "a skill named "+sk.Name+" lives at "+path)
		return
	}
	base := ""
	if b.Base != nil {
		base = *b.Base
	} else if _, err := vault.CleanAgent(b.Agent); err == nil {
		files, err := h.Store.VaultList(r.Context(), tenant, b.Agent)
		if err != nil {
			h.fail(w, "vault list", err)
			return
		}
		for _, f := range files {
			if f.Path == path {
				base = f.SHA256
			}
		}
	}
	h.save(w, r, tenant, b.Agent, path, b.Content, base, by)
}

func (h *LearningHandler) vaultList(w http.ResponseWriter, r *http.Request, tenant string) {
	agent, err := vault.CleanAgent(r.URL.Query().Get("agent"))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	files, err := h.Store.VaultList(r.Context(), tenant, agent)
	if err != nil {
		h.fail(w, "vault list", err)
		return
	}
	writeJSONBody(w, http.StatusOK, map[string]any{"agent": agent, "files": files})
}

func (h *LearningHandler) vaultPut(w http.ResponseWriter, r *http.Request, tenant, by string) {
	b, ok := readNote(w, r)
	if !ok {
		return
	}
	if b.Base == nil {
		jsonErr(w, http.StatusBadRequest, `base_sha256 is required ("" for a new note), so changes made elsewhere aren't overwritten`)
		return
	}
	h.save(w, r, tenant, b.Agent, b.Path, b.Content, *b.Base, by)
}

func (h *LearningHandler) save(w http.ResponseWriter, r *http.Request, tenant, agent, path, content, base, by string) {
	res, err := h.Saver.Save(r.Context(), tenant, agent, path, content, base, by)
	if err != nil {
		h.saveError(w, err)
		return
	}
	switch {
	case res.Conflict:
		writeJSONBody(w, http.StatusConflict, map[string]any{"error": "the note changed since base_sha256; pull, merge and push again",
			"path": res.Path, "current_sha256": res.CurrentSHA})
	case res.Unchanged:
		writeJSONBody(w, http.StatusOK, res)
	default:
		writeJSONBody(w, http.StatusCreated, res)
	}
}

func (h *LearningHandler) saveError(w http.ResponseWriter, err error) {
	if pe, ok := vault.IsPassportError(err); ok {
		writeJSONBody(w, http.StatusUnprocessableEntity, map[string]any{"error": pe.Error(), "passport_check": pe.Check})
		return
	}
	if skills.IsInvalid(err) {
		jsonErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	h.fail(w, "save", err)
}

// CustomLabels returns the workspace's custom redaction pattern names, which
// are data classes too (built-in labels are added by the vault).
func CustomLabels(policies PolicySource) func(ctx context.Context, tenant string) (map[string]bool, error) {
	return func(ctx context.Context, tenant string) (map[string]bool, error) {
		out := map[string]bool{}
		if policies == nil {
			return out, nil
		}
		pol, err := policies.Policy(ctx, tenant)
		if err != nil {
			return nil, err
		}
		for _, pat := range pol.Config.RedactPatterns {
			out[pat.Name] = true
		}
		return out, nil
	}
}
