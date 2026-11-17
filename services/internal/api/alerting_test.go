package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/bindle-ai/bindle/services/internal/alerts"
	"github.com/bindle-ai/bindle/services/internal/control"
)

type fakeAlerting struct {
	tenants  []string
	channels map[string]alerts.Channel
}

func (f *fakeAlerting) saw(t string) { f.tenants = append(f.tenants, t) }

func (f *fakeAlerting) ListAlertRules(_ context.Context, t string) ([]alerts.Rule, error) {
	f.saw(t)
	return alerts.DefaultRules(), nil
}
func (f *fakeAlerting) CreateAlertRule(_ context.Context, t, _ string, r alerts.Rule) (alerts.Rule, error) {
	f.saw(t)
	if err := alerts.ValidateRule(r); err != nil {
		return alerts.Rule{}, err
	}
	r.ID = "ar_00000000000000000001"
	return r, nil
}
func (f *fakeAlerting) UpdateAlertRule(_ context.Context, t, id string, r alerts.Rule) (alerts.Rule, error) {
	f.saw(t)
	return alerts.Rule{}, control.ErrNotFound
}
func (f *fakeAlerting) ListAlertChannels(_ context.Context, t string) ([]alerts.Channel, error) {
	f.saw(t)
	var out []alerts.Channel
	for _, c := range f.channels {
		out = append(out, c)
	}
	return out, nil
}
func (f *fakeAlerting) GetAlertChannel(_ context.Context, t, id string) (alerts.Channel, error) {
	f.saw(t)
	c, ok := f.channels[id]
	if !ok || c.TenantID != t {
		return alerts.Channel{}, control.ErrNotFound
	}
	return c, nil
}
func (f *fakeAlerting) CreateAlertChannel(_ context.Context, t, _ string, c alerts.Channel) (alerts.Channel, string, error) {
	f.saw(t)
	if err := alerts.ValidateChannel(c); err != nil {
		return alerts.Channel{}, "", err
	}
	c.ID, c.TenantID = "ch_00000000000000000001", t
	f.channels[c.ID] = c
	return c, "whsec_abc", nil
}
func (f *fakeAlerting) SetAlertChannelEnabled(_ context.Context, t, _ string, _ bool) error {
	f.saw(t)
	return nil
}
func (f *fakeAlerting) DeleteAlertChannel(_ context.Context, t, _ string) error {
	f.saw(t)
	return nil
}
func (f *fakeAlerting) ListAlertEvents(_ context.Context, t string, _ int) ([]alerts.Event, error) {
	f.saw(t)
	return nil, nil
}

