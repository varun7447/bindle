package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/bindle-ai/bindle/services/internal/alerts"
	"github.com/bindle-ai/bindle/services/internal/control"
)

// AlertingStore manages alert rules, channels and history.
// Implemented by *control.Store.
type AlertingStore interface {
	ListAlertRules(ctx context.Context, tenant string) ([]alerts.Rule, error)
	CreateAlertRule(ctx context.Context, tenant, userID string, r alerts.Rule) (alerts.Rule, error)
	UpdateAlertRule(ctx context.Context, tenant, id string, r alerts.Rule) (alerts.Rule, error)
	ListAlertChannels(ctx context.Context, tenant string) ([]alerts.Channel, error)
	GetAlertChannel(ctx context.Context, tenant, id string) (alerts.Channel, error)
	CreateAlertChannel(ctx context.Context, tenant, userID string, c alerts.Channel) (alerts.Channel, string, error)
	SetAlertChannelEnabled(ctx context.Context, tenant, id string, enabled bool) error
	DeleteAlertChannel(ctx context.Context, tenant, id string) error
	ListAlertEvents(ctx context.Context, tenant string, limit int) ([]alerts.Event, error)
}

var (
	ruleIDRe     = regexp.MustCompile(`^ar_[0-9a-f]{20}$`)
	channelIDRe  = regexp.MustCompile(`^ch_[0-9a-f]{20}$`)
	httpStatusRe = regexp.MustCompile(`^HTTP \d{3}$`)
)

func (s *Server) alertingAllowed(w http.ResponseWriter, r *http.Request, change bool) bool {
	if s.Alerting == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "alerting is not configured")
		return false
	}
	if change && !principalFrom(r.Context()).CanManageKeys() {
		writeError(w, http.StatusForbidden, "forbidden", "only owners and admins can change alerts")
		return false
	}
	return true
}

