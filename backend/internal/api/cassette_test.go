package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/query"
)

// stepsByID answers GetStep per step ID.
type stepsByID struct {
	*fakeStore
	details map[string]*query.StepDetail
}

func (f *stepsByID) GetStep(_ context.Context, t, _, step string) (*query.StepDetail, error) {
	f.rec(t)
	return f.details[step], nil
}

func TestCassette(t *testing.T) {
	started := query.Time{Time: time.Date(2026, 11, 16, 11, 0, 0, 0, time.UTC)}
	steps := []query.StepSummary{
		{StepID: "1000000000000001", Name: "invoke_agent", Type: "agent", Status: "success"},
		{StepID: "1000000000000002", Name: "chat", Type: "llm_call", Status: "success"},
		{StepID: "1000000000000003", Name: "execute_tool lookup_order", Type: "tool_call", ToolName: "lookup_order", Status: "success"},
		{StepID: "1000000000000004", Name: "refund", Type: "tool_call", Status: "failure", ErrorMessage: "timeout"},
	}
	st := &stepsByID{
		fakeStore: &fakeStore{trace: &query.Trace{Run: query.Run{TraceID: traceID, Started: started, Agent: "refunds", AgentVersion: "v2"}, Steps: steps}},
		details: map[string]*query.StepDetail{
			"1000000000000003": {Input: `{"order_id":"A1"}`, Output: `{"status":"shipped"}`},
			"1000000000000004": {Input: `{"order_id":"A1"}`, TruncatedFields: []string{"input"}},
		},
	}
	h := (&Server{Store: st, Sessions: tokenResolver{"tok-a": "tenant-a"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()

	r := httptest.NewRequest("GET", "/api/traces/"+traceID+"/cassette", nil)
	r.Header.Set("Authorization", "Bearer tok-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="run-`+traceID+`.cassette.json"` {
		t.Errorf("content-disposition: %q", cd)
	}
	var c Cassette
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if c.Format != "bindle.cassette" || c.Version != 1 || c.TraceID != traceID || c.Agent != "refunds" || c.AgentVersion != "v2" {
		t.Fatalf("header: %+v", c)
	}
	if c.Output != "refund issued for "+traceID || c.RecordedAt != "2026-11-16T11:00:00Z" {
		t.Fatalf("run fields: %+v", c)
	}
	if len(c.ToolCalls) != 2 {
		t.Fatalf("tool calls: %+v", c.ToolCalls)
	}
	first, second := c.ToolCalls[0], c.ToolCalls[1]
	if first.Tool != "lookup_order" || first.Arguments != `{"order_id":"A1"}` || first.Result != `{"status":"shipped"}` || first.Status != "success" {
		t.Errorf("first call: %+v", first)
	}
	if second.Tool != "refund" || second.Status != "failure" || second.Error != "timeout" || second.Result != "" {
		t.Errorf("second call (tool name from step name, failure kept): %+v", second)
	}
	if len(c.Incomplete) != 1 || c.Incomplete[0] != "content_truncated" {
		t.Errorf("incomplete: %v", c.Incomplete)
	}
	for _, tn := range st.tenants {
		if tn != "tenant-a" {
			t.Fatalf("store called for tenant %q", tn)
		}
	}

	// Unknown trace: 404; bad ID: 400.
	st.trace = nil
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Errorf("missing trace: %d", w.Code)
	}
	bad := httptest.NewRequest("GET", "/api/traces/xyz/cassette", nil)
	bad.Header.Set("Authorization", "Bearer tok-a")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, bad)
	if w.Code != 400 {
		t.Errorf("bad id: %d", w.Code)
	}
}
