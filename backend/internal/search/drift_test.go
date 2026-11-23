package search

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

func TestPassportDriftRequestAndParse(t *testing.T) {
	f, c := newFake(t)
	// Filters buckets come back in the order of driftKinds.
	f.answers["/_search"] = `{"aggregations":{"agents":{"buckets":[{"key":"refunds",
	  "reasons":{"buckets":[{"key":"tool_not_allowed:refund_payment","doc_count":3},{"key":"egress_blocked:CARD","doc_count":1}]},
	  "kinds":{"buckets":[
	    {"doc_count":3,"runs":{"value":2},"last":{"value":1763294400000000000},"examples":{"hits":{"hits":[{"_source":{"trace_id":"t1","step_id":"s1","name":"execute_tool refund_payment","tool_name":"refund_payment","passport_drift":["egress_blocked:CARD","tool_not_allowed:refund_payment"]}}]}}},
	    {"doc_count":0,"runs":{"value":0},"last":{"value":null},"examples":{"hits":{"hits":[]}}},
	    {"doc_count":1,"runs":{"value":1},"last":{"value":1763294400000000000},"examples":{"hits":{"hits":[]}}},
	    {"doc_count":0,"runs":{"value":0},"last":{"value":null},"examples":{"hits":{"hits":[]}}}]}}]}}}`
	s := &Store{Client: c}
	got, err := s.PassportDrift(context.Background(), "tenant-1", t0, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != "tool_not_allowed" || got[0].Steps != 3 || got[0].Runs != 2 || got[1].Kind != "egress_blocked" {
		t.Fatalf("drift = %+v", got)
	}
	if !reflect.DeepEqual(got[0].Reasons, map[string]uint64{"tool_not_allowed:refund_payment": 3}) ||
		!reflect.DeepEqual(got[0].Examples[0].Reasons, []string{"tool_not_allowed:refund_payment"}) {
		t.Fatalf("reasons = %+v", got[0])
	}
	body := string(f.all()[0].Body)
	if !strings.Contains(body, `"exists":{"field":"passport_drift"}`) || !strings.Contains(body, `"prefix":{"passport_drift":"egress_blocked:"}`) {
		t.Fatalf("request = %s", body)
	}
	if _, err := s.PassportDrift(context.Background(), "", t0, t0); !errors.Is(err, query.ErrNoTenant) {
		t.Fatalf("no tenant: %v", err)
	}
}

func TestToolCallsRequest(t *testing.T) {
	f, c := newFake(t)
	f.answers["/_search"] = `{"hits":{"total":{"value":7},"hits":[{"_source":{"trace_id":"t9"}}]}}`
	s := &Store{Client: c}
	n, trace, err := s.ToolCalls(context.Background(), "tenant-1", "refunds", "refund_payment", t0, t0.Add(5*time.Minute))
	if err != nil || n != 7 || trace != "t9" {
		t.Fatalf("calls %d %q %v", n, trace, err)
	}
	body := string(f.all()[0].Body)
	for _, want := range []string{`"term":{"tool_name":"refund_payment"}`, `"term":{"agent_name":"refunds"}`, `"term":{"type":"` + model.StepTool + `"}`, `"track_total_hits":true`} {
		if !strings.Contains(body, want) {
			t.Fatalf("request lacks %s: %s", want, body)
		}
	}
	if _, _, err := s.ToolCalls(context.Background(), "tenant-1", "refunds", "crm_?*", t0, t0); err != nil {
		t.Fatal(err)
	}
	if body := string(f.all()[1].Body); !strings.Contains(body, `"wildcard":{"tool_name":{"value":"crm_\\?*"}}`) {
		t.Fatalf("wildcard request = %s", body)
	}
}
