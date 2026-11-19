package search

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/query"
)

func TestProblemRequest(t *testing.T) {
	from, to := t0, t0.Add(time.Hour)
	cases := []struct {
		f     query.ProblemFilter
		index string
		want  []string // fragments of the clauses
	}{
		{query.ProblemFilter{Kind: query.SourceFailure, StepName: "lookup", Pattern: "timeout <n>", Agent: "a"}, "steps",
			[]string{`"status":"failure"`, `"name":"lookup"`, `"error_pattern":"timeout \u003cn\u003e"`, `"agent_name":"a"`}},
		{query.ProblemFilter{Kind: query.SourceNegativeFeedback, Agent: "ignored"}, "steps",
			[]string{`"score_source":"user"`, `"score_pass":"fail"`}},
		{query.ProblemFilter{Kind: query.SourceFailedScore, Score: "judge"}, "steps",
			[]string{`"score_pass":"fail"`, `"must_not":[{"term":{"score_source":"user"}}]`, `"score_name":"judge"`}},
		{query.ProblemFilter{Kind: query.SourceFailedRuns, Agent: "a"}, "traces",
			[]string{`"failed_steps"`, `"agent_name":"a"`, `"first_start_ns"`}},
	}
	for _, c := range cases {
		c.f.From, c.f.To = from, to
		index, clauses, body, err := problemRequest(c.f)
		if err != nil || index != c.index {
			t.Fatalf("%s: %s %v", c.f.Kind, index, err)
		}
		js, _ := json.Marshal(clauses)
		for _, w := range c.want {
			if !strings.Contains(string(js), w) {
				t.Errorf("%s: clauses %s lack %s", c.f.Kind, js, w)
			}
		}
		if c.f.Kind == query.SourceNegativeFeedback && strings.Contains(string(js), "ignored") {
			t.Errorf("feedback filtered by agent on the step: %s", js)
		}
		b, _ := json.Marshal(body)
		if index == "steps" && !strings.Contains(string(b), `"size":200`) {
			t.Errorf("%s: limit not capped: %s", c.f.Kind, b)
		}
	}
	if _, _, _, err := problemRequest(query.ProblemFilter{Kind: "x"}); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestEconomicsMath(t *testing.T) {
	b := econBucket{Key: "v1", DocCount: 10, Cost: numValue{ptr(5.0)}}
	b.Failed.DocCount, b.Failed.Cost = 2, numValue{ptr(1.0)}
	b.Unknown.DocCount = 1

	// Inferred: 2 failed + 1 disliked run that succeeded.
	e := economics("a", false, b, &econAdjust{dislikedOK: 1, dislikedOKUSD: 0.5})
	if e.Definition != query.OutcomeInferred || e.Wasted != 3 || e.Successes != 7 || e.WastedUSD != 1.5 || e.UnknownCostRuns != 1 {
		t.Errorf("inferred %+v", e)
	}
	if math.Abs(*e.CostPerSuccess-5.0/7) > 1e-9 {
		t.Errorf("cost per success %v", *e.CostPerSuccess)
	}
	// Explicit: 6 runs reported; 4 passed, 2 failed (one of them also had a
	// failed step). The other failed step run has no outcome.
	e = economics("a", true, b, &econAdjust{outcomeRuns: 6, outcomePass: 4, outcomeFail: 2, outcomeFailUSD: 0.8,
		failedWithOutcome: 1, failedWithOutcomeUSD: 0.3})
	if e.Definition != query.OutcomeExplicit || e.Successes != 4 || e.Wasted != 3 || math.Abs(e.WastedUSD-1.5) > 1e-9 || e.OutcomeRuns != 6 {
		t.Errorf("explicit %+v", e)
	}
	// No successes: no cost per success, never divided by zero.
	e = economics("a", true, b, nil)
	if e.CostPerSuccess != nil || e.Successes != 0 || e.Wasted != 2 {
		t.Errorf("none %+v", e)
	}
}

func ptr(v float64) *float64 { return &v }

func TestEconomicsQueries(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, r.URL.Path+" "+string(b))
		if !strings.Contains(r.URL.RawQuery, "routing=t1") || !bytes.Contains(b, []byte(`{"term":{"tenant_id":"t1"}}`)) {
			t.Errorf("not tenant scoped: %s %s", r.URL.RawQuery, b)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "-traces/_search") && bytes.Contains(b, []byte(`"agents"`)):
			_, _ = io.WriteString(w, `{"aggregations":{"agents":{"buckets":[
				{"key":"refunds","versions":{"buckets":[{"key":"v2","doc_count":4,"cost":{"value":2},"failed":{"doc_count":1,"cost":{"value":0.5}},"unknown":{"doc_count":0}}]}},
				{"key":"triage","versions":{"buckets":[{"key":"v1","doc_count":3,"cost":{"value":0.3},"failed":{"doc_count":0,"cost":{"value":0}},"unknown":{"doc_count":1}}]}}]}}}`)
		case strings.HasSuffix(r.URL.Path, "-traces/_search"):
			_, _ = io.WriteString(w, `{"hits":{"hits":[
				{"_source":{"trace_id":"r1","agent_name":"refunds","agent_version":"v2","cost_usd":0.5,"failed_steps":0}},
				{"_source":{"trace_id":"r2","agent_name":"refunds","agent_version":"v2","cost_usd":0.7,"failed_steps":0}},
				{"_source":{"trace_id":"t1","agent_name":"triage","agent_version":"v1","cost_usd":0.1,"failed_steps":0}}]}}`)
		case bytes.Contains(b, []byte(`"score_source":"outcome"`)):
			_, _ = io.WriteString(w, `{"aggregations":{"traces":{"buckets":[
				{"key":"r1","pass":{"buckets":[{"key":"pass","doc_count":1}]}},
				{"key":"r2","pass":{"buckets":[{"key":"fail","doc_count":1}]}}]}}}`)
		default: // negative feedback
			_, _ = io.WriteString(w, `{"aggregations":{"traces":{"buckets":[{"key":"t1","pass":{"buckets":[{"key":"fail","doc_count":1}]}},
				{"key":"r1","pass":{"buckets":[{"key":"fail","doc_count":1}]}}]}}}`)
		}
	}))
	defer srv.Close()
	s := &Store{Client: &Client{URL: srv.URL, User: "reader", Password: "pw"}}
	out, err := s.Economics(context.Background(), "t1", t0, t0.Add(24*time.Hour))
	if err != nil || len(out) != 2 {
		t.Fatalf("%v %+v", err, out)
	}
	r, tr := out[0], out[1]
	// refunds sends outcomes: r1 passed, r2 failed, one failed run without
	// an outcome; r1's thumbs down doesn't change an explicit outcome.
	if r.Agent != "refunds" || r.Definition != query.OutcomeExplicit || r.Successes != 1 || r.Wasted != 2 || math.Abs(r.WastedUSD-1.2) > 1e-9 || *r.CostPerSuccess != 2 {
		t.Errorf("refunds %+v", r)
	}
	// triage doesn't: 3 runs, none failed, one disliked.
	if tr.Definition != query.OutcomeInferred || tr.Successes != 2 || tr.Wasted != 1 || math.Abs(tr.WastedUSD-0.1) > 1e-9 || tr.UnknownCostRuns != 1 {
		t.Errorf("triage %+v", tr)
	}
	if len(bodies) != 4 {
		t.Errorf("%d requests", len(bodies))
	}
}
