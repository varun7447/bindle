package alerts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/passport"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

type driftDetector struct {
	fakeDetector
	sigs  []query.DriftSignal
	calls map[string]uint64
}

func (d *driftDetector) PassportDrift(_ context.Context, _ string, _, _ time.Time) ([]query.DriftSignal, error) {
	return d.sigs, nil
}
func (d *driftDetector) ToolCalls(_ context.Context, _, _, pattern string, _, _ time.Time) (uint64, string, error) {
	return d.calls[pattern], "trace-rate", nil
}

type fixedPassports struct {
	set passport.Set
	err error
}

func (f fixedPassports) Passports(context.Context, string) (passport.Set, error) { return f.set, f.err }
func (f fixedPassports) Tenants(context.Context) ([]string, error)               { return []string{"t1", "t2"}, nil }

func testPassport(t *testing.T) *passport.Passport {
	t.Helper()
	p, err := passport.Parse([]byte(`{"version":"bindle.passport/v1","agent":"refunds","owner":"payments",
		"tools":{"allow":["lookup_order"]},"limits":[{"tool":"refund_payment","max":3,"window":"5m"}],"budget":{"per_day_usd":10}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPassportDriftRule(t *testing.T) {
	st := &memStore{
		rules:  map[string][]Rule{"t1": {{ID: "r_drift", TenantID: "t1", Kind: KindPassportDrift, Threshold: 2, WindowMinutes: 60, Enabled: true}}},
		events: map[string]Event{}, delivery: map[string]map[string]string{},
	}
	det := &driftDetector{
		fakeDetector: fakeDetector{spend: map[string]float64{"refunds": 12.5}},
		sigs: []query.DriftSignal{
			{Kind: passport.ToolNotAllowed, Agent: "refunds", Steps: 3, Runs: 2, Reasons: map[string]uint64{"tool_not_allowed:refund_payment": 3},
				Examples: []query.SecurityExample{{TraceID: "tr1"}}},
			{Kind: passport.EgressBlocked, Agent: "refunds", Steps: 1, Runs: 1}, // under the threshold
		},
		calls: map[string]uint64{"refund_payment": 4},
	}
	now := time.Date(2026, 11, 24, 12, 31, 0, 0, time.UTC)
	e := &Evaluator{Store: st, Detect: det, Passports: fixedPassports{set: passport.Set{"refunds": testPassport(t)}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	for i := 0; i < 2; i++ { // the second run is within the same windows: no new events
		if err := e.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var summaries []string
	for _, ev := range st.events {
		summaries = append(summaries, ev.Summary)
	}
	if len(st.events) != 3 {
		t.Fatalf("events %d: %v", len(st.events), summaries)
	}
	joined := strings.Join(summaries, "\n")
	for _, want := range []string{
		"refunds: 3 steps with calls to tools outside the passport in the last 60 minutes (refund_payment ×3)",
		"refunds called refund_payment 4 times in the last 5m (passport limit 3)",
		"refunds has spent $12.50 today (passport daily budget $10.00)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	// The next rate window fires again; the day budget doesn't.
	now = now.Add(5 * time.Minute)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Per-step drift keeps its 60-minute slot at 12:36, so only the rate
	// finding is new.
	if len(st.events) != 4 {
		t.Fatalf("after 5 minutes: %d events", len(st.events))
	}
	if err := ValidateRule(Rule{Kind: KindPassportDrift, Threshold: 0.5, WindowMinutes: 60}); err == nil {
		t.Fatal("fractional threshold accepted")
	}
	if err := ValidateRule(Rule{Kind: KindPassportDrift, Threshold: 1, WindowMinutes: 60}); err != nil {
		t.Fatal(err)
	}
	// Without passports configured the rule fails rather than staying silent.
	e.Passports = nil
	if _, err := e.detect(context.Background(), "t1", st.rules["t1"][0]); err == nil {
		t.Fatal("rule without passports should fail")
	}
}

type recorded struct {
	tenant, agent string
	findings      []passport.Finding
}

func TestDriftJournal(t *testing.T) {
	var got []recorded
	det := &driftDetector{fakeDetector: fakeDetector{spend: map[string]float64{"refunds": 3}}, calls: map[string]uint64{"refund_payment": 9}}
	j := &DriftJournal{
		Tenants: fixedPassports{}, Passports: fixedPassports{set: passport.Set{"refunds": testPassport(t)}},
		Counter: driftCounter{det, det},
		Record: func(_ context.Context, tenant, agent string, f []passport.Finding, _ time.Time) error {
			got = append(got, recorded{tenant, agent, f})
			return nil
		},
		Features: featureSet{"t1/ledger": true},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].tenant != "t1" || got[0].agent != "refunds" || len(got[0].findings) != 1 || got[0].findings[0].Kind != passport.RateOverLimit {
		t.Fatalf("recorded %+v", got)
	}
	// A passport lookup failure is logged, not fatal.
	j.Passports = fixedPassports{err: errors.New("db down")}
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type featureSet map[string]bool

func (f featureSet) Enabled(_ context.Context, tenant, feature string) bool {
	return f[tenant+"/"+feature]
}
