// Package api serves the console's read API over the query Store.
//
// Every /api route requires a session; the tenant comes only from the
// session resolver and is passed explicitly to the store. Client-supplied
// tenant hints (query params, headers) are ignored.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/alerts"
	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/cost"
	"github.com/bindle-ai/bindle/backend/internal/query"
	"github.com/bindle-ai/bindle/backend/internal/ratelimit"
	"github.com/bindle-ai/bindle/backend/internal/replay"
	"github.com/bindle-ai/bindle/backend/internal/sdkconfig"
	"github.com/bindle-ai/bindle/backend/internal/session"
)

// KeyStore manages a tenant's ingest keys. Implemented by *control.Store.
type KeyStore interface {
	ListKeys(ctx context.Context, tenant string) ([]control.APIKey, error)
	CreateKey(ctx context.Context, tenant, userID, name string) (control.APIKey, string, error)
	RotateKey(ctx context.Context, tenant, userID, keyID string, grace time.Duration) (control.APIKey, string, error)
	RevokeKey(ctx context.Context, tenant, keyID string) error
}

// ConfigStore reads and saves a tenant's remote SDK config.
// Implemented by *control.Store.
type ConfigStore interface {
	GetSDKConfig(ctx context.Context, tenant string) (sdkconfig.Config, error)
	PutSDKConfig(ctx context.Context, tenant, userID string, c sdkconfig.Config) (sdkconfig.Config, error)
	PutLimits(ctx context.Context, tenant, userID string, version int64, limits []sdkconfig.Limit) (sdkconfig.Config, error)
	PutPrices(ctx context.Context, tenant, userID string, version int64, prices []sdkconfig.PriceOverride) (sdkconfig.Config, error)
}

// Server holds the API's dependencies.
type Server struct {
	Store    query.Store
	Keys     KeyStore      // nil disables the /api/keys endpoints (503)
	Config   ConfigStore   // nil disables the /api/config endpoints (503)
	Alerting AlertingStore // nil disables the alert rule/channel endpoints (503)
	Evals    EvalStore     // nil disables the checks and datasets endpoints (503)
	Stops    RunStopStore  // nil disables stopping runs (503)
	// Features lists the workspace's operator-set feature flags; nil means
	// every flagged feature is off.
	Features FeatureLister
	Ledger   LedgerReader   // the ledger (feature "ledger"); nil hides it (404)
	Replay   *replay.Runner // nil disables step replay (503)
	Prices   *cost.Table    // list prices, for replay cost
	// AWSPrices serves Bedrock list prices; nil disables (503).
	AWSPrices *AWSPriceList
	// ReplayLimiter caps replays per tenant; nil uses 10 a minute.
	ReplayLimiter *ratelimit.Limiter
	Deliver       *alerts.Deliverer // test sends; nil disables them
	Sessions      session.Resolver
	Limiter       *ratelimit.Limiter // per-tenant requests/sec; nil disables
	Log           *slog.Logger
	Now           func() time.Time

	MaxSteps       int // waterfall step cap; default 5000
	LoopMinRepeats int // runaway-loop threshold; default 5
}

const (
	defaultLimit = 50
	maxLimit     = 200
	maxRange     = 30 * 24 * time.Hour
	maxFilterLen = 256
)

