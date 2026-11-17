// Package ingest is the OTLP/HTTP edge: authenticate, decode, normalize,
// rate-limit and hand steps to the pipeline.
package ingest

import (
	"compress/gzip"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/bindle-ai/bindle/services/internal/auth"
	"github.com/bindle-ai/bindle/services/internal/model"
	"github.com/bindle-ai/bindle/services/internal/normalize"
	"github.com/bindle-ai/bindle/services/internal/otlp"
	"github.com/bindle-ai/bindle/services/internal/pipeline"
	"github.com/bindle-ai/bindle/services/internal/ratelimit"
)

// Submitter accepts normalized steps (the batcher in v0, Kinesis in v0.2).
type Submitter interface {
	Submit(steps []model.Step) error
}

// Handler serves POST /v1/traces.
type Handler struct {
	Keys         auth.Resolver
	Normalizer   *normalize.Normalizer
	Limiter      *ratelimit.Limiter
	Out          Submitter
	MaxBodyBytes int64
	Log          *slog.Logger
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
	if isJSON {
		spans, err = otlp.DecodeJSON(body)
	} else {
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
		steps = append(steps, h.Normalizer.Normalize(tenant, s))
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

	// An empty ExportTraceServiceResponse means full success.
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
