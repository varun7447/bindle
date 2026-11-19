package ingest

import (
	"bytes"
	"compress/gzip"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/auth"
	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/normalize"
	"github.com/bindle-ai/bindle/backend/internal/otlp/otlptest"
	"github.com/bindle-ai/bindle/backend/internal/pipeline"
	"github.com/bindle-ai/bindle/backend/internal/ratelimit"
	"github.com/bindle-ai/bindle/backend/internal/redact"
)

type capture struct {
	mu    sync.Mutex
	steps []model.Step
	full  bool
}

func (c *capture) Submit(s []model.Step) error {
	if c.full {
		return pipeline.ErrBackpressure
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps = append(c.steps, s...)
	return nil
}

func newHandler(out Submitter, rate float64) *Handler {
	keys, _ := auth.ParseEnv("bd_test_good=acme")
	return &Handler{
		Keys:       keys,
		Normalizer: &normalize.Normalizer{Redactor: redact.Default()},
		Limiter:    ratelimit.New(rate, rate),
		Out:        out,
		Log:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}
}

func payload() []byte {
	return otlptest.Request(
		[]otlptest.KV{{Key: "tenant.id", Value: "evil"}},
		"bindle",
		otlptest.Span{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331", Name: "crm.lookup",
			Attrs: []otlptest.KV{{Key: "bindle.step.type", Value: "tool_call"}, {Key: "tenant_id", Value: "evil"}}},
	)
}

func post(h http.Handler, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAcceptsProtobufAndSetsTenantFromKey(t *testing.T) {
	c := &capture{}
	rec := post(newHandler(c, 0), payload(), map[string]string{"Authorization": "Bearer bd_test_good"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(c.steps) != 1 || c.steps[0].TenantID != "acme" || c.steps[0].Type != model.StepTool {
		t.Fatalf("steps: %+v", c.steps)
	}
}

func TestGzipAndHeaderKey(t *testing.T) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(payload())
	_ = w.Close()
	c := &capture{}
	rec := post(newHandler(c, 0), gz.Bytes(), map[string]string{"x-bindle-key": "bd_test_good", "Content-Encoding": "gzip"})
	if rec.Code != 200 || len(c.steps) != 1 {
		t.Fatalf("status %d steps %d", rec.Code, len(c.steps))
	}
}

func TestRejectsBadKey(t *testing.T) {
	rec := post(newHandler(&capture{}, 0), payload(), map[string]string{"Authorization": "Bearer nope"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestJSON(t *testing.T) {
	body := `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331","name":"x"}]}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer bd_test_good")
	rec := httptest.NewRecorder()
	c := &capture{}
	newHandler(c, 0).ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "{}" || len(c.steps) != 1 {
		t.Fatalf("status %d body %q steps %d", rec.Code, rec.Body, len(c.steps))
	}
}

func TestBackpressureReturns503(t *testing.T) {
	rec := post(newHandler(&capture{full: true}, 0), payload(), map[string]string{"Authorization": "Bearer bd_test_good"})
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("want 503 with Retry-After, got %d", rec.Code)
	}
}

func TestRateLimitReturns429(t *testing.T) {
	h := newHandler(&capture{}, 1)
	hdr := map[string]string{"Authorization": "Bearer bd_test_good"}
	if rec := post(h, payload(), hdr); rec.Code != 200 {
		t.Fatalf("first request: %d", rec.Code)
	}
	if rec := post(h, payload(), hdr); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: want 429, got %d", rec.Code)
	}
}

func TestBadPayload(t *testing.T) {
	rec := post(newHandler(&capture{}, 0), []byte{0x0a, 0xff}, map[string]string{"Authorization": "Bearer bd_test_good"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestLogsEndpointKeepsEvaluations(t *testing.T) {
	c := &capture{}
	h := newHandler(c, 0)
	h.Logs = true
	body := otlptest.LogsRequest(nil, "evals",
		otlptest.LogRecord{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331", Time: 1, EventName: "gen_ai.evaluation.result",
			Attrs: []otlptest.KV{{Key: "gen_ai.evaluation.name", Value: "Groundedness"}, {Key: "gen_ai.evaluation.score.value", Value: 0.9}}},
		otlptest.LogRecord{TraceID: "0af7651916cd43dd8448eb211c80319c", Time: 2, EventName: "app.log"})
	rec := post(h, body, map[string]string{"Authorization": "Bearer bd_test_good"})
	if rec.Code != 200 || len(c.steps) != 1 {
		t.Fatalf("%d %+v", rec.Code, c.steps)
	}
	st := c.steps[0]
	if st.TenantID != "acme" || st.Type != model.StepEval || st.ScoreName != "Groundedness" || *st.ScoreValue != 0.9 || st.ParentStepID != "b7ad6b7169203331" ||
		st.AgentVersion != "" {
		t.Errorf("step %+v", st)
	}
	if rec := post(h, body, nil); rec.Code != 401 {
		t.Errorf("logs need a key: %d", rec.Code)
	}
	// Span events on traces become eval steps too.
	tr := newHandler(c, 0)
	c.steps = nil
	span := otlptest.Span{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "00f067aa0ba902b7", Name: "chat", Start: 1, End: 2,
		Events: []otlptest.Event{{Name: "gen_ai.evaluation.result", Time: 2, Attrs: []otlptest.KV{{Key: "gen_ai.evaluation.name", Value: "Relevance"}, {Key: "gen_ai.evaluation.score.label", Value: "pass"}}}}}
	if rec := post(tr, otlptest.Request(nil, "x", span), map[string]string{"Authorization": "Bearer bd_test_good"}); rec.Code != 200 || len(c.steps) != 2 ||
		c.steps[1].ScorePass != "pass" || c.steps[1].ParentStepID != "00f067aa0ba902b7" {
		t.Errorf("span events: %d %+v", rec.Code, c.steps)
	}
}
