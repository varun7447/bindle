package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/passport"
	"github.com/bindle-ai/bindle/backend/internal/query"
	"github.com/bindle-ai/bindle/backend/internal/redact"
)

// MgmtKeyStore manages management keys. Implemented by *control.Store.
type MgmtKeyStore interface {
	ListMgmtKeys(ctx context.Context, tenant string) ([]control.MgmtKey, error)
	CreateMgmtKey(ctx context.Context, tenant, userID, name string, scopes []string) (control.MgmtKey, string, error)
	RevokeMgmtKey(ctx context.Context, tenant, keyID string) error
}

// PassportStore reads passports (pushed through ingest with a management
// key). Implemented by *control.Store.
type PassportStore interface {
	ListPassports(ctx context.Context, tenant string) ([]control.PassportVersion, error)
	PassportHistory(ctx context.Context, tenant, agent string) ([]control.PassportVersion, error)
}

// DriftStore is the optional part of the trace store behind passport drift
// (search.Store implements it).
type DriftStore interface {
	passport.Counter
	PassportDrift(ctx context.Context, tenant string, from, to time.Time) ([]query.DriftSignal, error)
}

var mgmtKeyIDRe = regexp.MustCompile(`^mkey_[0-9a-f]{20}$`)

func (s *Server) passportRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/mgmt-keys", s.listMgmtKeys)
	api.HandleFunc("POST /api/mgmt-keys", s.createMgmtKey)
	api.HandleFunc("POST /api/mgmt-keys/{id}/revoke", s.revokeMgmtKey)
	api.HandleFunc("GET /api/passports", s.listPassports)
	api.HandleFunc("POST /api/passports/validate", s.validatePassport)
	api.HandleFunc("GET /api/agents/passport", s.agentPassport)
	api.HandleFunc("GET /api/passport-drift", s.passportDrift)
}

// passportsOn is featureOn for the passports feature, plus the stores.
func (s *Server) passportsOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.featureOn(w, r, passport.Feature) {
		return false
	}
	if s.Passports == nil || s.MgmtKeys == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "passports are not configured")
		return false
	}
	return true
}

func (s *Server) listMgmtKeys(w http.ResponseWriter, r *http.Request) {
	if !s.passportsOn(w, r) {
		return
	}
	keys, err := s.MgmtKeys.ListMgmtKeys(r.Context(), tenantFrom(r.Context()))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if keys == nil {
		keys = []control.MgmtKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys, "scopes": control.Scopes})
}

func (s *Server) createMgmtKey(w http.ResponseWriter, r *http.Request) {
	if !s.passportsOn(w, r) {
		return
	}
	p := principalFrom(r.Context())
	if !p.CanManageKeys() {
		writeError(w, http.StatusForbidden, "forbidden", "only owners and admins can manage keys")
		return
	}
	var body struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if _, err := control.CleanScopes(body.Scopes); err != nil {
		badRequest(w, "scopes must be one or more of passport:write, vault:sync, skills:write")
		return
	}
	k, secret, err := s.MgmtKeys.CreateMgmtKey(r.Context(), p.TenantID, p.UserID, body.Name, body.Scopes)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "secret": secret})
}

func (s *Server) revokeMgmtKey(w http.ResponseWriter, r *http.Request) {
	if !s.passportsOn(w, r) {
		return
	}
	p := principalFrom(r.Context())
	if !p.CanManageKeys() {
		writeError(w, http.StatusForbidden, "forbidden", "only owners and admins can manage keys")
		return
	}
	id := r.PathValue("id")
	if !mgmtKeyIDRe.MatchString(id) {
		badRequest(w, "invalid key id")
		return
	}
	err := s.MgmtKeys.RevokeMgmtKey(r.Context(), p.TenantID, id)
	if errors.Is(err, control.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "key not found")
		return
	}
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/passports: every agent's active passport.
func (s *Server) listPassports(w http.ResponseWriter, r *http.Request) {
	if !s.passportsOn(w, r) {
		return
	}
	list, err := s.Passports.ListPassports(r.Context(), tenantFrom(r.Context()))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"passports": list})
}

// GET /api/agents/passport?agent=: the agent's active passport (null when it
// has none) and its version history.
func (s *Server) agentPassport(w http.ResponseWriter, r *http.Request) {
	if !s.passportsOn(w, r) {
		return
	}
	agent := r.URL.Query().Get("agent")
	if agent == "" || len(agent) > 128 {
		badRequest(w, "agent is required (at most 128 characters)")
		return
	}
	history, err := s.Passports.PassportHistory(r.Context(), tenantFrom(r.Context()), agent)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	var active *control.PassportVersion
	if len(history) > 0 {
		active = &history[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": agent, "active": active, "history": history})
}

// POST /api/passports/validate: check a passport against the schema and the
// workspace's data classes. Nothing is stored; pushes go through ingest
// with a management key (bindle passport push).
func (s *Server) validatePassport(w http.ResponseWriter, r *http.Request) {
	if !s.passportsOn(w, r) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, passport.MaxBytes+1))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "a passport must be at most 64 KiB")
		return
	}
	known := map[string]bool{}
	for _, l := range redact.Labels {
		known[l] = true
	}
	if s.Config != nil {
		c, err := s.Config.GetSDKConfig(r.Context(), tenantFrom(r.Context()))
		if err != nil {
			s.storeError(w, r, err)
			return
		}
		for _, p := range c.RedactPatterns {
			known[p.Name] = true
		}
	}
	p, err := passport.Parse(body, known)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "passport": p})
}

// GET /api/passport-drift?window=: per-step drift (tools, data classes,
// egress, release actions) counted per kind and agent in the window, and
// rate-limit and budget drift as of now, with what each kind means.
func (s *Server) passportDrift(w http.ResponseWriter, r *http.Request) {
	if !s.passportsOn(w, r) {
		return
	}
	ds, ok := s.Store.(DriftStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "passport drift is not available")
		return
	}
	name, from, to, ok := s.window(w, r, "7d")
	if !ok {
		return
	}
	tenant := tenantFrom(r.Context())
	sigs, err := ds.PassportDrift(r.Context(), tenant, from, to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	list, err := s.Passports.ListPassports(r.Context(), tenant)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	findings := []passport.Finding{}
	agents := []string{}
	for _, v := range list {
		agents = append(agents, v.Agent)
		got, err := passport.Stateful(r.Context(), ds, tenant, v.Passport, from, to)
		if err != nil {
			s.storeError(w, r, err)
			return
		}
		findings = append(findings, got...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": name, "signals": sigs, "findings": findings,
		"agents_with_passport": agents, "explanations": passport.Explain})
}
