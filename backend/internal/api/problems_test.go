package api

import (
	"strings"
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/evals"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

func tid(c string) string { return strings.Repeat(c, 32) }

func problemStore() *fakeStore {
	// a, b: problem runs of "refunds" (a leaked the error); c: another agent;
	// g1..g6: good runs that always call lookup_order in about 4 steps.
	st := &fakeStore{problemIDs: []string{tid("a"), tid("b"), tid("c")}}
	st.evalRuns = []evals.Run{
		{TraceID: tid("a"), Agent: "refunds", Output: "Sorry: timeout after 30s on order 12", Tools: []string{}, Steps: 40, Failed: true},
		{TraceID: tid("b"), Agent: "refunds", Output: "done", Tools: []string{"lookup_order"}, Steps: 5, Failed: true},
		{TraceID: tid("c"), Agent: "billing", Output: "x", Failed: true},
	}
	for _, c := range "123456" {
		id := tid(string(c))
		st.runs = append(st.runs, query.Run{TraceID: id})
		st.evalRuns = append(st.evalRuns, evals.Run{TraceID: id, Agent: "refunds", Output: "ok", Tools: []string{"lookup_order"}, Steps: 4})
	}
	return st
}

func TestProblemSuggestions(t *testing.T) {
	st := problemStore()
	ev := newFakeEvals()
	ev.scorers = []evals.Scorer{{ID: "sc_x", Name: "No step fails", Kind: evals.KindSucceeded, Agent: "refunds", Enabled: true},
		{ID: "sc_y", Name: "Calls lookup_order", Kind: evals.KindContains, Value: "something else", Enabled: true}}
	h := evalServer(st, ev)

	w, body := send(t, h, "GET", "/api/problems/suggestions?kind=failure&step_name=refund&pattern=timeout+after+%3Cn%3Es+on+order+%3Cn%3E&agent=refunds", "tok-a", "")
	if w.Code != 200 {
		t.Fatalf("%d %v", w.Code, body)
	}
	if st.problem.Kind != "failure" || st.problem.Pattern != "timeout after <n>s on order <n>" || st.problem.Agent != "refunds" || st.problem.To.Sub(st.problem.From).Hours() != 168 {
		t.Errorf("filter %+v", st.problem)
	}
	if body["matched"].(float64) != 2 || body["source"] != "failure: refund: timeout after <n>s on order <n>" {
		t.Errorf("matched/source: %v", body)
	}
	got := map[string]map[string]any{}
	for _, s := range body["suggestions"].([]any) {
		m := s.(map[string]any)
		sc := m["scorer"].(map[string]any)
		got[sc["kind"].(string)] = m
	}
	if s := got["run_succeeded"]; s == nil || s["exists"] != true || s["catches"].(float64) != 2 {
		t.Errorf("run_succeeded: %v", s)
	}
	if s := got["not_contains"]; s == nil || s["scorer"].(map[string]any)["value"] != "timeout after" || s["catches"].(float64) != 1 {
		t.Errorf("not_contains: %v", s)
	}
	// The name is taken by a different check, so it gets a suffix.
	if s := got["tool_called"]; s == nil || s["scorer"].(map[string]any)["name"] != "Calls lookup_order (2)" || s["catches"].(float64) != 1 {
		t.Errorf("tool_called: %v", s)
	}
	if s := got["max_steps"]; s == nil || s["scorer"].(map[string]any)["number"].(float64) != 9 || s["catches"].(float64) != 1 {
		t.Errorf("max_steps: %v", s)
	}
	if _, ok := got["json_valid"]; ok {
		t.Error("json_valid suggested though good runs don't return JSON")
	}

	for _, bad := range []string{"kind=nope", "kind=failure", "kind=failure&pattern=x&window=1y", "kind=failed_runs&agent=" + strings.Repeat("x", 300)} {
		if w, _ := send(t, h, "GET", "/api/problems/suggestions?"+bad, "tok-a", ""); w.Code != 400 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	for _, tn := range st.tenants {
		if tn != "tenant-a" {
			t.Fatalf("tenant %q", tn)
		}
	}
}

func TestAddProblemItems(t *testing.T) {
	st := problemStore()
	ev := newFakeEvals()
	h := evalServer(st, ev)
	_, ds := send(t, h, "POST", "/api/datasets", "tok-a", `{"name":"refund failures"}`)
	id := ds["id"].(string)

	w, body := send(t, h, "POST", "/api/datasets/"+id+"/items/from-problems", "tok-a", `{"kind":"negative_feedback","agent":"refunds","window":"30d"}`)
	if w.Code != 200 || body["added"].(float64) != 2 || body["source"] != "negative feedback" {
		t.Fatalf("%d %v", w.Code, body)
	}
	items := ev.items[id]
	if len(items) != 2 || items[0].TraceID != tid("a") || items[0].Source != "negative feedback" {
		t.Errorf("items %+v", items)
	}
	if st.problem.Kind != "negative_feedback" || st.problem.To.Sub(st.problem.From).Hours() != 720 {
		t.Errorf("filter %+v", st.problem)
	}
	if w, _ := send(t, h, "POST", "/api/datasets/"+id+"/items/from-problems", "tok-a", `{"kind":"failure"}`); w.Code != 400 {
		t.Errorf("failure without a group: %d", w.Code)
	}
	if w, _ := send(t, h, "POST", "/api/datasets/ds_"+strings.Repeat("9", 20)+"/items/from-problems", "tok-a", `{"kind":"failed_runs"}`); w.Code != 404 {
		t.Errorf("unknown dataset: %d", w.Code)
	}
}

func TestEconomicsEndpoint(t *testing.T) {
	cps := 0.25
	st := &fakeStore{economics: []query.Economics{{Agent: "refunds", Version: "v2", Definition: query.OutcomeExplicit, Runs: 10, Successes: 4, CostUSD: 1, CostPerSuccess: &cps}}}
	h := evalServer(st, newFakeEvals())
	w, body := send(t, h, "GET", "/api/economics?window=30d", "tok-a", "")
	if w.Code != 200 || body["window"] != "30d" {
		t.Fatalf("%d %v", w.Code, body)
	}
	a := body["agents"].([]any)[0].(map[string]any)
	if a["cost_per_success_usd"].(float64) != 0.25 || a["definition"] != "outcome" || st.tenants[0] != "tenant-a" {
		t.Errorf("%v", a)
	}
	if w, _ := send(t, h, "GET", "/api/economics?window=2d", "tok-a", ""); w.Code != 400 {
		t.Errorf("bad window: %d", w.Code)
	}
}
