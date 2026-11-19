package api

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/query"
)

type linkFake struct {
	*fakeStore
	gotTenant, gotTrace string
	links               []query.TraceLink
}

func (f *linkFake) TraceLinks(_ context.Context, tenant, traceID string, _ int) ([]query.TraceLink, error) {
	f.gotTenant, f.gotTrace = tenant, traceID
	return f.links, f.err
}

func TestTraceLinksEndpoint(t *testing.T) {
	other := strings.Repeat("b", 32)
	st := &linkFake{fakeStore: &fakeStore{}, links: []query.TraceLink{
		{Direction: "outgoing", Kind: query.LinkResumed, StepID: stepID, StepName: "resume", TraceID: other, SpanID: stepID,
			Run: &query.Run{TraceID: other, Agent: "triage"}},
		{Direction: "incoming", Kind: query.LinkHandoff, TraceID: strings.Repeat("c", 32), SpanID: stepID},
	}}
	h := (&Server{Store: st, Sessions: tokenResolver{"tok-a": "tenant-a"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return now }}).Handler()

	w, body := get(t, h, "/api/traces/"+strings.ToUpper(traceID)+"/links", "tok-a")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if st.gotTenant != "tenant-a" || st.gotTrace != traceID {
		t.Errorf("store got tenant=%q trace=%q", st.gotTenant, st.gotTrace)
	}
	links, _ := body["links"].([]any)
	if len(links) != 2 {
		t.Fatalf("links = %v", body["links"])
	}
	first := links[0].(map[string]any)
	run, _ := first["run"].(map[string]any)
	if first["kind"] != "resumed" || first["direction"] != "outgoing" || run["agent"] != "triage" {
		t.Errorf("first = %v", first)
	}
	if second := links[1].(map[string]any); second["run"] != nil {
		t.Errorf("missing run should be null: %v", second)
	}

	if w, _ := get(t, h, "/api/traces/nothex/links", "tok-a"); w.Code != 400 {
		t.Errorf("bad id: %d", w.Code)
	}
	if w, _ := get(t, h, "/api/traces/"+traceID+"/links", ""); w.Code != 401 {
		t.Errorf("no auth: %d", w.Code)
	}
}

func TestTraceLinksWithoutLinkStore(t *testing.T) {
	w, body := get(t, newServer(&fakeStore{}), "/api/traces/"+traceID+"/links", "tok-a")
	if w.Code != 200 || len(body["links"].([]any)) != 0 {
		t.Fatalf("%d %v", w.Code, body)
	}
}
