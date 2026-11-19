// Package alerts evaluates per-tenant alert rules against trace data and
// delivers fired alerts to Slack, webhooks or email. Alerts never carry
// prompt or output content: only counts, tool names and a console link.
package alerts

import (
	"context"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/query"
)

// Rule kinds.
const (
	KindErrorRate   = "error_rate"   // threshold: failed-run fraction (0-1]
	KindRunawayLoop = "runaway_loop" // threshold: repeats of the same tool+input in a trace
	KindRunCost     = "run_cost"     // threshold: USD for one run
	// threshold: share (0-1] of user feedback that is negative; min_runs: feedback needed
	KindNegativeFeedback = "negative_feedback"
	// threshold: cost-per-run increase (0.25 = +25%) of an agent's newest
	// version over the previous one; min_runs: runs needed per version
	KindCostRegression = "cost_regression"
	// threshold: failures of an error pattern not seen in the 7 days before
	KindNewError = "new_error"
	// a Monday summary of the last 7 days; threshold and window are fixed
	KindWeeklyDigest = "weekly_digest"
	// threshold: steps with the same security flag (prompt injection, an
	// exposed credential, possible exfiltration) for one agent in the window
	KindSecuritySignal = "security_signal"
)

// Rule is one alert condition.
type Rule struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"-"`
	Kind          string    `json:"kind"`
	Threshold     float64   `json:"threshold"`
	WindowMinutes int       `json:"window_minutes"`
	MinRuns       int       `json:"min_runs"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
}

// Channel is a delivery destination.
type Channel struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"-"`
	Kind      string    `json:"kind"` // slack | webhook | email
	Name      string    `json:"name"`
	Target    string    `json:"-"` // secret-bearing: never returned by the API
	Display   string    `json:"target_display"`
	Secret    string    `json:"-"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// Event is a fired alert.
type Event struct {
	ID          string            `json:"id"`
	TenantID    string            `json:"-"`
	RuleID      string            `json:"rule_id"`
	Kind        string            `json:"kind"`
	Fingerprint string            `json:"-"`
	Summary     string            `json:"summary"`
	TraceID     string            `json:"trace_id,omitempty"`
	Details     map[string]any    `json:"details"`
	Delivery    map[string]string `json:"delivery"`
	FiredAt     time.Time         `json:"fired_at"`
}

// InvalidError is a validation failure whose message is safe to show.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &InvalidError{Msg: fmt.Sprintf(format, a...)} }

// DefaultRules are created with every new workspace.
func DefaultRules() []Rule {
	return []Rule{
		{Kind: KindRunawayLoop, Threshold: 5, WindowMinutes: 60, Enabled: true},
		{Kind: KindErrorRate, Threshold: 0.2, WindowMinutes: 60, MinRuns: 20, Enabled: true},
	}
}

// ValidateRule checks a rule's settings.
func ValidateRule(r Rule) error {
	if r.WindowMinutes < 5 || r.WindowMinutes > 1440 {
		return invalid("window must be 5 to 1440 minutes")
	}
	if r.MinRuns < 0 || r.MinRuns > 100000 {
		return invalid("min_runs must be 0 to 100000")
	}
	switch r.Kind {
	case KindErrorRate:
		if r.Threshold <= 0 || r.Threshold > 1 {
			return invalid("error-rate threshold must be above 0 and at most 1")
		}
	case KindRunawayLoop:
		if r.Threshold < 2 || r.Threshold > 10000 || r.Threshold != float64(int(r.Threshold)) {
			return invalid("loop threshold must be a whole number of repeats from 2 to 10000")
		}
	case KindRunCost:
		if r.Threshold <= 0 || r.Threshold > 1e6 {
			return invalid("cost threshold must be above $0 and at most $1,000,000")
		}
	case KindNegativeFeedback:
		if r.Threshold <= 0 || r.Threshold > 1 {
			return invalid("negative-feedback threshold must be above 0 and at most 1")
		}
	case KindCostRegression:
		if r.Threshold <= 0 || r.Threshold > 100 {
			return invalid("cost-regression threshold must be above 0 (0.25 = 25%% more per run)")
		}
	case KindNewError:
		if r.Threshold < 1 || r.Threshold > 100000 || r.Threshold != float64(int(r.Threshold)) {
			return invalid("new-error threshold must be a whole number of failures, at least 1")
		}
	case KindSecuritySignal:
		if r.Threshold < 1 || r.Threshold > 100000 || r.Threshold != float64(int(r.Threshold)) {
			return invalid("security-signal threshold must be a whole number of flagged steps, at least 1")
		}
	case KindWeeklyDigest:
		if r.Threshold != 1 || r.WindowMinutes != 1440 {
			return invalid("the weekly digest has no threshold or window to set")
		}
	default:
		return invalid("unknown rule kind %q", r.Kind)
	}
	return nil
}

