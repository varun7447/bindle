package search

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLessonGroupsAndAgentTools(t *testing.T) {
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		if bytes.Contains(b, []byte(`"patterns"`)) {
			_, _ = io.WriteString(w, `{"aggregations":{"agents":{"buckets":[{"key":"refund","steps":{"buckets":[{"key":"lookup_order",
				"patterns":{"buckets":[{"key":"order <N> not found","doc_count":7,"runs":{"value":6},"first":{"value":1.7e18},"last":{"value":1.8e18},
				"tool":{"buckets":[{"key":"lookup_order"}]},"stype":{"buckets":[{"key":"tool_call"}]},
				"traces":{"buckets":[{"key":"0123456789abcdef0123456789abcdef"}]},
				"samples":{"hits":{"hits":[{"_source":{"trace_id":"0123456789abcdef0123456789abcdef","step_id":"0123456789abcdef","type":"tool_call",
				"name":"lookup_order","tool_name":"lookup_order","start_ns":1800000000000000000,"input":"{}","output":"not found"}}]}}}]}}]}}]}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"aggregations":{"agents":{"buckets":[{"key":"refund","tools":{"buckets":[{"key":"lookup_order"},{"key":"send_email"}]}}]}}}`)
	}))
	defer srv.Close()
	s := &Store{Client: &Client{URL: srv.URL, User: "reader", Password: "pw"}}
	now := time.Unix(0, 1.8e18)
	gs, err := s.LessonGroups(context.Background(), "t1", now.Add(-time.Hour), now)
	if err != nil || len(gs) != 1 {
		t.Fatalf("groups %+v %v", gs, err)
	}
	g := gs[0]
	if g.Agent != "refund" || g.Tool != "lookup_order" || g.Runs != 6 || g.Failures != 7 || len(g.Samples) != 1 || len(g.Traces) != 1 {
		t.Fatalf("group %+v", g)
	}
	for _, want := range []string{`"status":"failure"`, `"tenant_id":"t1"`, `"error_pattern"`} {
		if !strings.Contains(string(bodies[0]), want) {
			t.Errorf("query lacks %s: %s", want, bodies[0])
		}
	}
	tools, err := s.AgentToolNames(context.Background(), "t1", now.Add(-time.Hour), now)
	if err != nil || strings.Join(tools["refund"], ",") != "lookup_order,send_email" {
		t.Fatalf("tools %v %v", tools, err)
	}
	if !strings.Contains(string(bodies[1]), `"type":"tool_call"`) || !strings.Contains(string(bodies[1]), `"tenant_id":"t1"`) {
		t.Errorf("tools query %s", bodies[1])
	}
}
