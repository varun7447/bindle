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

// signalStore adds security signals and topics to the fake store.
type signalStore struct {
	*fakeStore
	agent    string
	from, to time.Time
}

func (s *signalStore) SecuritySignals(_ context.Context, t string, from, to time.Time) ([]query.SecuritySignal, error) {
	s.rec(t)
	s.from, s.to = from, to
	return []query.SecuritySignal{{Flag: "prompt_injection", Agent: "support", Steps: 2, Runs: 1,
		Reasons: map[string]uint64{"prompt_injection:ignore_instructions": 2}, Examples: []query.SecurityExample{}}}, nil
}

func (s *signalStore) Topics(_ context.Context, t, agent string, from, to time.Time) (*query.Topics, error) {
	s.rec(t)
	s.agent, s.from, s.to = agent, from, to
	return &query.Topics{Rising: []query.Topic{{Term: "refund", Runs: 9}}, New: []query.Topic{}, Runs: 10}, nil
}

func TestSecurityAndTopics(t *testing.T) {
	st := &signalStore{fakeStore: &fakeStore{}}
	srv := &Server{Store: st, Sessions: tokenResolver{"tok-a": "tenant-a"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return now }}
	h := srv.Handler()

	w, body := get(t, h, "/api/security?window=24h", "tok-a")
	if w.Code != 200 || body["signals"].([]any)[0].(map[string]any)["flag"] != "prompt_injection" || st.to.Sub(st.from) != 24*time.Hour {
		t.Fatalf("security %d %v", w.Code, body)
	}
	if body["explanations"].(map[string]any)["prompt_injection:ignore_instructions"] == "" {
		t.Fatalf("no explanations: %v", body)
	}
	w, body = get(t, h, "/api/agents/topics?agent=support", "tok-a")
	if w.Code != 200 || st.agent != "support" || body["window"] != "7d" ||
		body["topics"].(map[string]any)["rising"].([]any)[0].(map[string]any)["term"] != "refund" {
		t.Fatalf("topics %d %v", w.Code, body)
	}
	if w, _ := get(t, h, "/api/agents/topics?agent="+strings.Repeat("a", 300), "tok-a"); w.Code != 400 {
		t.Errorf("long agent: %d", w.Code)
	}
	if w, _ := get(t, h, "/api/security?window=1y", "tok-a"); w.Code != 400 {
		t.Errorf("bad window: %d", w.Code)
	}
	if w, _ := get(t, h, "/api/security", ""); w.Code != 401 {
		t.Errorf("no token: %d", w.Code)
	}
	for _, tn := range st.tenants {
		if tn != "tenant-a" {
			t.Fatalf("tenant %q", tn)
		}
	}

	// A store without signals answers 503.
	h = newServer(&fakeStore{})
	if w, _ := get(t, h, "/api/security", "tok-a"); w.Code != 503 {
		t.Errorf("no signal store: %d", w.Code)
	}
}
