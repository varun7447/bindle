package alerts

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/query"
)

// signalDetector adds security signals and topics to the fake detector.
type signalDetector struct {
	*fakeDetector
	signals []query.SecuritySignal
	topics  *query.Topics
}

func (s *signalDetector) SecuritySignals(_ context.Context, t string, _, _ time.Time) ([]query.SecuritySignal, error) {
	s.tenants = append(s.tenants, t)
	return s.signals, nil
}

func (s *signalDetector) Topics(_ context.Context, t, _ string, _, _ time.Time) (*query.Topics, error) {
	s.tenants = append(s.tenants, t)
	return s.topics, nil
}

func TestSecuritySignalRule(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 7, 0, 0, time.UTC)
	det := &signalDetector{fakeDetector: &fakeDetector{}, signals: []query.SecuritySignal{
		{Flag: "prompt_injection", Agent: "support", Steps: 3, Runs: 2,
			Reasons:  map[string]uint64{"prompt_injection:ignore_instructions": 3, "prompt_injection:chat_markup": 1},
			Examples: []query.SecurityExample{{TraceID: "t1", StepID: "s1"}}},
		{Flag: "secret_exposed", Agent: "billing", Steps: 1, Runs: 1, Reasons: map[string]uint64{"secret_exposed:credential_in_output": 1}},
	}}
	e := &Evaluator{Detect: det, Now: func() time.Time { return now }}
	r := Rule{Kind: KindSecuritySignal, Threshold: 2, WindowMinutes: 60}
	if err := ValidateRule(r); err != nil {
		t.Fatal(err)
	}
	got, err := e.detect(context.Background(), "t", r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TraceID != "t1" ||
		got[0].Summary != "support: 3 steps flagged for possible prompt injection in the last 60 minutes (2 runs): content the agent read tells it to ignore its instructions" {
		t.Fatalf("events = %+v", got)
	}
	// One event per flag, agent and window.
	if fp := got[0].Fingerprint; fp != "security:prompt_injection:support:"+strconv.FormatInt(now.Truncate(time.Hour).Unix(), 10) {
		t.Fatalf("fingerprint %s", fp)
	}
	r.Threshold = 1
	if got, _ := e.detect(context.Background(), "t", r); len(got) != 2 || !strings.Contains(got[1].Summary, "an exposed credential") {
		t.Fatalf("threshold 1: %+v", got)
	}
	for _, bad := range []float64{0, 1.5} {
		if ValidateRule(Rule{Kind: KindSecuritySignal, Threshold: bad, WindowMinutes: 60}) == nil {
			t.Errorf("threshold %v accepted", bad)
		}
	}
	// A detector without signals reports an error instead of staying silent.
	e.Detect = &fakeDetector{}
	if _, err := e.detect(context.Background(), "t", r); err == nil {
		t.Fatal("no error without a signal detector")
	}
}

func TestDigestIncludesTopics(t *testing.T) {
	monday := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	det := &signalDetector{fakeDetector: &fakeDetector{overview: &query.Overview{Runs: 10}},
		topics: &query.Topics{Rising: []query.Topic{{Term: "refund"}, {Term: "invoice"}}, New: []query.Topic{{Term: "outage"}}, Runs: 200, PreviousRuns: 100}}
	e := &Evaluator{Detect: det, Now: func() time.Time { return monday }}
	got, err := e.detect(context.Background(), "t", Rule{Kind: KindWeeklyDigest, Threshold: 1, WindowMinutes: 1440})
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %+v", err, got)
	}
	s := got[0].Summary
	for _, want := range []string{"Users asked more about: refund, invoice.", "New this week: outage.", "User requests rose 100%."} {
		if !strings.Contains(s, want) {
			t.Errorf("digest lacks %q: %s", want, s)
		}
	}
	if got[0].Details["new_topics"].([]string)[0] != "outage" {
		t.Errorf("details %v", got[0].Details)
	}
}