var (
	traceIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	stepIDRe  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// Handler returns the HTTP handler: /healthz (no auth) and /api/* (auth).
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/runs", s.listRuns)
	api.HandleFunc("GET /api/traces/{id}", s.getTrace)
	api.HandleFunc("GET /api/traces/{id}/steps/{step}", s.getStep)
	api.HandleFunc("GET /api/traces/{id}/links", s.traceLinks)
	api.HandleFunc("GET /api/overview", s.overview)
	api.HandleFunc("GET /api/alerts", s.alerts)
	api.HandleFunc("GET /api/insights", s.insights)
	api.HandleFunc("GET /api/me", s.me)
	api.HandleFunc("GET /api/keys", s.listKeys)
	api.HandleFunc("POST /api/keys", s.createKey)
	api.HandleFunc("POST /api/keys/{id}/rotate", s.rotateKey)
	api.HandleFunc("POST /api/keys/{id}/revoke", s.revokeKey)
	api.HandleFunc("GET /api/config", s.getConfig)
	api.HandleFunc("PUT /api/config", s.putConfig)
	api.HandleFunc("GET /api/limits", s.getLimits)
	api.HandleFunc("PUT /api/limits", s.putLimits)
	api.HandleFunc("GET /api/prices", s.getPrices)
	api.HandleFunc("GET /api/prices/aws", s.awsPrices)
	api.HandleFunc("PUT /api/prices", s.putPrices)
	api.HandleFunc("GET /api/models", s.models)
	api.HandleFunc("GET /api/tools", s.tools)
	api.HandleFunc("GET /api/sessions", s.listSessions)
	api.HandleFunc("GET /api/search", s.search)
	api.HandleFunc("GET /api/sessions/{id}", s.getSession)
	api.HandleFunc("GET /api/agents/versions", s.agentVersions)
	api.HandleFunc("GET /api/features", s.features)
	api.HandleFunc("GET /api/agents/ledger", s.ledgerList)
	api.HandleFunc("GET /api/agents/ledger/verify", s.ledgerVerify)
	api.HandleFunc("GET /api/agents/ledger/summary", s.ledgerSummary)
	api.HandleFunc("GET /api/agents/tool-usage", s.toolUsage)
	api.HandleFunc("GET /api/economics", s.economics)
	api.HandleFunc("GET /api/agents/topics", s.agentTopics)
	api.HandleFunc("GET /api/security", s.securitySignals)
	s.evalRoutes(api)
	api.HandleFunc("POST /api/traces/{id}/steps/{step}/replay", s.replayStep)
	api.HandleFunc("GET /api/traces/{id}/stop", s.getRunStop)
	api.HandleFunc("POST /api/traces/{id}/stop", s.stopRun)
	api.HandleFunc("GET /api/traces/{id}/cassette", s.getCassette)
	api.HandleFunc("GET /api/alert-rules", s.listAlertRules)
	api.HandleFunc("POST /api/alert-rules", s.createAlertRule)
	api.HandleFunc("PUT /api/alert-rules/{id}", s.updateAlertRule)
	api.HandleFunc("GET /api/alert-channels", s.listAlertChannels)
	api.HandleFunc("POST /api/alert-channels", s.createAlertChannel)
	api.HandleFunc("PUT /api/alert-channels/{id}/state", s.setAlertChannelState)
	api.HandleFunc("DELETE /api/alert-channels/{id}", s.deleteAlertChannel)
	api.HandleFunc("POST /api/alert-channels/{id}/test", s.testAlertChannel)
	api.HandleFunc("GET /api/alert-events", s.listAlertEvents)
	api.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	root.Handle("/api/", s.authed(api))
	return s.recoverer(securityHeaders(root))
}

// ---- middleware ----------------------------------------------------------

type ctxKey struct{}

func principalFrom(ctx context.Context) session.Principal {
	p, _ := ctx.Value(ctxKey{}).(session.Principal)
	return p
}

func tenantFrom(ctx context.Context) string { return principalFrom(ctx).TenantID }

func (s *Server) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Sessions.Resolve(r)
		if err != nil || p.TenantID == "" {
			if err != nil && !errors.Is(err, session.ErrUnauthenticated) {
				s.logger().Error("session", "err", err) // control plane failure, not a bad token
				writeError(w, http.StatusServiceUnavailable, "unavailable", "sign-in is temporarily unavailable")
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="bindle"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "a valid session is required")
			return
		}
		if !s.Limiter.Allow(p.TenantID, 1) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.logger().Error("panic", "path", r.URL.Path, "panic", fmt.Sprint(v))
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---- helpers -------------------------------------------------------------

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var b errorBody
	b.Error.Code, b.Error.Message = code, msg
	writeJSON(w, status, b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func badRequest(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusBadRequest, "bad_request", msg)
}