func alertingServer(f *fakeAlerting, d *alerts.Deliverer) http.Handler {
	return (&Server{Store: &fakeStore{}, Alerting: f, Deliver: d,
		Sessions: tokenResolver{"tok-a": "tenant-a", "member-tok": "tenant-a", "tok-b": "tenant-b"},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
}

const chID = "ch_00000000000000000001"

func TestAlertingEndpoints(t *testing.T) {
	f := &fakeAlerting{channels: map[string]alerts.Channel{}}
	h := alertingServer(f, &alerts.Deliverer{})
	if w, body := send(t, h, "GET", "/api/alert-rules", "member-tok", ""); w.Code != 200 || len(body["rules"].([]any)) != 2 {
		t.Fatalf("list rules %d %v", w.Code, body)
	}
	if w, _ := send(t, h, "POST", "/api/alert-rules", "member-tok", `{"kind":"run_cost","threshold":5,"window_minutes":60,"enabled":true}`); w.Code != 403 {
		t.Fatalf("member created a rule: %d", w.Code)
	}
	if w, _ := send(t, h, "POST", "/api/alert-rules", "tok-a", `{"kind":"run_cost","threshold":5,"window_minutes":60,"enabled":true}`); w.Code != 201 {
		t.Fatalf("create rule %d", w.Code)
	}
	w, body := send(t, h, "POST", "/api/alert-rules", "tok-a", `{"kind":"error_rate","threshold":3,"window_minutes":60,"enabled":true}`)
	if w.Code != 400 || !strings.Contains(body["error"].(map[string]any)["message"].(string), "error-rate threshold") {
		t.Fatalf("invalid rule %d %v", w.Code, body)
	}
	if w, _ := send(t, h, "POST", "/api/alert-rules", "tok-a", `{"kind":"run_cost","threshold":5,"window_minutes":60,"enabled":true,"tenant_id":"tenant-b"}`); w.Code != 400 {
		t.Fatalf("tenant_id in body accepted: %d", w.Code)
	}
	if w, _ := send(t, h, "PUT", "/api/alert-rules/ar_00000000000000000009", "tok-a", `{"threshold":5,"window_minutes":60,"enabled":true}`); w.Code != 404 {
		t.Fatalf("update missing %d", w.Code)
	}
	if w, _ := send(t, h, "PUT", "/api/alert-rules/../x", "tok-a", `{}`); w.Code == 200 {
		t.Fatal("bad id accepted")
	}

	w, body = send(t, h, "POST", "/api/alert-channels", "tok-a", `{"kind":"webhook","name":"ops","target":"https://example.com/hook"}`)
	if w.Code != 201 || body["signing_secret"] != "whsec_abc" {
		t.Fatalf("create channel %d %v", w.Code, body)
	}
	if w, _ := send(t, h, "POST", "/api/alert-channels", "tok-a", `{"kind":"webhook","name":"x","target":"https://169.254.169.254/"}`); w.Code != 400 {
		t.Fatalf("metadata URL accepted: %d", w.Code)
	}
	w, body = send(t, h, "GET", "/api/alert-channels", "tok-a", "")
	raw := w.Body.String()
	if w.Code != 200 || strings.Contains(raw, "example.com/hook") || strings.Contains(raw, "whsec") {
		t.Fatalf("channel list leaks target or secret: %s", raw)
	}
	if w, _ := send(t, h, "PUT", "/api/alert-channels/"+chID+"/state", "tok-a", `{"enabled":false}`); w.Code != 204 {
		t.Fatalf("state %d", w.Code)
	}
	if w, _ := send(t, h, "DELETE", "/api/alert-channels/"+chID, "member-tok", ""); w.Code != 403 {
		t.Fatalf("member deleted: %d", w.Code)
	}
	// Tenant B can't test-send tenant A's channel.
	if w, _ := send(t, h, "POST", "/api/alert-channels/"+chID+"/test", "tok-b", ""); w.Code != 404 {
		t.Fatalf("cross-tenant test %d", w.Code)
	}
	if w, _ := send(t, h, "GET", "/api/alert-events", "tok-a", ""); w.Code != 200 {
		t.Fatalf("events %d", w.Code)
	}
	for _, tn := range f.tenants {
		if tn != "tenant-a" && tn != "tenant-b" {
			t.Fatalf("tenant %q", tn)
		}
	}
}

func TestAlertingTestSendReportsSafeError(t *testing.T) {
	f := &fakeAlerting{channels: map[string]alerts.Channel{
		chID: {ID: chID, TenantID: "tenant-a", Kind: "email", Name: "e", Target: "ops@example.com"},
	}}
	h := alertingServer(f, &alerts.Deliverer{})
	w, body := send(t, h, "POST", "/api/alert-channels/"+chID+"/test", "tok-a", "")
	if w.Code != 200 || body["ok"] != false || body["error"] != "email delivery is not configured on this deployment" {
		t.Fatalf("%d %v", w.Code, body)
	}
}

func TestAlertingUnconfigured(t *testing.T) {
	h := keysServer(&fakeKeys{})
	if w, _ := send(t, h, "GET", "/api/alert-rules", "tok-a", ""); w.Code != 503 {
		t.Fatalf("%d", w.Code)
	}
}
