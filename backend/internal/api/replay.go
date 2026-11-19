package api

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/bindle-ai/bindle/backend/internal/ratelimit"
	"github.com/bindle-ai/bindle/backend/internal/replay"
	"github.com/bindle-ai/bindle/backend/internal/sdkconfig"
)

const maxReplayBodyBytes = 512 << 10

var (
	defaultReplayLimiter     *ratelimit.Limiter
	defaultReplayLimiterOnce sync.Once
)

func (s *Server) replayLimiter() *ratelimit.Limiter {
	if s.ReplayLimiter != nil {
		return s.ReplayLimiter
	}
	defaultReplayLimiterOnce.Do(func() { defaultReplayLimiter = ratelimit.New(10.0/60, 5) })
	return defaultReplayLimiter
}

// replayStep runs one captured LLM step again with the caller's edits and
// their own provider key. Nothing about the replay is stored: the key is
// used for this request only, and the result is returned, not saved.
func (s *Server) replayStep(w http.ResponseWriter, r *http.Request) {
	if s.Replay == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "replay is not configured")
		return
	}
	traceID, stepID := strings.ToLower(r.PathValue("id")), strings.ToLower(r.PathValue("step"))
	if !traceIDRe.MatchString(traceID) || !stepIDRe.MatchString(stepID) {
		badRequest(w, "invalid trace or step ID")
		return
	}
	tenant := tenantFrom(r.Context())
	if !s.replayLimiter().Allow(tenant, 1) {
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many replays; try again in a minute")
		return
	}
	var b struct {
		Provider    string           `json:"provider"`
		Model       string           `json:"model"`
		APIKey      string           `json:"api_key"`
		Messages    []replay.Message `json:"messages"`
		MaxTokens   int              `json:"max_tokens"`
		Temperature *float64         `json:"temperature"`
	}
	if !decodeBodyLimit(w, r, &b, maxReplayBodyBytes) {
		return
	}
	req := replay.Request{Provider: b.Provider, Model: strings.TrimSpace(b.Model), APIKey: strings.TrimSpace(b.APIKey), Messages: b.Messages, MaxTokens: b.MaxTokens, Temperature: b.Temperature}
	b.APIKey = ""
	if err := replay.Validate(req); err != nil {
		badRequest(w, err.Error())
		return
	}
	orig, err := s.Store.GetStep(r.Context(), tenant, traceID, stepID)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if orig == nil {
		writeError(w, http.StatusNotFound, "not_found", "step not found")
		return
	}
	if orig.Type != "llm_call" {
		badRequest(w, "only LLM steps can be replayed")
		return
	}
	res, err := s.Replay.Run(r.Context(), req)
	var pe *replay.ProviderError
	switch {
	case errors.Is(err, replay.ErrUnsupported):
		badRequest(w, "replay with "+req.Provider+" isn't available in this build")
		return
	case errors.As(err, &pe):
		writeError(w, http.StatusBadGateway, "provider_error", pe.Error())
		return
	case err != nil && (strings.HasPrefix(err.Error(), "the conversation") || strings.HasPrefix(err.Error(), "add at least")):
		badRequest(w, err.Error())
		return
	case err != nil:
		s.logger().Warn("replay failed", "tenant", tenant, "provider", req.Provider) // never the error text: it may quote the request
		writeError(w, http.StatusBadGateway, "provider_error", "the provider could not be reached")
		return
	}
	costUSD, known := s.replayCost(r, req.Provider, res)
	writeJSON(w, http.StatusOK, map[string]any{
		"result": res, "cost_usd": costUSD, "cost_known": known,
		"original": map[string]any{
			"model": orig.Model, "output": orig.Output, "input_tokens": orig.InputTokens, "output_tokens": orig.OutputTokens,
			"cached_tokens": orig.CachedTokens, "cost_usd": orig.CostUSD, "cost_known": orig.CostKnown, "duration_ms": orig.DurationMs,
		},
	})
}

// replayCost prices a replay like ingest would: workspace prices first, then
// list prices; unknown when neither covers the model.
func (s *Server) replayCost(r *http.Request, provider string, res replay.Result) (float64, bool) {
	if s.Config != nil {
		if c, err := s.Config.GetSDKConfig(r.Context(), tenantFrom(r.Context())); err == nil {
			if t := sdkconfig.PriceTable(c.Prices); t != nil {
				if _, ok := t.Lookup(provider, res.Model); ok {
					return t.Compute(provider, res.Model, res.InputTokens, res.OutputTokens, res.CachedTokens)
				}
			}
		}
	}
	return s.Prices.Compute(provider, res.Model, res.InputTokens, res.OutputTokens, res.CachedTokens)
}
