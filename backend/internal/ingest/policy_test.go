package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/auth"
	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/ratelimit"
	"github.com/bindle-ai/bindle/backend/internal/sdkconfig"
)

type fixedPolicy struct {
	p       sdkconfig.Policy
	err     error
	tenants []string
}

func (f *fixedPolicy) Policy(_ context.Context, tenant string) (sdkconfig.Policy, error) {
	f.tenants = append(f.tenants, tenant)
	return f.p, f.err
}

func policy(rate float64, capture bool, patterns ...sdkconfig.Pattern) sdkconfig.Policy {
	return sdkconfig.Policy{
		Config:   sdkconfig.Config{Version: 7, SampleRate: rate, CaptureContent: capture, RedactPatterns: patterns},
		Redactor: sdkconfig.NewRedactor(patterns),
	}
}

func TestApplyPolicy(t *testing.T) {
	keep := "00000000000000000000000000000001" // low bits ~0: kept at any rate > 0
	drop := "0000000000000000ffffffffffffffff" // dropped below rate 1
	steps := []model.Step{
		{TraceID: keep, Input: "order ord_123", Output: "ok", Reasoning: "r", ErrorMessage: "failed ord_9",
			Name: "lookup ord_1", Attributes: map[string]string{"q": "ord_77"}},
		{TraceID: drop, Input: "x"},
	}
	out := ApplyPolicy(policy(0.5, true, sdkconfig.Pattern{Name: "ORDER", Pattern: `ord_\d+`}), append([]model.Step(nil), steps...))
	if len(out) != 1 || out[0].TraceID != keep {
		t.Fatalf("sampling: %+v", out)
	}
	s := out[0]
	if s.Input != "order [REDACTED:ORDER]" || s.ErrorMessage != "failed [REDACTED:ORDER]" || s.Name != "lookup [REDACTED:ORDER]" ||
		s.Attributes["q"] != "[REDACTED:ORDER]" || s.Output != "ok" {
		t.Errorf("redaction: %+v", s)
	}
	if steps[0].Attributes["q"] != "ord_77" {
		t.Error("caller's attribute map was mutated")
	}

	out = ApplyPolicy(policy(1, false), append([]model.Step(nil), steps...))
	if len(out) != 2 || out[0].Input != "" || out[0].Output != "" || out[0].Reasoning != "" || out[0].ErrorMessage == "" {
		t.Errorf("capture off must drop content but keep errors: %+v", out[0])
	}
	if out[0].Attributes[ContentDroppedAttr] != "workspace" || out[0].Attributes["q"] != "ord_77" {
		t.Errorf("capture off must say why and keep other attributes: %v", out[0].Attributes)
	}
	if _, ok := steps[0].Attributes[ContentDroppedAttr]; ok {
		t.Error("caller's attribute map was mutated")
	}
	if out := ApplyPolicy(policy(0, true), append([]model.Step(nil), steps...)); len(out) != 0 {
		t.Errorf("rate 0 kept %d", len(out))
	}
}

func TestHandlerEnforcesPolicyAndFailsSafe(t *testing.T) {
	c := &capture{}
	h := newHandler(c, 1000)
	pol := &fixedPolicy{p: policy(1, false)}
	h.Policies = pol
	if w := post(h, payload(), map[string]string{"Authorization": "Bearer bd_test_good"}); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if len(c.steps) != 1 || c.steps[0].Input != "" || len(pol.tenants) != 1 || pol.tenants[0] != "acme" {
		t.Fatalf("policy not applied for the key's tenant: %+v %v", c.steps, pol.tenants)
	}
	pol.err = errors.New("db down")
	w := post(h, payload(), map[string]string{"Authorization": "Bearer bd_test_good"})
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" || len(c.steps) != 1 {
		t.Fatalf("unknown policy must 503 and store nothing: %d, %d steps", w.Code, len(c.steps))
	}
}

