package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/ledger"
)

type fakeFeatures map[string][]string

func (f fakeFeatures) List(_ context.Context, tenant string) ([]string, error) { return f[tenant], nil }

type fakeLedger struct {
	gotTenant, gotAgent string
	gotFrom, gotTo      time.Time
	gotBefore           int64
	entries             []ledger.Entry
	result              ledger.Result
}

func (f *fakeLedger) List(_ context.Context, tenant, agent string, from, to time.Time, before int64, limit int) ([]ledger.Entry, error) {
	f.gotTenant, f.gotAgent, f.gotFrom, f.gotTo, f.gotBefore = tenant, agent, from, to, before
	if limit < len(f.entries) {
		return f.entries[:limit], nil
	}
	return f.entries, nil
}

func (f *fakeLedger) Verify(_ context.Context, tenant, agent string, from, to time.Time) (ledger.Result, error) {
	f.gotTenant, f.gotAgent, f.gotFrom, f.gotTo = tenant, agent, from, to
	return f.result, nil
}

func (f *fakeLedger) Day(_ context.Context, tenant, agent string, day time.Time) ([]ledger.Entry, error) {
	f.gotTenant, f.gotAgent, f.gotFrom = tenant, agent, day
	return f.entries, nil
}

func TestLedgerEndpoints(t *testing.T) {
	lg := &fakeLedger{
		entries: []ledger.Entry{
			{Agent: "refund-agent", Seq: 2, Kind: ledger.KindStep, StepType: "tool_call", StepName: "refund_payment", Status: "failure", ErrorClass: "timeout", CostUSD: "0", Hash: []byte{0xab}, PrevHash: []byte{0x01}},
			{Agent: "refund-agent", Seq: 1, Kind: ledger.KindStep, StepType: "llm_call", StepName: "plan", Status: "success", CostUSD: "0.002", CostKnown: true, Hash: []byte{0x01}, PrevHash: ledger.Genesis},
		},
		result: ledger.Result{OK: true, Entries: 2, FirstSeq: 1, LastSeq: 2},
	}
	srv := &Server{Store: &fakeStore{}, Sessions: tokenResolver{"tok-a": "tenant-a", "tok-b": "tenant-b"},
		Features: fakeFeatures{"tenant-a": {"ledger"}}, Ledger: lg,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	h := srv.Handler()

	// Flags are listed for the console.
	if w, body := get(t, h, "/api/features", "tok-a"); w.Code != 200 || len(body["features"].([]any)) != 1 {
		t.Fatalf("features %d %v", w.Code, body)
	}
	if w, body := get(t, h, "/api/features", "tok-b"); w.Code != 200 || len(body["features"].([]any)) != 0 {
		t.Fatalf("features for b %d %v", w.Code, body)
	}

	// Without the flag, the ledger doesn't exist.
	for _, p := range []string{"/api/agents/ledger?agent=x", "/api/agents/ledger/verify?agent=x", "/api/agents/ledger/summary?agent=x"} {
		if w, _ := get(t, h, p, "tok-b"); w.Code != 404 {
			t.Errorf("%s without the flag: %d", p, w.Code)
		}
	}

	w, body := get(t, h, "/api/agents/ledger?agent=refund-agent&days=7&limit=2", "tok-a")
	if w.Code != 200 {
		t.Fatalf("list %d %s", w.Code, w.Body)
	}
	es := body["entries"].([]any)
	first := es[0].(map[string]any)
	if lg.gotTenant != "tenant-a" || lg.gotAgent != "refund-agent" || first["hash"] != "ab" || first["prev_hash"] != "01" || first["seq"] != float64(2) {
		t.Fatalf("list: tenant %q agent %q first %v", lg.gotTenant, lg.gotAgent, first)
	}
	if body["next_before_seq"] != float64(1) {
		t.Errorf("next_before_seq = %v", body["next_before_seq"])
	}
	if got := lg.gotTo.Sub(lg.gotFrom); got != 6*24*time.Hour {
		t.Errorf("days=7 covers %v", got)
	}

	if w, body := get(t, h, "/api/agents/ledger/verify?agent=refund-agent&from=2026-11-01&to=2026-11-03", "tok-a"); w.Code != 200 ||
		body["result"].(map[string]any)["ok"] != true || lg.gotFrom.Format("2006-01-02") != "2026-11-01" {
		t.Fatalf("verify %d %v", w.Code, body)
	}

	req := httptest.NewRequest("GET", "/api/agents/ledger/summary?agent=refund%20agent/1&day=2026-11-20", nil)
	req.Header.Set("Authorization", "Bearer tok-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/markdown") ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), `filename="ledger-refund-agent-1-2026-11-20.md"`) ||
		!strings.Contains(rec.Body.String(), "# Ledger summary: refund agent/1, 2026-11-20 (UTC)") {
		t.Fatalf("summary %d %v\n%s", rec.Code, rec.Header(), rec.Body)
	}

	for _, p := range []string{"/api/agents/ledger?days=0", "/api/agents/ledger?days=x", "/api/agents/ledger?from=yesterday",
		"/api/agents/ledger?from=2026-11-05&to=2026-11-01", "/api/agents/ledger?before_seq=-1", "/api/agents/ledger?limit=9999",
		"/api/agents/ledger/summary?day=11/20", "/api/agents/ledger?agent=" + strings.Repeat("a", 300)} {
		if w, _ := get(t, h, p, "tok-a"); w.Code != 400 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w, _ := get(t, h, "/api/agents/ledger", ""); w.Code != 401 {
		t.Errorf("no auth: %d", w.Code)
	}

	// No ledger configured: hidden even when flagged.
	srv.Ledger = nil
	if w, _ := get(t, srv.Handler(), "/api/agents/ledger", "tok-a"); w.Code != 404 {
		t.Errorf("nil ledger: %d", w.Code)
	}
}
