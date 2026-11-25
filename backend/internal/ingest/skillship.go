package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/skills"
	"github.com/bindle-ai/bindle/backend/internal/skillship"
	"github.com/bindle-ai/bindle/backend/internal/vault"
)

// ShipStore is phase 2 of the learning loop for ingest (control.LearningIngest).
type ShipStore interface {
	Cassettes(ctx context.Context, tenant, lesson string) (json.RawMessage, error)
	ProofThreshold(ctx context.Context, tenant string) (int, error)
	AddProof(ctx context.Context, tenant string, r skillship.Report, v skillship.Verdict, by string) (control.ProofResult, error)
	Skills(ctx context.Context, tenant, agent string) ([]skillship.Release, error)
}

var cassettesPathRe = regexp.MustCompile(`^/v1/lessons/(les_[0-9a-f]{16})/cassettes$`)

// cassettes serves a lesson's recorded evidence runs for bindle skills
// prove: what Bindle stored (redacted at ingest), nothing more.
func (h *LearningHandler) cassettes(w http.ResponseWriter, r *http.Request, tenant, lesson string) {
	cs, err := h.Ship.Cassettes(r.Context(), tenant, lesson)
	if err != nil {
		h.fail(w, "cassettes", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`{"lesson":"` + lesson + `","cassettes":`))
	_, _ = w.Write(cs)
	_, _ = w.Write([]byte("}"))
}

// proof records a proof report from bindle skills prove. Bindle judges it
// against the workspace's threshold; a passing report moves the draft at
// that exact text to proven. The report comes from the customer's own run
// of their agent; Bindle never runs it.
func (h *LearningHandler) proof(w http.ResponseWriter, r *http.Request, tenant, by string) {
	var rep skillship.Report
	dec := json.NewDecoder(io.LimitReader(http.MaxBytesReader(w, r.Body, skillship.MaxReportBytes), skillship.MaxReportBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		jsonErr(w, http.StatusBadRequest, "body must be a proof report (JSON, at most 256 KiB)")
		return
	}
	if _, err := vault.CleanAgent(rep.Agent); err != nil {
		jsonErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := skillship.Validate(&rep); err != nil {
		jsonErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	threshold, err := h.Ship.ProofThreshold(r.Context(), tenant)
	if err != nil {
		h.fail(w, "proof threshold", err)
		return
	}
	v := skillship.Judge(rep, threshold)
	res, err := h.Ship.AddProof(r.Context(), tenant, rep, v, by)
	switch {
	case errors.Is(err, control.ErrNotFound):
		jsonErr(w, http.StatusNotFound, "no such skill; push it first (bindle skills push)")
		return
	case errors.Is(err, control.ErrChanged):
		jsonErr(w, http.StatusConflict, "the skill changed since this text was proven; prove the current text")
		return
	case err != nil:
		h.fail(w, "proof", err)
		return
	}
	if res.Proven && h.Saver != nil {
		if err := h.Saver.Record(r.Context(), tenant, rep.Agent, rep.Path, res.Version, rep.SHA256, "prove", by); err != nil {
			h.fail(w, "proof ledger", err)
			return
		}
	}
	writeJSONBody(w, http.StatusOK, map[string]any{"verdict": v, "proven": res.Proven, "status": res.Status})
}

// serveSkills answers GET /v1/skills?agent=&version= for the SDKs, with an
// ingest key: the approved skills shipped to that agent (only with the
// learning loop on and the agent opted in), with an ETag so an unchanged
// set costs one round trip and no body.
func (h *LearningHandler) serveSkills(w http.ResponseWriter, r *http.Request) {
	if h.IngestKeys == nil {
		jsonErr(w, http.StatusNotFound, "no such endpoint")
		return
	}
	tenant, ok := h.IngestKeys.Tenant(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="bindle"`)
		jsonErr(w, http.StatusUnauthorized, "missing or invalid ingest key")
		return
	}
	if h.Features == nil || !h.Features.Enabled(r.Context(), tenant, skills.Feature) {
		jsonErr(w, http.StatusNotFound, "no such endpoint")
		return
	}
	agent, err := vault.CleanAgent(r.URL.Query().Get("agent"))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	version := r.URL.Query().Get("version")
	if len(version) > 128 {
		jsonErr(w, http.StatusBadRequest, "version is too long")
		return
	}
	if h.Limiter != nil && !h.Limiter.Allow("skills:"+tenant, 1) {
		w.Header().Set("Retry-After", "5")
		jsonErr(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	rs, err := h.Ship.Skills(r.Context(), tenant, agent)
	if err != nil {
		h.Log.Error("learning: skills", "err", err)
		w.Header().Set("Retry-After", "5")
		jsonErr(w, http.StatusServiceUnavailable, "temporarily unavailable, retry")
		return
	}
	body, err := json.Marshal(map[string]any{"agent": agent, "version": version, "skills": skillship.Serve(rs, version)})
	if err != nil {
		h.fail(w, "skills", err)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