func configServer(p PolicySource) *ConfigHandler {
	keys, _ := auth.ParseEnv("bd_test_good=acme")
	return &ConfigHandler{Keys: keys, Policies: p, Limiter: ratelimit.New(100, 100), Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
}

func getConfig(h http.Handler, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestConfigEndpoint(t *testing.T) {
	pol := &fixedPolicy{p: policy(0.25, false, sdkconfig.Pattern{Name: "ORDER", Pattern: `ord_\d+`})}
	h := configServer(pol)
	auth := map[string]string{"Authorization": "Bearer bd_test_good"}

	if w := getConfig(h, nil); w.Code != 401 {
		t.Fatalf("no key: %d", w.Code)
	}
	if w := getConfig(h, map[string]string{"Authorization": "Bearer nope"}); w.Code != 401 {
		t.Fatalf("bad key: %d", w.Code)
	}
	w := getConfig(h, auth)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 200 || w.Header().Get("ETag") != `"v7"` || body["sample_rate"] != 0.25 || body["capture_content"] != false ||
		body["poll_interval_seconds"] != float64(sdkconfig.PollSeconds) || len(body["redact_patterns"].([]any)) != 1 {
		t.Fatalf("%d %v %v", w.Code, w.Header(), body)
	}
	if w := getConfig(h, map[string]string{"Authorization": "Bearer bd_test_good", "If-None-Match": `"v7"`}); w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("If-None-Match: %d", w.Code)
	}
	if w := getConfig(h, map[string]string{"Authorization": "Bearer bd_test_good", "If-None-Match": `"v6"`}); w.Code != 200 {
		t.Fatalf("stale ETag: %d", w.Code)
	}
	pol.err = errors.New("db down")
	if w := getConfig(h, auth); w.Code != 503 {
		t.Fatalf("outage: %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/config", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != 405 {
		t.Fatalf("POST: %d", rw.Code)
	}
	// Without a policy source the defaults are served.
	w = getConfig(configServer(nil), auth)
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 200 || body["sample_rate"] != float64(1) || body["capture_content"] != true {
		t.Fatalf("defaults: %v", body)
	}
}

func TestConfigEndpointStoppedRuns(t *testing.T) {
	p := policy(1, true)
	auth := map[string]string{"Authorization": "Bearer bd_test_good"}
	w := getConfig(configServer(&fixedPolicy{p: p}), auth)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if runs, ok := body["stopped_runs"].([]any); !ok || len(runs) != 0 {
		t.Fatalf("stopped_runs should be an empty list: %v", body["stopped_runs"])
	}
	plain := w.Header().Get("ETag")

	p.Config.StoppedRuns = []string{"0af7651916cd43dd8448eb211c80319c"}
	w = getConfig(configServer(&fixedPolicy{p: p}), map[string]string{"Authorization": "Bearer bd_test_good", "If-None-Match": plain})
	if w.Code != 200 {
		t.Fatalf("a new stop must change the ETag: %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if runs := body["stopped_runs"].([]any); len(runs) != 1 || runs[0] != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("stopped_runs: %v", body["stopped_runs"])
	}
}

func TestApplyPolicyWorkspacePrices(t *testing.T) {
	p := policy(1, true)
	p.Prices = sdkconfig.PriceTable([]sdkconfig.PriceOverride{{Model: "gpt-x", InputPerMTok: 1, OutputPerMTok: 2}})
	steps := []model.Step{
		{TraceID: strings.Repeat("0", 32), Type: model.StepLLM, Provider: "openai", Model: "gpt-x", InputTokens: 1_000_000, OutputTokens: 500_000, CostUSD: 99, CostKnown: true},
		{TraceID: strings.Repeat("0", 32), Type: model.StepLLM, Provider: "openai", Model: "other", CostUSD: 5, CostKnown: true},
		{TraceID: strings.Repeat("0", 32), Type: model.StepLLM, Provider: "openai", Model: "gpt-x", InputTokens: 10, CachedTokens: 5, CostUSD: 1, CostKnown: true},
	}
	out := ApplyPolicy(p, steps)
	if out[0].CostUSD != 2 || !out[0].CostKnown || out[0].Attributes[PriceSourceAttr] != "workspace" {
		t.Errorf("override: %+v", out[0])
	}
	if out[1].CostUSD != 5 || out[1].Attributes[PriceSourceAttr] != "" {
		t.Errorf("list price kept for models without an override: %+v", out[1])
	}
	if out[2].CostKnown {
		t.Errorf("override without a cached price must make cached usage unknown, not list-priced: %+v", out[2])
	}
}