// alertingError maps store errors: validation messages are ours and safe.
func (s *Server) alertingError(w http.ResponseWriter, r *http.Request, err error, what string) {
	var inv *alerts.InvalidError
	switch {
	case errors.As(err, &inv):
		badRequest(w, inv.Msg)
	case errors.Is(err, control.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", what+" not found")
	default:
		s.storeError(w, r, err)
	}
}

func (s *Server) listAlertRules(w http.ResponseWriter, r *http.Request) {
	if !s.alertingAllowed(w, r, false) {
		return
	}
	rules, err := s.Alerting.ListAlertRules(r.Context(), tenantFrom(r.Context()))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if rules == nil {
		rules = []alerts.Rule{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

type ruleBody struct {
	Kind          string   `json:"kind"`
	Threshold     *float64 `json:"threshold"`
	WindowMinutes *int     `json:"window_minutes"`
	MinRuns       int      `json:"min_runs"`
	Enabled       *bool    `json:"enabled"`
}

func (b ruleBody) rule() (alerts.Rule, bool) {
	if b.Threshold == nil || b.WindowMinutes == nil || b.Enabled == nil {
		return alerts.Rule{}, false
	}
	return alerts.Rule{Kind: b.Kind, Threshold: *b.Threshold, WindowMinutes: *b.WindowMinutes, MinRuns: b.MinRuns, Enabled: *b.Enabled}, true
}

func (s *Server) createAlertRule(w http.ResponseWriter, r *http.Request) {
	if !s.alertingAllowed(w, r, true) {
		return
	}
	var body ruleBody
	if !decodeBody(w, r, &body) {
		return
	}
	rule, ok := body.rule()
	if !ok {
		badRequest(w, "kind, threshold, window_minutes and enabled are required")
		return
	}
	p := principalFrom(r.Context())
	out, err := s.Alerting.CreateAlertRule(r.Context(), p.TenantID, p.UserID, rule)
	if err != nil {
		s.alertingError(w, r, err, "rule")
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) updateAlertRule(w http.ResponseWriter, r *http.Request) {
	if !s.alertingAllowed(w, r, true) {
		return
	}
	id := r.PathValue("id")
	if !ruleIDRe.MatchString(id) {
		badRequest(w, "invalid rule id")
		return
	}
	var body ruleBody
	if !decodeBody(w, r, &body) {
		return
	}
	rule, ok := body.rule()
	if !ok {
		badRequest(w, "threshold, window_minutes and enabled are required")
		return
	}
	out, err := s.Alerting.UpdateAlertRule(r.Context(), tenantFrom(r.Context()), id, rule)
	if err != nil {
		s.alertingError(w, r, err, "rule")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listAlertChannels(w http.ResponseWriter, r *http.Request) {
	if !s.alertingAllowed(w, r, false) {
		return
	}
	chs, err := s.Alerting.ListAlertChannels(r.Context(), tenantFrom(r.Context()))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if chs == nil {
		chs = []alerts.Channel{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": chs}) // Target and Secret are not serialized
}

func (s *Server) createAlertChannel(w http.ResponseWriter, r *http.Request) {
	if !s.alertingAllowed(w, r, true) {
		return
	}
	var body struct {
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Target string `json:"target"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	p := principalFrom(r.Context())
	ch, secret, err := s.Alerting.CreateAlertChannel(r.Context(), p.TenantID, p.UserID, alerts.Channel{Kind: body.Kind, Name: body.Name, Target: body.Target})
	if err != nil {
		s.alertingError(w, r, err, "channel")
		return
	}
	resp := map[string]any{"channel": ch}
	if secret != "" {
		resp["signing_secret"] = secret // shown once
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) channelID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !channelIDRe.MatchString(id) {
		badRequest(w, "invalid channel id")
		return "", false
	}
	return id, true
}

func (s *Server) setAlertChannelState(w http.ResponseWriter, r *http.Request) {
	if !s.alertingAllowed(w, r, true) {
		return
	}
	id, ok := s.channelID(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		badRequest(w, "enabled is required")
		return
	}
	if err := s.Alerting.SetAlertChannelEnabled(r.Context(), tenantFrom(r.Context()), id, *body.Enabled); err != nil {
		s.alertingError(w, r, err, "channel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAlertChannel(w http.ResponseWriter, r *http.Request) {
	if !s.alertingAllowed(w, r, true) {
		return
	}
	id, ok := s.channelID(w, r)
	if !ok {
		return
	}
	if err := s.Alerting.DeleteAlertChannel(r.Context(), tenantFrom(r.Context()), id); err != nil {
		s.alertingError(w, r, err, "channel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testAlertChannel sends a sample alert so users can check the wiring.
func (s *Server) testAlertChannel(w http.ResponseWriter, r *http.Request) {
	if !s.alertingAllowed(w, r, true) {
		return
	}
	if s.Deliver == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "test delivery is not configured")
		return
	}
	id, ok := s.channelID(w, r)
	if !ok {
		return
	}
	ch, err := s.Alerting.GetAlertChannel(r.Context(), tenantFrom(r.Context()), id)
	if err != nil {
		s.alertingError(w, r, err, "channel")
		return
	}
	ev := alerts.Event{ID: "evt_test", Kind: "test", Summary: "Test alert from Bindle. If you can read this, the channel works.", FiredAt: s.now()}
	if err := s.Deliver.Send(r.Context(), ch, ev, alerts.Rule{}); err != nil {
		s.logger().Warn("test alert failed", "tenant", ch.TenantID, "channel", ch.ID, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": testFailure(err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// testFailure reduces a delivery error to a message that doesn't echo
// internal details (resolved addresses, TLS internals).
func testFailure(err error) string {
	msg := err.Error()
	switch {
	case httpStatusRe.MatchString(msg):
		return "the receiver answered " + msg
	case strings.Contains(msg, "non-public address"):
		return "the URL resolves to a private or local address"
	case strings.Contains(msg, "redirects"):
		return "the receiver answered with a redirect; use the final URL"
	case strings.Contains(msg, "not configured"):
		return "email delivery is not configured on this deployment"
	}
	return "the receiver could not be reached"
}

func (s *Server) listAlertEvents(w http.ResponseWriter, r *http.Request) {
	if !s.alertingAllowed(w, r, false) {
		return
	}
	evs, err := s.Alerting.ListAlertEvents(r.Context(), tenantFrom(r.Context()), 100)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if evs == nil {
		evs = []alerts.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}