// storeError logs the real error and returns a generic 500: database
// messages can contain SQL and internal names.
func (s *Server) storeError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger().Error("store", "path", r.URL.Path, "tenant", tenantFrom(r.Context()), "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

func parseTime(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, errors.New("must be an RFC 3339 timestamp")
	}
	return t.UTC(), nil
}

// ---- cursor --------------------------------------------------------------

// Cursors are opaque to clients but not secret: they only position a keyset
// scan inside the caller's own tenant, so tampering cannot widen access.
type cursor struct {
	Started int64  `json:"s"` // unix nanos
	TraceID string `json:"t"`
}

func encodeCursor(started time.Time, traceID string) string {
	b, _ := json.Marshal(cursor{Started: started.UnixNano(), TraceID: traceID})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(v string) (cursor, error) {
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || !traceIDRe.MatchString(c.TraceID) || c.Started <= 0 {
		return cursor{}, errors.New("invalid cursor")
	}
	return c, nil
}

// ---- response shapes -----------------------------------------------------

// RunJSON is a run in API responses.
type RunJSON struct {
	TraceID          string    `json:"trace_id"`
	StartedAt        time.Time `json:"started_at"`
	DurationMs       float64   `json:"duration_ms"`
	Status           string    `json:"status"`      // ok | failed | unknown
	RootStatus       string    `json:"root_status"` // root step status, "" if no root seen
	Steps            uint64    `json:"steps"`
	FailedSteps      uint64    `json:"failed_steps"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	CostPartial      bool      `json:"cost_partial"` // some model steps have no known price
	CostUnknownSteps uint64    `json:"cost_unknown_steps"`
	Agent            string    `json:"agent"`
	AgentVersion     string    `json:"agent_version"`
	Framework        string    `json:"framework"`
	Environment      string    `json:"environment"`
	SessionID        string    `json:"session_id"`
	CachedTokens     int64     `json:"cached_tokens"`
	LLMCalls         int64     `json:"llm_calls"`
	ToolCalls        int64     `json:"tool_calls"`
	PeakInputTokens  int64     `json:"peak_input_tokens"`
}

func runJSON(r query.Run) RunJSON {
	status := "ok"
	switch {
	case r.RootStatus == "failure" || r.FailedSteps > 0:
		status = "failed"
	case r.RootStatus == "":
		status = "unknown"
	}
	return RunJSON{
		TraceID: r.TraceID, StartedAt: r.Started.Time, DurationMs: r.DurationMs,
		Status: status, RootStatus: r.RootStatus, Steps: r.Steps, FailedSteps: r.FailedSteps,
		InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CostUSD: r.CostUSD,
		CostPartial: r.CostUnknownSteps > 0, CostUnknownSteps: r.CostUnknownSteps,
		Agent: r.Agent, AgentVersion: r.AgentVersion, Framework: r.Framework,
		Environment: r.Environment, SessionID: r.SessionID,
		CachedTokens: r.CachedTokens, LLMCalls: r.LLMCalls, ToolCalls: r.ToolCalls, PeakInputTokens: r.PeakInputTokens,
	}
}

// ---- handlers ------------------------------------------------------------

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := query.RunFilter{To: s.now(), Limit: defaultLimit}
	var err error
	if v := q.Get("to"); v != "" {
		if f.To, err = parseTime(v); err != nil {
			badRequest(w, "to "+err.Error())
			return
		}
	}
	f.From = f.To.Add(-24 * time.Hour)
	if v := q.Get("from"); v != "" {
		if f.From, err = parseTime(v); err != nil {
			badRequest(w, "from "+err.Error())
			return
		}
	}
	if !f.From.Before(f.To) {
		badRequest(w, "from must be before to")
		return
	}
	if f.To.Sub(f.From) > maxRange {
		badRequest(w, "time range is limited to 30 days")
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			badRequest(w, "limit must be a positive integer")
			return
		}
		f.Limit = min(n, maxLimit)
	}
	f.Agent, f.Environment, f.Version = q.Get("agent"), q.Get("environment"), q.Get("version")
	if len(f.Agent) > maxFilterLen || len(f.Environment) > maxFilterLen || len(f.Version) > maxFilterLen {
		badRequest(w, "filter value too long")
		return
	}
	switch f.Status = q.Get("status"); f.Status {
	case "", "failed", "ok":
	default:
		badRequest(w, "status must be failed or ok")
		return
	}
	if v := q.Get("cursor"); v != "" {
		c, err := decodeCursor(v)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		f.AfterStart, f.AfterTrace = time.Unix(0, c.Started).UTC(), c.TraceID
	}

	limit := f.Limit
	f.Limit = limit + 1 // one extra row tells us whether there is a next page
	runs, err := s.Store.ListRuns(r.Context(), tenantFrom(r.Context()), f)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	resp := struct {
		Runs       []RunJSON `json:"runs"`
		NextCursor string    `json:"next_cursor,omitempty"`
	}{Runs: make([]RunJSON, 0, min(len(runs), limit))}
	if len(runs) > limit {
		runs = runs[:limit]
		last := runs[limit-1]
		resp.NextCursor = encodeCursor(last.Started.Time, last.TraceID)
	}
	for _, run := range runs {
		resp.Runs = append(resp.Runs, runJSON(run))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getTrace(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	if !traceIDRe.MatchString(id) {
		badRequest(w, "trace id must be 32 hex characters")
		return
	}
	maxSteps := s.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 5000
	}
	tr, err := s.Store.GetTrace(r.Context(), tenantFrom(r.Context()), id, maxSteps)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if tr == nil {
		writeError(w, http.StatusNotFound, "not_found", "trace not found")
		return
	}
	steps := tr.Steps
	if steps == nil {
		steps = []query.StepSummary{}
	}
	writeJSON(w, http.StatusOK, struct {
		Trace     RunJSON             `json:"trace"`
		Steps     []query.StepSummary `json:"steps"`
		Truncated bool                `json:"truncated"`
	}{runJSON(tr.Run), steps, tr.Truncated})
}

func (s *Server) getStep(w http.ResponseWriter, r *http.Request) {
	id, step := strings.ToLower(r.PathValue("id")), strings.ToLower(r.PathValue("step"))
	if !traceIDRe.MatchString(id) || !stepIDRe.MatchString(step) {
		badRequest(w, "trace id must be 32 and step id 16 hex characters")
		return
	}
	sd, err := s.Store.GetStep(r.Context(), tenantFrom(r.Context()), id, step)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if sd == nil {
		writeError(w, http.StatusNotFound, "not_found", "step not found")
		return
	}
	if sd.Attributes == nil {
		sd.Attributes = map[string]string{}
	}
	if sd.Links == nil {
		sd.Links = []string{}
	}
	if sd.RedactedKinds == nil {
		sd.RedactedKinds = []string{}
	}
	writeJSON(w, http.StatusOK, struct {
		Step *query.StepDetail `json:"step"`
	}{sd})
}

var overviewWindows = map[string]struct{ window, bucket time.Duration }{
	"24h": {24 * time.Hour, time.Hour},
	"7d":  {7 * 24 * time.Hour, 6 * time.Hour},
	"30d": {30 * 24 * time.Hour, 24 * time.Hour},
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("window")
	if name == "" {
		name = "24h"
	}
	win, ok := overviewWindows[name]
	if !ok {
		badRequest(w, "window must be 24h, 7d or 30d")
		return
	}
	to := s.now()
	from := to.Add(-win.window)
	o, err := s.Store.Overview(r.Context(), tenantFrom(r.Context()), from, to, win.bucket)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	ratio := func(a, b uint64) float64 {
		if b == 0 {
			return 0
		}
		return float64(a) / float64(b)
	}
	series, tools := o.Series, o.TopFailingTools
	if series == nil {
		series = []query.SeriesPoint{}
	}
	if tools == nil {
		tools = []query.ToolFailure{}
	}
	writeJSON(w, http.StatusOK, struct {
		Window           string              `json:"window"`
		From             time.Time           `json:"from"`
		To               time.Time           `json:"to"`
		BucketSeconds    int                 `json:"bucket_seconds"`
		Runs             uint64              `json:"runs"`
		FailedRuns       uint64              `json:"failed_runs"`
		RunFailureRate   float64             `json:"run_failure_rate"`
		StepFailureRate  float64             `json:"step_failure_rate"`
		P50RunMs         float64             `json:"p50_run_ms"`
		P95RunMs         float64             `json:"p95_run_ms"`
		CostUSD          float64             `json:"cost_usd"`
		CostPartial      bool                `json:"cost_partial"`
		CostUnknownSteps uint64              `json:"cost_unknown_steps"`
		InputTokens      int64               `json:"input_tokens"`
		OutputTokens     int64               `json:"output_tokens"`
		Series           []query.SeriesPoint `json:"series"`
		TopFailingTools  []query.ToolFailure `json:"top_failing_tools"`
	}{
		Window: name, From: from, To: to, BucketSeconds: int(win.bucket / time.Second),
		Runs: o.Runs, FailedRuns: o.FailedRuns,
		RunFailureRate: ratio(o.FailedRuns, o.Runs), StepFailureRate: ratio(o.FailedSteps, o.Steps),
		P50RunMs: o.P50RunMs, P95RunMs: o.P95RunMs, CostUSD: o.CostUSD,
		CostPartial: o.CostUnknownSteps > 0, CostUnknownSteps: o.CostUnknownSteps,
		InputTokens: o.InputTokens, OutputTokens: o.OutputTokens,
		Series: series, TopFailingTools: tools,
	})
}

var alertWindows = map[string]time.Duration{"1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour}

// AlertJSON is a computed detection. Persisted alert rules and delivery
// arrive in step 7; this endpoint evaluates detections on read.
type AlertJSON struct {
	Kind      string    `json:"kind"`
	Severity  string    `json:"severity"`
	TraceID   string    `json:"trace_id"`
	ToolName  string    `json:"tool_name"`
	Repeats   uint64    `json:"repeats"`
	Threshold int       `json:"threshold"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// insights: what needs attention in a window (agents, grouped failures,
// and the previous period for comparison).
func (s *Server) insights(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("window")
	if name == "" {
		name = "24h"
	}
	win, ok := overviewWindows[name]
	if !ok {
		badRequest(w, "window must be 24h, 7d or 30d")
		return
	}
	to := s.now()
	in, err := s.Store.Insights(r.Context(), tenantFrom(r.Context()), to.Add(-win.window), to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	type outlierJSON struct {
		Run     RunJSON `json:"run"`
		Reason  string  `json:"reason"`
		Value   float64 `json:"value"`
		Typical float64 `json:"typical"`
	}
	outliers := make([]outlierJSON, 0, len(in.Outliers))
	for _, o := range in.Outliers {
		outliers = append(outliers, outlierJSON{Run: runJSON(o.Run), Reason: o.Reason, Value: o.Value, Typical: o.Typical})
	}
	// Sessions where users gave more negative than positive feedback.
	type unhappyJSON struct {
		SessionID string                `json:"session_id"`
		Feedback  query.FeedbackSummary `json:"feedback"`
	}
	unhappy := []unhappyJSON{}
	if fb, err := s.sessionFeedback(r, to.Add(-win.window), to); err == nil {
		for id, f := range fb {
			if f.Negative > 0 && f.Negative >= f.Positive {
				unhappy = append(unhappy, unhappyJSON{id, f})
			}
		}
		sort.Slice(unhappy, func(i, j int) bool {
			if unhappy[i].Feedback.Negative != unhappy[j].Feedback.Negative {
				return unhappy[i].Feedback.Negative > unhappy[j].Feedback.Negative
			}
			return unhappy[i].SessionID < unhappy[j].SessionID
		})
		if len(unhappy) > 10 {
			unhappy = unhappy[:10]
		}
	} else {
		s.logger().Warn("feedback lookup failed", "err", err)
	}
	writeJSON(w, http.StatusOK, struct {
		Agents   []query.AgentSummary `json:"agents"`
		Issues   []query.Issue        `json:"issues"`
		Outliers []outlierJSON        `json:"outliers"`
		Unhappy  []unhappyJSON        `json:"unhappy_sessions"`
		Previous query.PeriodTotals   `json:"previous"`
	}{in.Agents, in.Issues, outliers, unhappy, in.Previous})
}

func (s *Server) alerts(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("window")
	if name == "" {
		name = "1h"
	}
	win, ok := alertWindows[name]
	if !ok {
		badRequest(w, "window must be 1h, 6h or 24h")
		return
	}
	threshold := s.LoopMinRepeats
	if threshold <= 0 {
		threshold = 5
	}
	to := s.now()
	loops, err := s.Store.Loops(r.Context(), tenantFrom(r.Context()), to.Add(-win), to, threshold)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	out := make([]AlertJSON, 0, len(loops))
	for _, l := range loops {
		out = append(out, AlertJSON{
			Kind: "runaway_loop", Severity: "warning", TraceID: l.TraceID, ToolName: l.ToolName,
			Repeats: l.Repeats, Threshold: threshold, FirstSeen: l.FirstSeen.Time, LastSeen: l.LastSeen.Time,
		})
	}
	writeJSON(w, http.StatusOK, struct {
		Window string      `json:"window"`
		Alerts []AlertJSON `json:"alerts"`
	}{name, out})
}

// ---- account and keys ----------------------------------------------------

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": p.UserID, "tenant_id": p.TenantID, "tenant_name": p.TenantName,
		"role": p.Role, "can_manage_keys": p.CanManageKeys(),
	})
}

