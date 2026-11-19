package search

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/query"
)

func TestTraceLinks(t *testing.T) {
	a, b, c := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	sp := strings.Repeat("1", 16)
	f, cl := newFake(t)
	// The fake answers every step search with the same hits; TraceLinks must
	// sort them into outgoing and incoming by trace ID.
	f.answers["-steps-*/_search"] = `{"hits":{"hits":[
	  {"_source":{"trace_id":"` + a + `","step_id":"` + sp + `","name":"resume approval","links":["` + b + `:` + sp + `","` + a + `:` + sp + `","garbage"],"attributes":{"bindle.link.kind":"resumed"}}},
	  {"_source":{"trace_id":"` + c + `","step_id":"` + sp + `","name":"billing agent","links":["` + a + `:` + sp + `"],"attributes":{"bindle.link.kind":"HANDOFF"}}}
	]}}`
	f.answers["-traces/_search"] = `{"hits":{"hits":[{"_source":{"trace_id":"` + b + `","agent_name":"triage","first_start_ns":1}}]}}`

	s := &Store{Client: cl}
	got, err := s.TraceLinks(context.Background(), "tenant-1", a, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d links: %+v", len(got), got)
	}
	out, in := got[0], got[1]
	if out.Direction != "outgoing" || out.Kind != query.LinkResumed || out.TraceID != b || out.Run == nil || out.Run.Agent != "triage" {
		t.Errorf("outgoing = %+v", out)
	}
	if in.Direction != "incoming" || in.Kind != query.LinkHandoff || in.TraceID != c || in.Run != nil {
		t.Errorf("incoming = %+v", in)
	}

	reqs := f.all()
	if len(reqs) != 3 {
		t.Fatalf("%d requests", len(reqs))
	}
	for _, r := range reqs {
		if !strings.Contains(r.Query, "routing=tenant-1") || !strings.Contains(string(r.Body), `{"term":{"tenant_id":"tenant-1"}}`) {
			t.Errorf("not tenant scoped: %s %s", r.Query, r.Body)
		}
		if strings.Contains(string(r.Body), `"input"`) {
			t.Errorf("link lookup reads payload fields: %s", r.Body)
		}
	}
	var incoming struct {
		Query struct {
			Bool struct {
				Filter []map[string]any `json:"filter"`
			} `json:"bool"`
		} `json:"query"`
	}
	if err := json.Unmarshal(reqs[1].Body, &incoming); err != nil {
		t.Fatal(err)
	}
	p, _ := json.Marshal(incoming.Query.Bool.Filter[1])
	if string(p) != `{"prefix":{"links":"`+a+`:"}}` {
		t.Errorf("incoming query = %s", p)
	}
}

func TestTraceLinksNone(t *testing.T) {
	f, cl := newFake(t)
	got, err := (&Store{Client: cl}).TraceLinks(context.Background(), "tenant-1", strings.Repeat("a", 32), 10)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
	if n := len(f.all()); n != 2 {
		t.Errorf("%d requests; no run lookup expected without links", n)
	}
}

func TestParseLinkAndKind(t *testing.T) {
	if _, _, ok := query.ParseLink(strings.Repeat("a", 32) + ":" + strings.Repeat("b", 16)); !ok {
		t.Error("valid link rejected")
	}
	for _, bad := range []string{"", "x:y", strings.Repeat("A", 32) + ":" + strings.Repeat("b", 16), strings.Repeat("a", 32) + strings.Repeat("b", 16)} {
		if _, _, ok := query.ParseLink(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if query.LinkKind(" Called ") != query.LinkCalled || query.LinkKind("other") != "" {
		t.Error("LinkKind")
	}
}
