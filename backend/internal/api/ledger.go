package api

import (
	"context"
	"encoding/hex"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/ledger"
)

// FeatureLister reports a tenant's enabled features (operator-set flags).
// Implemented by *control.FeatureResolver.
type FeatureLister interface {
	List(ctx context.Context, tenant string) ([]string, error)
}

// LedgerReader reads a tenant's ledger. Implemented by *ledger.Reader.
type LedgerReader interface {
	List(ctx context.Context, tenant, agent string, from, to time.Time, beforeSeq int64, limit int) ([]ledger.Entry, error)
	Verify(ctx context.Context, tenant, agent string, from, to time.Time) (ledger.Result, error)
	Day(ctx context.Context, tenant, agent string, day time.Time) ([]ledger.Entry, error)
}

// features: GET /api/features, the workspace's enabled features.
func (s *Server) features(w http.ResponseWriter, r *http.Request) {
	list := []string{}
	if s.Features != nil {
		got, err := s.Features.List(r.Context(), tenantFrom(r.Context()))
		if err != nil {
			s.storeError(w, r, err)
			return
		}
		list = append(list, got...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"features": list})
}

// featureOn answers 404 (as if the route didn't exist) unless the feature
// is on for the caller's workspace.
func (s *Server) featureOn(w http.ResponseWriter, r *http.Request, feature string) bool {
	if s.Features != nil && s.Ledger != nil {
		got, err := s.Features.List(r.Context(), tenantFrom(r.Context()))
		if err != nil {
			s.storeError(w, r, err)
			return false
		}
		for _, f := range got {
			if f == feature {
				return true
			}
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	return false
}

const maxLedgerDays = 400

// ledgerParams reads agent and the day range: days=N (the last N UTC days,
// default 30) or from/to (YYYY-MM-DD).
func (s *Server) ledgerParams(w http.ResponseWriter, r *http.Request) (agent string, from, to time.Time, ok bool) {
	q := r.URL.Query()
	agent = q.Get("agent")
	if len(agent) > maxFilterLen {
		badRequest(w, "agent is too long")
		return "", time.Time{}, time.Time{}, false
	}
	today := s.now().UTC().Truncate(24 * time.Hour)
	to, from = today, today.AddDate(0, 0, -29)
	if v := q.Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLedgerDays {
			badRequest(w, "days must be between 1 and 400")
			return "", time.Time{}, time.Time{}, false
		}
		from = today.AddDate(0, 0, -(n - 1))
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			badRequest(w, "from must be a date (YYYY-MM-DD)")
			return "", time.Time{}, time.Time{}, false
		}
		from = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			badRequest(w, "to must be a date (YYYY-MM-DD)")
			return "", time.Time{}, time.Time{}, false
		}
		to = t
	}
	if to.Before(from) || to.Sub(from) > maxLedgerDays*24*time.Hour {
		badRequest(w, "the range must run forwards and cover at most 400 days")
		return "", time.Time{}, time.Time{}, false
	}
	return agent, from, to, true
}

type ledgerEntryJSON struct {
	ledger.Entry
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

// ledgerList: GET /api/agents/ledger?agent=&days=|from=&to=&before_seq=&limit=
func (s *Server) ledgerList(w http.ResponseWriter, r *http.Request) {
	if !s.featureOn(w, r, ledger.Feature) {
		return
	}
	agent, from, to, ok := s.ledgerParams(w, r)
	if !ok {
		return
	}
	var before int64
	if v := r.URL.Query().Get("before_seq"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			badRequest(w, "before_seq must be a positive integer")
			return
		}
		before = n
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			badRequest(w, "limit must be between 1 and 500")
			return
		}
		limit = n
	}
	es, err := s.Ledger.List(r.Context(), tenantFrom(r.Context()), agent, from, to, before, limit)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	out := make([]ledgerEntryJSON, len(es))
	for i, e := range es {
		out[i] = ledgerEntryJSON{Entry: e, PrevHash: hex.EncodeToString(e.PrevHash), Hash: hex.EncodeToString(e.Hash)}
	}
	resp := map[string]any{"agent": agent, "from": from.Format("2006-01-02"), "to": to.Format("2006-01-02"), "entries": out}
	if len(es) == limit {
		resp["next_before_seq"] = es[len(es)-1].Seq
	}
	writeJSON(w, http.StatusOK, resp)
}

// ledgerVerify: GET /api/agents/ledger/verify?agent=&days=|from=&to=
func (s *Server) ledgerVerify(w http.ResponseWriter, r *http.Request) {
	if !s.featureOn(w, r, ledger.Feature) {
		return
	}
	agent, from, to, ok := s.ledgerParams(w, r)
	if !ok {
		return
	}
	res, err := s.Ledger.Verify(r.Context(), tenantFrom(r.Context()), agent, from, to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": agent, "result": res})
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// ledgerSummary: GET /api/agents/ledger/summary?agent=&day=YYYY-MM-DD, the
// day's plain-English summary as Markdown (default: yesterday, UTC).
func (s *Server) ledgerSummary(w http.ResponseWriter, r *http.Request) {
	if !s.featureOn(w, r, ledger.Feature) {
		return
	}
	q := r.URL.Query()
	agent := q.Get("agent")
	if len(agent) > maxFilterLen {
		badRequest(w, "agent is too long")
		return
	}
	day := s.now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	if v := q.Get("day"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			badRequest(w, "day must be a date (YYYY-MM-DD)")
			return
		}
		day = t
	}
	tenant := tenantFrom(r.Context())
	es, err := s.Ledger.Day(r.Context(), tenant, agent, day)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	check, err := s.Ledger.Verify(r.Context(), tenant, agent, day, day)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	d := day.Format("2006-01-02")
	name := unsafeFileChars.ReplaceAllString(agent, "-")
	if name == "" || name == "-" {
		name = "unnamed-agent"
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="ledger-`+name+`-`+d+`.md"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(ledger.Summary(agent, d, es, check)))
}
