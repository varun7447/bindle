package ledger

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/passport"
)

func TestDriftEntriesCarryNoContent(t *testing.T) {
	s := step("t", "a", 0)
	s.Input, s.Output, s.InputTokens, s.ErrorMessage = "secret prompt", "secret answer", 99, "raw error"
	e := DriftFromStep(s, "tool_not_allowed:lookup")
	if e.Kind != KindDrift || e.EntryKey != "tool_not_allowed:lookup" || e.InputTokens != 0 || e.CostUSD != "0" {
		t.Fatalf("drift entry %+v", e)
	}
	for _, leak := range []string{"secret prompt", "secret answer", "raw error"} {
		if strings.Contains(string(Canonical(e)), leak) {
			t.Fatalf("drift entry contains %q", leak)
		}
	}
	if e.Detail != `{"reason":"tool_not_allowed:lookup"}` {
		t.Fatalf("detail %s", e.Detail)
	}
	rate := Drift("a", "rate_over_limit:refund_payment", "rate_over_limit:refund_payment:1700000000", "", time.Now(), nil)
	sum := Summary("a", "2026-11-24", []Entry{FromStep(s), e, rate}, Result{OK: true, Entries: 3})
	if !strings.Contains(sum, "**Passport drift.** 2 times outside the agent's passport") ||
		!strings.Contains(sum, "unlisted tool") || !strings.Contains(sum, "tool over its rate limit") || !strings.Contains(sum, "nothing was blocked") {
		t.Fatalf("summary:\n%s", sum)
	}
}

func TestIntegrationLedgerDrift(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	inner := &memWriter{}
	s := &Sink{Inner: inner, Ledger: &Writer{DB: db.ingest}, Features: fakeFeatures{"on": true}}
	st := step("on", "a", 0)
	st.PassportDrift = []string{"egress_blocked:CARD", "tool_not_allowed:lookup"}
	for i := 0; i < 2; i++ { // the second write is a retry: no duplicates
		if err := s.Write(ctx, []model.Step{st, step("on", "a", 1)}); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reader{DB: db.api}
	if res := mustVerify(t, r, "on", "a"); !res.OK || res.Entries != 4 {
		t.Fatalf("want 2 steps + 2 drift entries: %+v", res)
	}
	now := time.Now().UTC()
	list, err := r.List(ctx, "on", "a", now.AddDate(0, 0, -1), now, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	drift := 0
	for _, e := range list {
		if e.Kind == KindDrift {
			drift++
		}
	}
	if drift != 2 {
		t.Fatalf("%d drift entries", drift)
	}

	// The alerts worker records rate-limit and budget drift, once per
	// period, with its own role.
	w := &Writer{DB: db.alerts}
	findings := []passport.Finding{
		{Kind: passport.RateOverLimit, Agent: "a", Detail: "refund_payment", Value: 4, Limit: 3, Window: "5m", TraceID: st.TraceID},
		{Kind: passport.DayOverBudget, Agent: "a", Detail: now.Format("2006-01-02"), Value: 201, Limit: 200},
	}
	at := now.Truncate(5 * time.Minute).Add(time.Minute)
	for _, t2 := range []time.Time{at, at.Add(time.Minute)} { // same 5-minute window
		if err := w.RecordFindings(ctx, "on", "a", findings, t2); err != nil {
			t.Fatal(err)
		}
	}
	if res := mustVerify(t, r, "on", "a"); !res.OK || res.Entries != 6 {
		t.Fatalf("want 2 new drift entries in one window: %+v", res)
	}
	if err := w.RecordFindings(ctx, "on", "a", findings, at.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if res := mustVerify(t, r, "on", "a"); !res.OK || res.Entries != 7 {
		t.Fatalf("next window adds the rate finding only (the day budget is once a day): %+v", res)
	}
}