var keyIDRe = regexp.MustCompile(`^key_[0-9a-f]{20}$`)

const (
	defaultRotateGrace = 24 * time.Hour
	maxRotateGrace     = 7 * 24 * time.Hour
	maxBodyBytes       = 4 << 10  // key endpoints
	maxConfigBodyBytes = 16 << 10 // 20 patterns of up to 200 characters
)

// keysAllowed checks the key store is configured and, for changes, the role.
func (s *Server) keysAllowed(w http.ResponseWriter, r *http.Request, change bool) (session.Principal, bool) {
	p := principalFrom(r.Context())
	if s.Keys == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "key management is not configured")
		return p, false
	}
	if change && !p.CanManageKeys() {
		writeError(w, http.StatusForbidden, "forbidden", "only owners and admins can manage keys")
		return p, false
	}
	return p, true
}

// decodeBody reads a small JSON object; an empty body is allowed.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeBodyLimit(w, r, v, maxBodyBytes)
}

func decodeBodyLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		badRequest(w, "invalid JSON body")
		return false
	}
	return true
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	p, ok := s.keysAllowed(w, r, false)
	if !ok {
		return
	}
	keys, err := s.Keys.ListKeys(r.Context(), p.TenantID)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if keys == nil {
		keys = []control.APIKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	p, ok := s.keysAllowed(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	k, secret, err := s.Keys.CreateKey(r.Context(), p.TenantID, p.UserID, body.Name)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "secret": secret})
}

