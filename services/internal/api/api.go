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
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/services/internal/query"
	"github.com/bindle-ai/bindle/services/internal/ratelimit"
	"github.com/bindle-ai/bindle/services/internal/session"
)

// Server holds the API's dependencies.
type Server struct {
	Store    query.Store
	Sessions session.Resolver
	Limiter  *ratelimit.Limiter // per-tenant requests/sec; nil disables
	Log      *slog.Logger
	Now      func() time.Time

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
	api.HandleFunc("GET /api/overview", s.overview)
	api.HandleFunc("GET /api/alerts", s.alerts)
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

func tenantFrom(ctx context.Context) string {
	t, _ := ctx.Value(ctxKey{}).(string)
	return t
}

func (s *Server) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, err := s.Sessions.Tenant(r)
		if err != nil || tenant == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="bindle"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "a valid session is required")
			return
		}
		if !s.Limiter.Allow(tenant, 1) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, tenant)))
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
	f.Agent, f.Environment = q.Get("agent"), q.Get("environment")
	if len(f.Agent) > maxFilterLen || len(f.Environment) > maxFilterLen {
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
