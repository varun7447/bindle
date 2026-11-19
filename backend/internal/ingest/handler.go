// Package ingest is the OTLP/HTTP edge: authenticate, decode, normalize,
// rate-limit and hand steps to the pipeline.
package ingest

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/bindle-ai/bindle/backend/internal/auth"
	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/normalize"
	"github.com/bindle-ai/bindle/backend/internal/otlp"
	"github.com/bindle-ai/bindle/backend/internal/pipeline"
	"github.com/bindle-ai/bindle/backend/internal/ratelimit"
	"github.com/bindle-ai/bindle/backend/internal/sdkconfig"
)

// Submitter accepts normalized steps (the batcher in v0, Kinesis in v0.2).
type Submitter interface {
	Submit(steps []model.Step) error
}

// PolicySource gives a tenant's remote SDK configuration.
// Implemented by control.ConfigResolver.
type PolicySource interface {
	Policy(ctx context.Context, tenant string) (sdkconfig.Policy, error)
}

// Handler serves POST /v1/traces.
type Handler struct {
	Keys         auth.Resolver
	Policies     PolicySource // nil: no remote policy (defaults)
	Normalizer   *normalize.Normalizer
	Limiter      *ratelimit.Limiter
	Out          Submitter
	MaxBodyBytes int64
	Log          *slog.Logger
	// Logs makes this the /v1/logs handler: only evaluation events
	// (gen_ai.evaluation.result) are kept, as eval steps of their runs.
	Logs bool
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenant, ok := h.Keys.Tenant(r)
	if !ok {
		http.Error(w, "invalid or missing API key", http.StatusUnauthorized)
		return
	}

	body, err := h.readBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	isJSON := isJSONContent(r.Header.Get("Content-Type"))
	var spans []model.Span
	switch {
	case h.Logs && isJSON:
		spans, err = otlp.DecodeLogsJSON(body)
	case h.Logs:
		spans, err = otlp.DecodeLogsProto(body)
	case isJSON:
		spans, err = otlp.DecodeJSON(body)
	default:
		spans, err = otlp.DecodeProto(body)
	}
	if err != nil {
		http.Error(w, "could not decode OTLP payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	if !h.Limiter.Allow(tenant, len(spans)) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	steps := make([]model.Step, 0, len(spans))
	for _, s := range spans {
		if normalize.IsHealthCheck(s) {
			continue // load balancer and platform probes, not agent work
		}
		steps = append(steps, h.Normalizer.Normalize(tenant, s))
		for _, ev := range normalize.EvaluationSpans(s) {
			steps = append(steps, h.Normalizer.Normalize(tenant, ev))
		}
	}
	if h.Policies != nil {
		pol, err := h.Policies.Policy(r.Context(), tenant)
		if err != nil {
			// Unknown policy: don't guess (capture may be off). Exporters retry.
			h.Log.Error("policy lookup failed", "tenant", tenant, "err", err)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "temporarily unavailable, retry", http.StatusServiceUnavailable)
			return
		}
		steps = ApplyPolicy(pol, steps)
	}
	if err := h.Out.Submit(steps); err != nil {
		if errors.Is(err, pipeline.ErrBackpressure) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "ingest busy, retry", http.StatusServiceUnavailable)
			return
		}
		h.Log.Error("submit failed", "tenant", tenant, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// An empty Export*ServiceResponse means full success.
	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	limit := h.MaxBodyBytes
	if limit <= 0 {
		limit = 8 << 20
	}
	var src io.Reader = http.MaxBytesReader(w, r.Body, limit)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(src)
		if err != nil {
			return nil, errors.New("bad gzip body")
		}
		defer gz.Close()
		// cap the decompressed size too (zip-bomb protection)
		src = io.LimitReader(gz, limit*4+1)
		b, err := io.ReadAll(src)
		if err != nil {
			return nil, errors.New("could not read body")
		}
		if int64(len(b)) > limit*4 {
			return nil, errors.New("decompressed body too large")
		}
		return b, nil
	}
	b, err := io.ReadAll(src)
	if err != nil {
		return nil, errors.New("body too large or unreadable")
	}
	return b, nil
}

func isJSONContent(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "application/json"
}

// ApplyPolicy enforces a tenant's remote settings on normalized steps, so
// they hold even for clients the SDK does not control: traces outside the
// sample are dropped (same rule as the SDK, so whole traces), content is
// removed when capture is off, and extra patterns are applied on top of the
// built-in redaction the normalizer already ran.
// ContentDroppedAttr records who dropped a step's content: "workspace" (the
// console's SDK settings, applied here) or "sdk" (the agent's own setting,
// set by the Bindle SDKs).
const ContentDroppedAttr = "bindle.content_dropped"

