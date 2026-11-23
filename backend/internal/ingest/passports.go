package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/bindle-ai/bindle/backend/internal/auth"
	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/passport"
	"github.com/bindle-ai/bindle/backend/internal/ratelimit"
	"github.com/bindle-ai/bindle/backend/internal/redact"
)

// MgmtKeys resolves management keys. Implemented by control.MgmtKeyResolver.
type MgmtKeys interface {
	Resolve(ctx context.Context, key string) (control.MgmtPrincipal, bool, error)
}

// PassportPusher stores passport versions. Implemented by control.PassportWriter.
type PassportPusher interface {
	Push(ctx context.Context, tenant string, p *passport.Passport, by string) (control.PushResult, error)
}

// FeatureChecker reports whether a tenant has a feature on.
type FeatureChecker interface {
	Enabled(ctx context.Context, tenant, feature string) bool
}

// PassportHandler serves the CLI's passport endpoints on ingest, the public
// edge (the API is private):
//
//	POST /v1/passports/validate   check a passport, store nothing
//	PUT  /v1/passports            push it as the agent's next version
//
// Both need a management key (bd_mgmt_...) with the passport:write scope;
// ingest keys are refused, so an agent can't change its own passport. The
// tenant comes from the key. Without the passports feature both answer 404.
type PassportHandler struct {
	Keys     MgmtKeys
	Store    PassportPusher
	Features FeatureChecker
	Policies PolicySource // the workspace's redaction pattern names are valid data classes
	Limiter  *ratelimit.Limiter
	Log      *slog.Logger
}

func (h *PassportHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	validate := r.URL.Path == "/v1/passports/validate"
	switch {
	case validate && r.Method == http.MethodPost, !validate && r.URL.Path == "/v1/passports" && r.Method == http.MethodPut:
	case validate || r.URL.Path == "/v1/passports":
		w.Header().Set("Allow", map[bool]string{true: http.MethodPost, false: http.MethodPut}[validate])
		jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	default:
		jsonErr(w, http.StatusNotFound, "no such endpoint")
		return
	}
	key := auth.KeyFromRequest(r)
	p, ok, err := h.Keys.Resolve(r.Context(), key)
	if err != nil {
		h.Log.Error("management key lookup failed", "err", err)
		w.Header().Set("Retry-After", "5")
		jsonErr(w, http.StatusServiceUnavailable, "temporarily unavailable, retry")
		return
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="bindle"`)
		jsonErr(w, http.StatusUnauthorized, "a management key (bd_mgmt_...) is required; ingest keys can't change passports")
		return
	}
	if h.Features == nil || !h.Features.Enabled(r.Context(), p.TenantID, passport.Feature) {
		jsonErr(w, http.StatusNotFound, "no such endpoint")
		return
	}
	if !p.Has(control.ScopePassportWrite) {
		jsonErr(w, http.StatusForbidden, "this management key doesn't have the passport:write scope")
		return
	}
	if h.Limiter != nil && !h.Limiter.Allow("mgmt:"+p.TenantID, 1) {
		w.Header().Set("Retry-After", "2")
		jsonErr(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, passport.MaxBytes+1))
	if err != nil {
		jsonErr(w, http.StatusRequestEntityTooLarge, "a passport must be at most 64 KiB")
		return
	}
	known, err := h.knownLabels(r.Context(), p.TenantID)
	if err != nil {
		h.Log.Error("policy lookup failed", "tenant", p.TenantID, "err", err)
		w.Header().Set("Retry-After", "5")
		jsonErr(w, http.StatusServiceUnavailable, "temporarily unavailable, retry")
		return
	}
	pp, err := passport.Parse(body, known)
	if err != nil {
		jsonErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if validate {
		writeJSONBody(w, http.StatusOK, map[string]any{"ok": true, "agent": pp.Agent})
		return
	}
	res, err := h.Store.Push(r.Context(), p.TenantID, pp, "key:"+p.KeyID)
	if err != nil {
		h.Log.Error("passport push failed", "tenant", p.TenantID, "err", err)
		jsonErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	status := http.StatusCreated
	if res.Unchanged {
		status = http.StatusOK
	}
	writeJSONBody(w, status, map[string]any{"agent": pp.Agent, "version": res.Version, "pushed_at": res.PushedAt, "unchanged": res.Unchanged})
}

func (h *PassportHandler) knownLabels(ctx context.Context, tenant string) (map[string]bool, error) {
	known := map[string]bool{}
	for _, l := range redact.Labels {
		known[l] = true
	}
	if h.Policies == nil {
		return known, nil
	}
	pol, err := h.Policies.Policy(ctx, tenant)
	if err != nil {
		return nil, err
	}
	for _, pat := range pol.Config.RedactPatterns {
		known[pat.Name] = true
	}
	return known, nil
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	writeJSONBody(w, status, map[string]any{"error": msg})
}

func writeJSONBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		return
	}
}
