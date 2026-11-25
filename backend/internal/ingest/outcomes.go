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
	"time"

	"github.com/bindle-ai/bindle/backend/internal/auth"
	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/outcomes"
	"github.com/bindle-ai/bindle/backend/internal/ratelimit"
)

// OutcomeStore is where outcome events go (control.OutcomeIngest).
type OutcomeStore interface {
	Settings(ctx context.Context, tenant string) (outcomes.Settings, error)
	EndpointSecret(ctx context.Context, id string) (tenant, secret string, ok bool, err error)
	Record(ctx context.Context, ev outcomes.Event) (stored outcomes.Event, created bool, err error)
	Status(ctx context.Context, tenant, id string) (outcomes.Event, error)
}

// OutcomeHandler serves business events reported after a run:
//
//	POST /v1/outcome-events                     ingest key (Authorization: Bearer)
//	GET  /v1/outcome-events/{id}                ingest key: the event's status
//	POST /v1/outcome-events/webhook/{endpoint}  signed (X-Bindle-Signature), no key
//
// Body: {"correlation": {"ticket_id": "T-123"}, "name": "ticket_reopened",
// "success": false, "value"?: n, "comment"?: "...", "occurred_at"?: RFC 3339}.
// Events are stored (202) and matched to runs by the alerts worker within
// about a minute, retried while the run may still be arriving; the response
// says pending, or matched/unmatched for an event already seen. Only for
// workspaces with the outcome_grading feature (404 otherwise).
type OutcomeHandler struct {
	Keys     auth.Resolver
	Store    OutcomeStore
	Features FeatureChecker
	Limiter  *ratelimit.Limiter
	Log      *slog.Logger
	Now      func() time.Time
}

var (
	outcomeEventIDRe = regexp.MustCompile(`^oev_[0-9a-f]{32}$`)
	endpointIDRe     = regexp.MustCompile(`^oep_[0-9a-f]{16}$`)
)

func (h *OutcomeHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *OutcomeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/outcome-events")
	switch {
	case rest == "" || rest == "/":
		h.withKey(w, r, http.MethodPost, h.report)
	case strings.HasPrefix(rest, "/webhook/"):
		h.webhook(w, r, strings.TrimPrefix(rest, "/webhook/"))
	default:
		id := strings.TrimPrefix(rest, "/")
		h.withKey(w, r, http.MethodGet, func(w http.ResponseWriter, r *http.Request, tenant string) { h.status(w, r, tenant, id) })
	}
}

func outcomeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

func (h *OutcomeHandler) withKey(w http.ResponseWriter, r *http.Request, method string, fn func(http.ResponseWriter, *http.Request, string)) {
	if r.Method != method {
		w.Header().Set("Allow", method)
		outcomeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant, ok := h.Keys.Tenant(r)
	if !ok {
		outcomeError(w, http.StatusUnauthorized, "invalid or missing API key")
		return
	}
	if !h.allowed(w, r, tenant) {
		return
	}
	fn(w, r, tenant)
}

// allowed checks the feature flag (404 when off, as if the route didn't
// exist) and the rate limit.
func (h *OutcomeHandler) allowed(w http.ResponseWriter, r *http.Request, tenant string) bool {
	if h.Features == nil || !h.Features.Enabled(r.Context(), tenant, outcomes.Feature) {
		outcomeError(w, http.StatusNotFound, "not found")
		return false
	}
	if h.Limiter != nil && !h.Limiter.Allow(tenant, 1) {
		w.Header().Set("Retry-After", "2")
		outcomeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return false
	}
	return true
}

func readOutcomeBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, outcomes.MaxBodyBytes))
	if err != nil {
		outcomeError(w, http.StatusRequestEntityTooLarge, "body too large (16 KB at most)")
		return nil, false
	}
	return body, true
}

func (h *OutcomeHandler) report(w http.ResponseWriter, r *http.Request, tenant string) {
	body, ok := readOutcomeBody(w, r)
	if !ok {
		return
	}
	h.record(w, r, tenant, body, "api", "")
}

func (h *OutcomeHandler) webhook(w http.ResponseWriter, r *http.Request, endpoint string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		outcomeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !endpointIDRe.MatchString(endpoint) {
		outcomeError(w, http.StatusNotFound, "not found")
		return
	}
	body, ok := readOutcomeBody(w, r)
	if !ok {
		return
	}
	tenant, secret, found, err := h.Store.EndpointSecret(r.Context(), endpoint)
	if err != nil {
		h.unavailable(w, "endpoint lookup failed", err)
		return
	}
	if !found {
		outcomeError(w, http.StatusNotFound, "not found")
		return
	}
	if err := outcomes.VerifySignature(r.Header.Get(outcomes.SignatureHeader), body, secret, h.now()); err != nil {
		outcomeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if !h.allowed(w, r, tenant) {
		return
	}
	h.record(w, r, tenant, body, "webhook", endpoint)
}

func (h *OutcomeHandler) record(w http.ResponseWriter, r *http.Request, tenant string, body []byte, source, endpoint string) {
	settings, err := h.Store.Settings(r.Context(), tenant)
	if err != nil {
		h.unavailable(w, "outcome settings lookup failed", err)
		return
	}
	ev, err := outcomes.Parse(tenant, body, settings, h.now().UTC(), source, endpoint)
	if err != nil {
		outcomeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), outcomes.ErrInvalid.Error()+": "))
		return
	}
	stored, _, err := h.Store.Record(r.Context(), ev)
	if err != nil {
		h.unavailable(w, "outcome event not stored", err)
		return
	}
	writeEventStatus(w, http.StatusAccepted, stored)
}

func (h *OutcomeHandler) status(w http.ResponseWriter, r *http.Request, tenant, id string) {
	if !outcomeEventIDRe.MatchString(id) {
		outcomeError(w, http.StatusNotFound, "not found")
		return
	}
	ev, err := h.Store.Status(r.Context(), tenant, id)
	if errors.Is(err, control.ErrNotFound) {
		outcomeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.unavailable(w, "outcome event lookup failed", err)
		return
	}
	writeEventStatus(w, http.StatusOK, ev)
}

// writeEventStatus answers {"id", "status", and pending: true, matched: n
// (runs) or unmatched: true}.
func writeEventStatus(w http.ResponseWriter, code int, ev outcomes.Event) {
	out := map[string]any{"id": ev.ID, "status": ev.Status}
	switch ev.Status {
	case outcomes.StatusMatched:
		out["matched"] = len(ev.MatchedTraces)
	case outcomes.StatusUnmatched:
		out["unmatched"] = true
	default:
		out["pending"] = true
		out["retry_until"] = ev.ExpiresAt.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(out)
}

func (h *OutcomeHandler) unavailable(w http.ResponseWriter, msg string, err error) {
	if h.Log != nil {
		h.Log.Error(msg, "err", err)
	}
	w.Header().Set("Retry-After", "5")
	outcomeError(w, http.StatusServiceUnavailable, "temporarily unavailable, retry")
}
