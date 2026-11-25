// Package outcomes is outcome-graded quality (feature "outcome_grading"):
// business events reported after a run, without its trace ID, found through
// a correlation key the agent recorded (bindle.correlate), and reversal
// outcomes that make a run count as wrong even when every step passed.
//
// Ingest accepts events (POST /v1/outcome-events, or a signed webhook) and
// stores them in the control plane as tenant-bound correlation hashes. The
// matcher (in the alerts worker) links each event to the runs that carry the
// same hashes, retrying with backoff while the run may still be arriving,
// and marks events that never match as unmatched. Nothing is dropped.
package outcomes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bindle-ai/bindle/backend/internal/correlate"
	"github.com/bindle-ai/bindle/backend/internal/redact"
)

// Feature is the workspace flag that turns outcome grading on.
const Feature = "outcome_grading"

// Event statuses.
const (
	StatusPending   = "pending"
	StatusMatched   = "matched"
	StatusUnmatched = "unmatched"
)

// Limits.
const (
	MaxNameLen       = 80
	MaxCommentLen    = 500
	MaxReversals     = 50
	DefaultPending   = 24  // hours an unmatched event keeps being retried
	MaxPending       = 168 // 7 days
	MatchLookback    = 30 * 24 * time.Hour
	maxFutureSkew    = 5 * time.Minute
	MaxBodyBytes     = 16 << 10
	SignatureMaxSkew = 5 * time.Minute
)

// Event is one reported business outcome.
type Event struct {
	ID            string     `json:"id"`
	TenantID      string     `json:"-"`
	Correlations  []string   `json:"correlation_keys"` // stored form; only the key part is shown
	Name          string     `json:"name"`
	Success       bool       `json:"success"`
	Value         *float64   `json:"value,omitempty"`
	Comment       string     `json:"comment,omitempty"`
	Source        string     `json:"source"` // "api" or "webhook"
	EndpointID    string     `json:"endpoint_id,omitempty"`
	OccurredAt    time.Time  `json:"occurred_at"`
	ReceivedAt    time.Time  `json:"received_at"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	MatchedTraces []string   `json:"matched_traces"`
	MatchedAt     *time.Time `json:"matched_at,omitempty"`
}

// MarshalJSON shows correlation keys by name only (the hashes are an
// implementation detail and useless to a person).
func (e Event) MarshalJSON() ([]byte, error) {
	type alias Event
	a := alias(e)
	a.Correlations = KeyNames(e.Correlations)
	if a.MatchedTraces == nil {
		a.MatchedTraces = []string{}
	}
	return json.Marshal(a)
}

// KeyNames returns the key part of stored correlations, sorted and unique.
func KeyNames(stored []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range stored {
		k, _, _ := strings.Cut(s, ":")
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Settings are a workspace's outcome settings.
type Settings struct {
	ReversalOutcomes []string  `json:"reversal_outcomes"`
	PendingHours     int       `json:"pending_hours"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
}

// DefaultSettings applies to workspaces that never saved settings.
func DefaultSettings() Settings {
	return Settings{ReversalOutcomes: []string{}, PendingHours: DefaultPending}
}

// IsReversal reports whether name is one of the settings' reversal outcomes.
func (s Settings) IsReversal(name string) bool {
	for _, r := range s.ReversalOutcomes {
		if r == name {
			return true
		}
	}
	return false
}

// ValidName reports whether n may name an outcome: 1-80 characters without
// control characters or surrounding spaces.
func ValidName(n string) bool {
	if n == "" || utf8.RuneCountInString(n) > MaxNameLen || strings.TrimSpace(n) != n || !utf8.ValidString(n) {
		return false
	}
	return strings.IndexFunc(n, unicode.IsControl) < 0
}

