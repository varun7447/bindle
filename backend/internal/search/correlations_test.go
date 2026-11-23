package search

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/correlate"
	"github.com/bindle-ai/bindle/backend/internal/outcomes"
)

func TestRunsByCorrelation(t *testing.T) {
	c := correlate.FromValue("t1", "ticket_id", "T-1")
	var seen []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"aggregations":{"traces":{"buckets":[{"key":"r2"},{"key":"r1"}]}}}`)
	}))
	defer srv.Close()
	s := &Store{Client: &Client{URL: srv.URL, User: "reader", Password: "pw"}}
	runs, err := s.RunsByCorrelation(context.Background(), "t1", []string{c, "not a stored value"}, t0, t0.Add(time.Hour))
	if err != nil || len(runs) != 2 || runs[0] != "r2" {
		t.Fatalf("runs %v %v", runs, err)
	}
	for _, want := range []string{`"correlations":["` + c + `"]`, `"tenant_id":"t1"`} {
		if !strings.Contains(string(seen), want) {
			t.Errorf("query %s lacks %s", seen, want)
		}
	}
	if strings.Contains(string(seen), "not a stored value") {
		t.Errorf("invalid correlation sent to search: %s", seen)
	}
	if none, err := s.RunsByCorrelation(context.Background(), "t1", []string{"x"}, t0, t0); err != nil || len(none) != 0 {
		t.Fatalf("no valid correlations should not query: %v %v", none, err)
	}
}

func TestGradingFactsAndGradedEconomics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "-traces/_search") && bytes.Contains(b, []byte(`"agents"`)):
			_, _ = io.WriteString(w, `{"aggregations":{"agents":{"buckets":[{"key":"support","versions":{"buckets":[
				{"key":"v12","doc_count":2,"cost":{"value":1},"failed":{"doc_count":0,"cost":{"value":0}},"unknown":{"doc_count":0}}]}}]}}}`)
		case strings.HasSuffix(r.URL.Path, "-traces/_search"):
			_, _ = io.WriteString(w, `{"hits":{"total":{"value":2},"hits":[
				{"_source":{"trace_id":"r1","agent_name":"support","agent_version":"v12","cost_usd":0.5,"failed_steps":0}},
				{"_source":{"trace_id":"r2","agent_name":"support","agent_version":"v12","cost_usd":0.5,"failed_steps":0}}]}}`)
		case bytes.Contains(b, []byte(`"names"`)): // in-run outcomes by name
			_, _ = io.WriteString(w, `{"aggregations":{"traces":{"buckets":[
				{"key":"r1","names":{"buckets":[{"key":"ticket_resolved","pass":{"buckets":[{"key":"pass","doc_count":1}]}}]}}]}}}`)
		case bytes.Contains(b, []byte(`"score_source":"user"`)) && bytes.Contains(b, []byte(`"must_not"`)): // evals
			_, _ = io.WriteString(w, `{"aggregations":{"traces":{"buckets":[
				{"key":"r1","pass":{"buckets":[{"key":"pass","doc_count":2}]}},{"key":"r2","pass":{"buckets":[{"key":"pass","doc_count":1}]}}]}}}`)
		default:
			_, _ = io.WriteString(w, `{"aggregations":{"traces":{"buckets":[]}}}`)
		}
	}))
	defer srv.Close()
	s := &Store{Client: &Client{URL: srv.URL, User: "reader", Password: "pw"}}
	ctx := context.Background()
	runs, inRun, truncated, err := s.GradingFacts(ctx, "t1", t0, t0.Add(24*time.Hour))
	if err != nil || truncated || len(runs) != 2 || runs[0].EvalPass != 2 || len(inRun["r1"]) != 1 {
		t.Fatalf("facts %+v %+v %v %v", runs, inRun, truncated, err)
	}
	settings := outcomes.Settings{ReversalOutcomes: []string{"ticket_reopened"}}
	// r2 was reopened later (matched by correlation): a reversal.
	extra := map[string][]outcomes.OutcomeFact{"r2": {{Name: "ticket_reopened", Success: true}}}
	g := outcomes.GradeRuns(runs, mergeFacts(inRun, extra), settings)
	if len(g) != 1 || g[0].Reversed != 1 || g[0].ReversedAfterPassing != 1 || g[0].Successes != 1 {
		t.Fatalf("grade %+v", g)
	}
	econ, err := s.EconomicsGraded(ctx, "t1", t0, t0.Add(24*time.Hour), extra, settings)
	if err != nil || len(econ) != 1 {
		t.Fatalf("economics %+v %v", econ, err)
	}
	if e := econ[0]; e.Successes != 1 || e.Wasted != 1 || e.OutcomeRuns != 2 {
		js, _ := json.Marshal(e)
		t.Fatalf("a reversal must count as a failed outcome: %s", js)
	}
}

func mergeFacts(a, b map[string][]outcomes.OutcomeFact) map[string][]outcomes.OutcomeFact {
	out := map[string][]outcomes.OutcomeFact{}
	for k, v := range a {
		out[k] = append(out[k], v...)
	}
	for k, v := range b {
		out[k] = append(out[k], v...)
	}
	return out
}