// PriceSourceAttr marks steps priced with the workspace's own model price
// instead of the list price.
const PriceSourceAttr = "bindle.price_source"

func ApplyPolicy(p sdkconfig.Policy, steps []model.Step) []model.Step {
	c := p.Config
	out := steps[:0]
	for _, s := range steps {
		if !sdkconfig.Sampled(s.TraceID, c.SampleRate) {
			continue
		}
		if p.Prices != nil && s.Type == model.StepLLM {
			if _, ok := p.Prices.Lookup(s.Provider, s.Model); ok {
				// The workspace's own price wins over list prices.
				s.CostUSD, s.CostKnown = p.Prices.Compute(s.Provider, s.Model, s.InputTokens, s.OutputTokens, s.CachedTokens)
				attrs := make(map[string]string, len(s.Attributes)+1)
				for k, v := range s.Attributes {
					attrs[k] = v
				}
				attrs[PriceSourceAttr] = "workspace"
				s.Attributes = attrs
			}
		}
		if !c.CaptureContent {
			s.Input, s.Output, s.Reasoning, s.UserInput = "", "", "", ""
			// Say why, so the console can explain the empty fields.
			attrs := make(map[string]string, len(s.Attributes)+1)
			for k, v := range s.Attributes {
				attrs[k] = v
			}
			attrs[ContentDroppedAttr] = "workspace"
			s.Attributes = attrs
		}
		if !p.Redactor.Empty() {
			s.Name = p.Redactor.Apply(s.Name)
			s.Input, s.Output = p.Redactor.Apply(s.Input), p.Redactor.Apply(s.Output)
			s.Reasoning, s.ErrorMessage = p.Redactor.Apply(s.Reasoning), p.Redactor.Apply(s.ErrorMessage)
			s.UserInput = p.Redactor.Apply(s.UserInput)
			if len(s.Attributes) > 0 {
				attrs := make(map[string]string, len(s.Attributes))
				for k, v := range s.Attributes {
					attrs[k] = p.Redactor.Apply(v)
				}
				s.Attributes = attrs
			}
		}
		out = append(out, s)
	}
	return out
}

// ConfigHandler serves GET /v1/config: the tenant's SDK settings, for the
// SDK to poll with its ingest key. Supports If-None-Match.
type ConfigHandler struct {
	Keys     auth.Resolver
	Policies PolicySource
	Limiter  *ratelimit.Limiter
	Log      *slog.Logger
}

func (h *ConfigHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenant, ok := h.Keys.Tenant(r)
	if !ok {
		http.Error(w, "invalid or missing API key", http.StatusUnauthorized)
		return
	}
	if !h.Limiter.Allow(tenant, 1) {
		w.Header().Set("Retry-After", "10")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	cfg := sdkconfig.Default()
	if h.Policies != nil {
		pol, err := h.Policies.Policy(r.Context(), tenant)
		if err != nil {
			h.Log.Error("config lookup failed", "tenant", tenant, "err", err)
			w.Header().Set("Retry-After", "30")
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		cfg = pol.Config
	}
	etag := `"v` + strconv.FormatInt(cfg.Version, 10)
	if len(cfg.OverBudget) > 0 {
		// Budget state changes without a config version bump.
		sum := sha256.Sum256([]byte(strings.Join(cfg.OverBudget, "\x00")))
		etag += "-b" + hex.EncodeToString(sum[:4])
	}
	if len(cfg.StoppedRuns) > 0 {
		// So do stopped runs.
		sum := sha256.Sum256([]byte(strings.Join(cfg.StoppedRuns, "\x00")))
		etag += "-s" + hex.EncodeToString(sum[:4])
	}
	etag += `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if cfg.RedactPatterns == nil {
		cfg.RedactPatterns = []sdkconfig.Pattern{}
	}
	if cfg.Limits == nil {
		cfg.Limits = []sdkconfig.Limit{}
	}
	if cfg.OverBudget == nil {
		cfg.OverBudget = []string{}
	}
	if cfg.StoppedRuns == nil {
		cfg.StoppedRuns = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		sdkconfig.Config
		PollIntervalSeconds int `json:"poll_interval_seconds"`
	}{cfg, sdkconfig.PollSeconds})
}
