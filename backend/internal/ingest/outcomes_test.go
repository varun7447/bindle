package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/auth"
	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/outcomes"
)

type fakeOutcomes struct {
	events map[string]outcomes.Event
}

func (f *fakeOutcomes) Settings(context.Context, string) (outcomes.Settings, error) {
	return outcomes.DefaultSettings(), nil
}
func (f *fakeOutcomes) EndpointSecret(_ context.Context, id string) (string, string, bool, error) {
	if id == "oep_0123456789abcdef" {
		return "acme", "obsec_test", true, nil
	}
	return "", "", false, nil
}
func (f *fakeOutcomes) Record(_ context.Context, ev outcomes.Event) (outcomes.Event, bool, error) {
	if old, ok := f.events[ev.ID]; ok {
		return old, false, nil
	}
	f.events[ev.ID] = ev
	return ev, true, nil
}
func (f *fakeOutcomes) Status(_ context.Context, tenant, id string) (outcomes.Event, error) {
	ev, ok := f.events[id]
	if !ok || ev.TenantID != tenant {
		return outcomes.Event{}, control.ErrNotFound
	}
	return ev, nil
}

type flags map[string]bool

func (f flags) Enabled(_ context.Context, tenant, feature string) bool { return f[tenant+"/"+feature] }

func newOutcomeHandler(t *testing.T, on bool) (*OutcomeHandler, *fakeOutcomes) {
	keys, _ := auth.ParseEnv("bd_test_good=acme,bd_test_other=globex")
	store := &fakeOutcomes{events: map[string]outcomes.Event{}}
	f := flags{"acme/" + outcomes.Feature: on, "globex/" + outcomes.Feature: true}
	return &OutcomeHandler{Keys: keys, Store: store, Features: f, Now: func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }}, store
}

func do(h http.Handler, method, path, key, body string, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

const event = `{"correlation":{"ticket_id":"T-123"},"name":"ticket_reopened","success":false}`

func TestOutcomeEventsAPI(t *testing.T) {
	h, store := newOutcomeHandler(t, true)
	w, out := do(h, http.MethodPost, "/v1/outcome-events", "bd_test_good", event, nil)
	if w.Code != http.StatusAccepted || out["pending"] != true || out["status"] != "pending" {
		t.Fatalf("POST = %d %v", w.Code, out)
	}
	id := out["id"].(string)
	if len(store.events) != 1 || strings.Contains(strings.Join(store.events[id].Correlations, ","), "T-123") {
		t.Fatalf("stored %+v", store.events)
	}
	// A retry is the same event.
	if _, again := do(h, http.MethodPost, "/v1/outcome-events", "bd_test_good", event, nil); again["id"] != id || len(store.events) != 1 {
		t.Fatalf("retry created a second event: %v", again)
	}
	ev := store.events[id]
	ev.Status, ev.MatchedTraces = outcomes.StatusMatched, []string{"t1", "t2"}
	store.events[id] = ev
	if w, st := do(h, http.MethodGet, "/v1/outcome-events/"+id, "bd_test_good", "", nil); w.Code != 200 || st["matched"] != float64(2) {
		t.Fatalf("status = %d %v", w.Code, st)
	}
	// Another tenant can't see it.
	if w, _ := do(h, http.MethodGet, "/v1/outcome-events/"+id, "bd_test_other", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant status = %d", w.Code)
	}
	for _, c := range []struct {
		method, path, key, body string
		code                    int
	}{
		{http.MethodPost, "/v1/outcome-events", "", event, http.StatusUnauthorized},
		{http.MethodPost, "/v1/outcome-events", "bd_test_good", `{"name":"x"}`, http.StatusBadRequest},
		{http.MethodGet, "/v1/outcome-events", "bd_test_good", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/outcome-events/nope", "bd_test_good", "", http.StatusNotFound},
		{http.MethodPost, "/v1/outcome-events", "bd_test_good", `{"correlation":{"a":"` + strings.Repeat("x", 20000) + `"}}`, http.StatusRequestEntityTooLarge},
	} {
		if w, _ := do(h, c.method, c.path, c.key, c.body, nil); w.Code != c.code {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, w.Code, c.code)
		}
	}
}

func TestOutcomeEventsNeedTheFeature(t *testing.T) {
	h, store := newOutcomeHandler(t, false)
	if w, _ := do(h, http.MethodPost, "/v1/outcome-events", "bd_test_good", event, nil); w.Code != http.StatusNotFound || len(store.events) != 0 {
		t.Fatalf("feature off: %d", w.Code)
	}
}

func TestOutcomeWebhook(t *testing.T) {
	h, store := newOutcomeHandler(t, true)
	path := "/v1/outcome-events/webhook/oep_0123456789abcdef"
	sig := outcomes.Sign("obsec_test", []byte(event), h.Now())
	w, out := do(h, http.MethodPost, path, "", event, map[string]string{outcomes.SignatureHeader: sig})
	if w.Code != http.StatusAccepted || out["pending"] != true {
		t.Fatalf("webhook = %d %v", w.Code, out)
	}
	for _, ev := range store.events {
		if ev.TenantID != "acme" || ev.Source != "webhook" || ev.EndpointID != "oep_0123456789abcdef" {
			t.Fatalf("event %+v", ev)
		}
	}
	// Replay: accepted, still one event.
	if w, _ := do(h, http.MethodPost, path, "", event, map[string]string{outcomes.SignatureHeader: sig}); w.Code != http.StatusAccepted || len(store.events) != 1 {
		t.Fatalf("replay = %d, events %d", w.Code, len(store.events))
	}
	bad := map[string]map[string]string{
		"wrong secret": {outcomes.SignatureHeader: outcomes.Sign("obsec_other", []byte(event), h.Now())},
		"stale":        {outcomes.SignatureHeader: outcomes.Sign("obsec_test", []byte(event), h.Now().Add(-10*time.Minute))},
		"missing":      {},
	}
	for name, hdr := range bad {
		if w, _ := do(h, http.MethodPost, path, "", event, hdr); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w, _ := do(h, http.MethodPost, "/v1/outcome-events/webhook/oep_ffffffffffffffff", "", event, map[string]string{outcomes.SignatureHeader: sig}); w.Code != http.StatusNotFound {
		t.Errorf("unknown endpoint: %d", w.Code)
	}
}
