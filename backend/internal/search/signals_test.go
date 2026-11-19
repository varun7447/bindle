package search

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

func TestSecuritySignals(t *testing.T) {
	f, c := newFake(t)
	f.answers["/_search"] = `{"aggregations":{"flags":{"buckets":[
	 {"key":"prompt_injection","agents":{"buckets":[{"key":"support","doc_count":4,"runs":{"value":3},"last":{"value":1763294400000000000},
	   "reasons":{"buckets":[{"key":"prompt_injection:ignore_instructions","doc_count":3},{"key":"secret_exposed:credential_in_output","doc_count":1}]},
	   "examples":{"hits":{"hits":[{"_source":{"trace_id":"t1","step_id":"s1","name":"execute_tool fetch","tool_name":"fetch","security_reasons":["prompt_injection:ignore_instructions","secret_exposed:credential_in_output"]}}]}}}]}},
	 {"key":"secret_exposed","agents":{"buckets":[{"key":"","doc_count":9,"runs":{"value":9},"last":{"value":null},"reasons":{"buckets":[]},"examples":{"hits":{"hits":[]}}}]}}
	]}}}`
	s := &Store{Client: c}
	got, err := s.SecuritySignals(context.Background(), "tenant-1", t0, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Flag != "secret_exposed" || got[0].Steps != 9 || got[1].Agent != "support" || got[1].Runs != 3 {
		t.Fatalf("signals = %+v", got)
	}
	inj := got[1]
	// Reasons of other flags on the same steps are not counted under this flag.
	if !reflect.DeepEqual(inj.Reasons, map[string]uint64{"prompt_injection:ignore_instructions": 3}) ||
		len(inj.Examples) != 1 || !reflect.DeepEqual(inj.Examples[0].Reasons, []string{"prompt_injection:ignore_instructions"}) {
		t.Fatalf("injection = %+v", inj)
	}
	body := string(f.all()[0].Body)
	if !strings.Contains(body, `"exists":{"field":"security_flags"}`) || !strings.Contains(body, `"field":"security_flags"`) {
		t.Fatalf("request = %s", body)
	}
}

func TestTopicsRequestAndParse(t *testing.T) {
	f, c := newFake(t)
	// One answer for both requests: the candidates (and period totals),
	// then exact run counts per candidate.
	f.answers["/_search"] = `{"aggregations":{
	 "cur":{"runs":{"value":100},"chars":{"value":84.5},"sample":{"terms":{"buckets":[{"key":"refund"},{"key":"outage"},{"key":"order"},{"key":"rare"}]}}},
	 "prev":{"runs":{"value":50},"chars":{"value":null}},
	 "topics":{"buckets":{
	   "refund":{"cur":{"runs":{"value":40},"examples":{"hits":{"hits":[{"_source":{"trace_id":"a"}},{"_source":{"trace_id":"a"}},{"_source":{"trace_id":"b"}}]}}},"prev":{"runs":{"value":5}}},
	   "outage":{"cur":{"runs":{"value":12},"examples":{"hits":{"hits":[{"_source":{"trace_id":"c"}}]}}},"prev":{"runs":{"value":0}}},
	   "order":{"cur":{"runs":{"value":60},"examples":{"hits":{"hits":[]}}},"prev":{"runs":{"value":30}}},
	   "rare":{"cur":{"runs":{"value":2},"examples":{"hits":{"hits":[]}}},"prev":{"runs":{"value":0}}}}}}}`
	s := &Store{Client: c}
	from := t0
	got, err := s.Topics(context.Background(), "tenant-1", "support", from, from.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.Runs != 100 || got.PreviousRuns != 50 || got.AvgChars != 84.5 || got.PrevAvgChars != 0 {
		t.Fatalf("totals = %+v", got)
	}
	// refund: 40% of runs now, 10% before. order: the same 60% share, so
	// steady. rare: too few runs.
	if len(got.Rising) != 1 || got.Rising[0].Term != "refund" || got.Rising[0].Share != 0.4 || got.Rising[0].PreviousShare != 0.1 ||
		!reflect.DeepEqual(got.Rising[0].Examples, []string{"a", "b"}) {
		t.Fatalf("rising = %+v", got.Rising)
	}
	if len(got.New) != 1 || got.New[0].Term != "outage" || !reflect.DeepEqual(got.New[0].Examples, []string{"c"}) {
		t.Fatalf("new = %+v", got.New)
	}
	reqs := f.all()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want the candidates and their counts", len(reqs))
	}
	if cnt := string(reqs[1].Body); !strings.Contains(cnt, `"match":{"user_input":"refund"}`) || !strings.Contains(cnt, `"term":{"tenant_id":"tenant-1"}`) {
		t.Fatalf("count request = %s", cnt)
	}

	// The proposal's background is the earlier period, pinned to the
	// tenant and agent too.
	var req struct {
		Aggs struct {
			Cur struct {
				Aggs struct {
					Sample struct {
						Sampler map[string]any `json:"diversified_sampler"`
						Aggs    struct {
							Terms struct {
								Sig struct {
									Field      string `json:"field"`
									Background struct {
										Bool struct {
											Filter []map[string]any `json:"filter"`
										} `json:"bool"`
									} `json:"background_filter"`
								} `json:"significant_text"`
							} `json:"terms"`
						} `json:"aggs"`
					} `json:"sample"`
				} `json:"aggs"`
			} `json:"cur"`
		} `json:"aggs"`
	}
	if !strings.Contains(reqs[0].Query, "routing=tenant-1") {
		t.Fatalf("not routed: %s", reqs[0].Query)
	}
	if err := json.Unmarshal(reqs[0].Body, &req); err != nil {
		t.Fatal(err)
	}
	smp := req.Aggs.Cur.Aggs.Sample
	if smp.Sampler["field"] != "trace_id" || smp.Sampler["max_docs_per_value"] != float64(1) || smp.Aggs.Terms.Sig.Field != "user_input" {
		t.Fatalf("sampler = %+v", smp)
	}
	bg, _ := json.Marshal(smp.Aggs.Terms.Sig.Background.Bool.Filter)
	want := `[{"term":{"tenant_id":"tenant-1"}},{"exists":{"field":"user_input"}},{"term":{"agent_name":"support"}},{"range":{"start_ns":{"gte":` +
		itoa(from.Add(-24*time.Hour).UnixNano()) + `,"lt":` + itoa(from.UnixNano()) + `}}}]`
	if string(bg) != want {
		t.Fatalf("background = %s\nwant %s", bg, want)
	}
}

func TestTopicsNeedAnEarlierPeriod(t *testing.T) {
	f, c := newFake(t)
	f.answers["/_search"] = `{"aggregations":{"cur":{"runs":{"value":10},"sample":{"terms":{"buckets":[{"key":"refund"}]}}},"prev":{"runs":{"value":0}}}}`
	got, err := (&Store{Client: c}).Topics(context.Background(), "tenant-1", "", t0, t0.Add(time.Hour))
	if err != nil || got.Runs != 10 || len(got.New)+len(got.Rising) != 0 || len(f.all()) != 1 {
		t.Fatalf("%v %+v (%d requests)", err, got, len(f.all()))
	}
	if strings.Contains(string(f.all()[0].Body), "agent_name") {
		t.Fatal("agent filter without an agent")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestSignalsNeedATenant(t *testing.T) {
	f, c := newFake(t)
	s := &Store{Client: c}
	if _, err := s.SecuritySignals(context.Background(), "", t0, t0); !errors.Is(err, query.ErrNoTenant) {
		t.Fatal(err)
	}
	if _, err := s.Topics(context.Background(), "", "", t0, t0); !errors.Is(err, query.ErrNoTenant) {
		t.Fatal(err)
	}
	if len(f.all()) != 0 {
		t.Fatal("request sent without a tenant")
	}
}

func TestDocCarriesSignalsAndUserInput(t *testing.T) {
	d := toDoc(model.Step{TenantID: "t", TraceID: "a", StepID: "b", UserInput: "where is my order",
		SecurityFlags: []string{"prompt_injection"}, SecurityReasons: []string{"prompt_injection:chat_markup"}}, "now")
	b, _ := json.Marshal(d)
	for _, want := range []string{`"user_input":"where is my order"`, `"user_input_bytes":17`, `"security_flags":["prompt_injection"]`, `"security_reasons":["prompt_injection:chat_markup"]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("doc lacks %s: %s", want, b)
		}
	}
	// Every field the writer sends is in the strict mapping.
	props := stepsMapping()["properties"].(map[string]any)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for k := range m {
		if _, ok := props[k]; !ok {
			t.Errorf("field %s is not mapped", k)
		}
	}
	if s := d.summary(); !reflect.DeepEqual(s.SecurityFlags, []string{"prompt_injection"}) {
		t.Errorf("summary flags = %v", s.SecurityFlags)
	}
}