// NormalizeSettings validates settings and returns them cleaned (reversal
// names unique and sorted).
func NormalizeSettings(s Settings) (Settings, error) {
	if s.PendingHours == 0 {
		s.PendingHours = DefaultPending
	}
	if s.PendingHours < 1 || s.PendingHours > MaxPending {
		return Settings{}, fmt.Errorf("pending_hours must be between 1 and %d", MaxPending)
	}
	if len(s.ReversalOutcomes) > MaxReversals {
		return Settings{}, fmt.Errorf("at most %d reversal outcomes", MaxReversals)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, n := range s.ReversalOutcomes {
		if !ValidName(n) {
			return Settings{}, fmt.Errorf("reversal outcome %q must be 1-%d characters without control characters or surrounding spaces", n, MaxNameLen)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	s.ReversalOutcomes = out
	return s, nil
}

// report is the request body of POST /v1/outcome-events.
type report struct {
	Correlation map[string]json.RawMessage `json:"correlation"`
	Name        string                     `json:"name"`
	Success     *bool                      `json:"success"`
	Value       *float64                   `json:"value"`
	Comment     string                     `json:"comment"`
	OccurredAt  string                     `json:"occurred_at"`
}

// ErrInvalid wraps request validation errors (HTTP 400).
var ErrInvalid = errors.New("invalid outcome event")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Parse validates a report and turns it into an event for tenant. Raw
// correlation values are hashed immediately and never kept; the comment is
// redacted. The ID depends only on the tenant and the report's content, so a
// retried or replayed request is the same event.
func Parse(tenant string, body []byte, settings Settings, now time.Time, source, endpointID string) (Event, error) {
	var r report
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Event{}, invalid("body must be a JSON object with correlation, name and success")
	}
	if len(r.Correlation) == 0 || len(r.Correlation) > correlate.MaxKeys {
		return Event{}, invalid("correlation must have 1-%d keys", correlate.MaxKeys)
	}
	if !ValidName(r.Name) {
		return Event{}, invalid("name must be 1-%d characters without control characters or surrounding spaces", MaxNameLen)
	}
	if r.Success == nil {
		return Event{}, invalid("success is required (true or false)")
	}
	if r.Value != nil && (math.IsNaN(*r.Value) || math.IsInf(*r.Value, 0)) {
		return Event{}, invalid("value must be a finite number")
	}
	ev := Event{TenantID: tenant, Name: r.Name, Success: *r.Success, Value: r.Value, Source: source, EndpointID: endpointID,
		ReceivedAt: now, Status: StatusPending, NextAttemptAt: now, MatchedTraces: []string{}}
	keys := make([]string, 0, len(r.Correlation))
	for k := range r.Correlation {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !correlate.ValidKey(k) {
			return Event{}, invalid("correlation key %q must be lowercase letters, digits and _, starting with a letter (at most 32)", k)
		}
		v, ok := correlationValue(r.Correlation[k])
		if !ok || !correlate.ValidValue(v) {
			return Event{}, invalid("correlation %s must be a string of 1-%d bytes or an integer", k, correlate.MaxValueLen)
		}
		ev.Correlations = append(ev.Correlations, correlate.FromValue(tenant, k, v))
	}
	if c := strings.TrimSpace(r.Comment); c != "" {
		c = redact.Default().Apply(c)
		if utf8.RuneCountInString(c) > MaxCommentLen {
			c = string([]rune(c)[:MaxCommentLen])
		}
		ev.Comment = c
	}
	ev.OccurredAt = now
	if r.OccurredAt != "" {
		t, err := time.Parse(time.RFC3339Nano, r.OccurredAt)
		if err != nil {
			return Event{}, invalid("occurred_at must be an RFC 3339 time")
		}
		if t.Before(now.Add(-MatchLookback)) || t.After(now.Add(maxFutureSkew)) {
			return Event{}, invalid("occurred_at must be within the last 30 days")
		}
		ev.OccurredAt = t.UTC()
	}
	hours := settings.PendingHours
	if hours < 1 || hours > MaxPending {
		hours = DefaultPending
	}
	ev.ExpiresAt = now.Add(time.Duration(hours) * time.Hour)
	ev.ID = eventID(tenant, ev, r.OccurredAt)
	return ev, nil
}

func correlationValue(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var n json.Number
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&n); err == nil {
		if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
			return strconv.FormatInt(i, 10), true
		}
	}
	return "", false
}

func eventID(tenant string, ev Event, occurredAt string) string {
	var b strings.Builder
	b.WriteString(tenant)
	for _, c := range ev.Correlations {
		b.WriteString("\x00" + c)
	}
	b.WriteString("\x00" + ev.Name + "\x00" + strconv.FormatBool(ev.Success))
	if ev.Value != nil {
		b.WriteString("\x00" + strconv.FormatFloat(*ev.Value, 'g', -1, 64))
	} else {
		b.WriteString("\x00-")
	}
	b.WriteString("\x00" + ev.Comment + "\x00" + occurredAt)
	h := sha256.Sum256([]byte(b.String()))
	return "oev_" + hex.EncodeToString(h[:16])
}

// Backoff is how long the matcher waits before the next attempt for an
// event that matched no run yet: quick at first (the run's spans may still
// be in flight), then at most hourly.
func Backoff(attempts int) time.Duration {
	steps := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute}
	if attempts < len(steps) {
		return steps[attempts]
	}
	return time.Hour
}