func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request) {
	p, ok := s.keysAllowed(w, r, true)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !keyIDRe.MatchString(id) {
		badRequest(w, "invalid key id")
		return
	}
	var body struct {
		GraceHours *float64 `json:"grace_hours"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	grace := defaultRotateGrace
	if body.GraceHours != nil {
		if *body.GraceHours < 0 || time.Duration(*body.GraceHours*float64(time.Hour)) > maxRotateGrace {
			badRequest(w, "grace_hours must be between 0 and 168")
			return
		}
		grace = time.Duration(*body.GraceHours * float64(time.Hour))
	}
	k, secret, err := s.Keys.RotateKey(r.Context(), p.TenantID, p.UserID, id, grace)
	if errors.Is(err, control.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no active key with that id")
		return
	}
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "secret": secret, "old_key_expires_in_hours": grace.Hours()})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	p, ok := s.keysAllowed(w, r, true)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !keyIDRe.MatchString(id) {
		badRequest(w, "invalid key id")
		return
	}
	err := s.Keys.RevokeKey(r.Context(), p.TenantID, id)
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

// ---- remote SDK config -------------------------------------------------------

func configJSON(c sdkconfig.Config) map[string]any {
	if c.RedactPatterns == nil {
		c.RedactPatterns = []sdkconfig.Pattern{}
	}
	out := map[string]any{
		"version": c.Version, "sample_rate": c.SampleRate, "capture_content": c.CaptureContent,
		"redact_patterns": c.RedactPatterns, "poll_interval_seconds": sdkconfig.PollSeconds,
	}
	if !c.UpdatedAt.IsZero() {
		out["updated_at"] = c.UpdatedAt
	}
	return out
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	if s.Config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "remote config is not configured")
		return
	}
	c, err := s.Config.GetSDKConfig(r.Context(), tenantFrom(r.Context()))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, configJSON(c))
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if s.Config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "remote config is not configured")
		return
	}
	if !p.CanManageKeys() {
		writeError(w, http.StatusForbidden, "forbidden", "only owners and admins can change SDK settings")
		return
	}
	var body struct {
		Version        *int64              `json:"version"`
		SampleRate     *float64            `json:"sample_rate"`
		CaptureContent *bool               `json:"capture_content"`
		RedactPatterns []sdkconfig.Pattern `json:"redact_patterns"`
	}
	if !decodeBodyLimit(w, r, &body, maxConfigBodyBytes) {
		return
	}
	if body.Version == nil || body.SampleRate == nil || body.CaptureContent == nil {
		badRequest(w, "version, sample_rate and capture_content are required")
		return
	}
	c := sdkconfig.Config{Version: *body.Version, SampleRate: *body.SampleRate, CaptureContent: *body.CaptureContent, RedactPatterns: body.RedactPatterns}
	if err := sdkconfig.Validate(c); err != nil {
		badRequest(w, err.Error()) // our own validation messages, safe to show
		return
	}
	saved, err := s.Config.PutSDKConfig(r.Context(), p.TenantID, p.UserID, c)
	if errors.Is(err, control.ErrVersionConflict) {
		writeError(w, http.StatusConflict, "conflict", "settings were changed by someone else; reload and try again")
		return
	}
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, configJSON(saved))
}

// ---- agent limits ----------------------------------------------------------

const maxLimitsBodyBytes = 32 << 10 // 50 limits with long agent names

// limitsJSON is the limits view: the limits, which agents are over budget
// today, and today's priced spend per agent (UTC day) for context.
func (s *Server) limitsJSON(r *http.Request, c sdkconfig.Config) map[string]any {
	limits, over := c.Limits, c.OverBudget
	if limits == nil {
		limits = []sdkconfig.Limit{}
	}
	if over == nil {
		over = []string{}
	}
	out := map[string]any{"version": c.Version, "limits": limits, "over_budget": over}
	now := s.now().UTC()
	day := now.Truncate(24 * time.Hour)
	if spend, err := s.Store.CostByAgent(r.Context(), tenantFrom(r.Context()), day, now); err == nil {
		out["spend_today"] = spend
		out["spend_since"] = day
	} else {
		s.logger().Warn("spend lookup failed", "err", err)
	}
	return out
}

func (s *Server) getLimits(w http.ResponseWriter, r *http.Request) {
	if s.Config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "remote config is not configured")
		return
	}
	c, err := s.Config.GetSDKConfig(r.Context(), tenantFrom(r.Context()))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.limitsJSON(r, c))
}

func (s *Server) putLimits(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if s.Config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "remote config is not configured")
		return
	}
	if !p.CanManageKeys() {
		writeError(w, http.StatusForbidden, "forbidden", "only owners and admins can change agent limits")
		return
	}
	var body struct {
		Version *int64            `json:"version"`
		Limits  []sdkconfig.Limit `json:"limits"`
	}
	if !decodeBodyLimit(w, r, &body, maxLimitsBodyBytes) {
		return
	}
	if body.Version == nil || body.Limits == nil {
		badRequest(w, "version and limits are required")
		return
	}
	if err := sdkconfig.ValidateLimits(body.Limits); err != nil {
		badRequest(w, err.Error())
		return
	}
	saved, err := s.Config.PutLimits(r.Context(), p.TenantID, p.UserID, *body.Version, body.Limits)
	if errors.Is(err, control.ErrVersionConflict) {
		writeError(w, http.StatusConflict, "conflict", "settings were changed by someone else; reload and try again")
		return
	}
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.limitsJSON(r, saved))
}

// ---- model prices ------------------------------------------------------------

const maxPricesBodyBytes = 64 << 10 // 100 prices with notes

func pricesJSON(c sdkconfig.Config) map[string]any {
	ps := c.Prices
	if ps == nil {
		ps = []sdkconfig.PriceOverride{}
	}
	return map[string]any{"version": c.Version, "prices": ps}
}

func (s *Server) getPrices(w http.ResponseWriter, r *http.Request) {
	if s.Config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "remote config is not configured")
		return
	}
	c, err := s.Config.GetSDKConfig(r.Context(), tenantFrom(r.Context()))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pricesJSON(c))
}

func (s *Server) putPrices(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if s.Config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "remote config is not configured")
		return
	}
	if !p.CanManageKeys() {
		writeError(w, http.StatusForbidden, "forbidden", "only owners and admins can change model prices")
		return
	}
	var body struct {
		Version *int64                    `json:"version"`
		Prices  []sdkconfig.PriceOverride `json:"prices"`
	}
	if !decodeBodyLimit(w, r, &body, maxPricesBodyBytes) {
		return
	}
	if body.Version == nil || body.Prices == nil {
		badRequest(w, "version and prices are required")
		return
	}
	if err := sdkconfig.ValidatePrices(body.Prices); err != nil {
		badRequest(w, err.Error())
		return
	}
	saved, err := s.Config.PutPrices(r.Context(), p.TenantID, p.UserID, *body.Version, body.Prices)
	if errors.Is(err, control.ErrVersionConflict) {
		writeError(w, http.StatusConflict, "conflict", "settings were changed by someone else; reload and try again")
		return
	}
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pricesJSON(saved))
}

// models lists the models used in a window, with how many calls were
// priced: what the model prices page needs to suggest overrides.
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("window")
	if name == "" {
		name = "7d"
	}
	win, ok := overviewWindows[name]
	if !ok {
		badRequest(w, "window must be 24h, 7d or 30d")
		return
	}
	to := s.now()
	ms, err := s.Store.Models(r.Context(), tenantFrom(r.Context()), to.Add(-win.window), to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": name, "models": ms})
}

// ---- tools and agent versions --------------------------------------------------

func (s *Server) window(w http.ResponseWriter, r *http.Request, def string) (string, time.Time, time.Time, bool) {
	name := r.URL.Query().Get("window")
	if name == "" {
		name = def
	}
	win, ok := overviewWindows[name]
	if !ok {
		badRequest(w, "window must be 24h, 7d or 30d")
		return "", time.Time{}, time.Time{}, false
	}
	to := s.now()
	return name, to.Add(-win.window), to, true
}

func (s *Server) tools(w http.ResponseWriter, r *http.Request) {
	name, from, to, ok := s.window(w, r, "7d")
	if !ok {
		return
	}
	ts, err := s.Store.Tools(r.Context(), tenantFrom(r.Context()), from, to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": name, "tools": ts})
}

func (s *Server) agentVersions(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if len(agent) > maxFilterLen {
		badRequest(w, "agent is too long")
		return
	}
	name, from, to, ok := s.window(w, r, "7d")
	if !ok {
		return
	}
	vs, err := s.Store.AgentVersions(r.Context(), tenantFrom(r.Context()), agent, from, to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": name, "agent": agent, "versions": vs})
}

// toolUsage: per agent version, how often each tool is called per run.
func (s *Server) toolUsage(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if len(agent) > maxFilterLen {
		badRequest(w, "agent is too long")
		return
	}
	name, from, to, ok := s.window(w, r, "7d")
	if !ok {
		return
	}
	vs, err := s.Store.ToolUsage(r.Context(), tenantFrom(r.Context()), agent, from, to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": name, "agent": agent, "versions": vs})
}