var slackPathRe = regexp.MustCompile(`^/services/[A-Za-z0-9]+/[A-Za-z0-9]+/[A-Za-z0-9]+$`)

// ValidateChannel checks a channel's target. Network-level checks (no
// private addresses) happen again at connect time in the delivery client.
func ValidateChannel(c Channel) error {
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 100 {
		return invalid("name must be 1-100 characters")
	}
	switch c.Kind {
	case "slack":
		u, err := url.Parse(c.Target)
		if err != nil || u.Scheme != "https" || u.Host != "hooks.slack.com" || !slackPathRe.MatchString(u.Path) || u.RawQuery != "" {
			return invalid("use a Slack incoming-webhook URL (https://hooks.slack.com/services/...)")
		}
	case "webhook":
		u, err := url.Parse(c.Target)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(c.Target) > 2000 {
			return invalid("webhook URL must be https, without credentials")
		}
		if ip := parseHostIP(u.Hostname()); ip != nil && !publicIP(ip) {
			return invalid("webhook URL must not point to a private or local address")
		}
		if h := strings.ToLower(u.Hostname()); h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".internal") {
			return invalid("webhook URL must not point to a private or local address")
		}
	case "email":
		a, err := mail.ParseAddress(c.Target)
		if err != nil || a.Address != c.Target || len(c.Target) > 254 {
			return invalid("enter a single email address")
		}
	default:
		return invalid("unknown channel kind %q", c.Kind)
	}
	return nil
}

// DisplayTarget is what the console shows for a target: enough to
// recognise it, not enough to use it.
func DisplayTarget(kind, target string) string {
	switch kind {
	case "slack":
		if len(target) > 12 {
			return "hooks.slack.com/…" + target[len(target)-4:]
		}
		return "hooks.slack.com/…"
	case "webhook":
		if u, err := url.Parse(target); err == nil {
			return u.Scheme + "://" + u.Host + "/…"
		}
		return "…"
	}
	return target
}

// Store is the control plane as the evaluator sees it.
type Store interface {
	AlertingTenants(ctx context.Context) ([]string, error)
	Rules(ctx context.Context, tenant string) ([]Rule, error)
	Channels(ctx context.Context, tenant string) ([]Channel, error)
	// RecordEvent inserts the event unless its fingerprint fired already;
	// it reports whether this call created it (and so must deliver it).
	RecordEvent(ctx context.Context, ev Event) (bool, error)
	SetDelivery(ctx context.Context, tenant, eventID string, d map[string]string) error
}

// Detector reads trace data (search.Store).
type Detector interface {
	Insights(ctx context.Context, tenant string, from, to time.Time) (*query.Insights, error)
	CostByAgent(ctx context.Context, tenant string, from, to time.Time) (map[string]float64, error)
	AgentVersions(ctx context.Context, tenant, agent string, from, to time.Time) ([]query.VersionStats, error)
	RecentFeedback(ctx context.Context, tenant string, from, to time.Time) (map[string]query.FeedbackSummary, error)
	ErrorPatterns(ctx context.Context, tenant string, from, to time.Time) ([]query.ErrorPatternCount, error)
	Overview(ctx context.Context, tenant string, from, to time.Time, bucket time.Duration) (*query.Overview, error)
	Loops(ctx context.Context, tenant string, from, to time.Time, minRepeats int) ([]query.Loop, error)
	CostlyRuns(ctx context.Context, tenant string, from, to time.Time, minCostUSD float64, limit int) ([]query.Run, error)
}
