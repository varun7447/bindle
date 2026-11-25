// Package passport is an agent's machine-readable manifest of what it may
// do: which tools, which data classes, how much it may spend, how often it
// may call a tool, and whether it may release changes. Bindle compares each
// step with the agent's passport and reports drift. It never blocks
// anything: passports are observe-only.
//
// Passports are JSON documents with version "bindle.passport/v1". The
// schema is in schema/bindle.passport.v1.schema.json; Parse is the
// authoritative validator.
package passport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Version is the only passport version this package reads.
const Version = "bindle.passport/v1"

// Feature is the per-workspace flag that turns passports on.
const Feature = "passports"

// MaxBytes caps a passport document.
const MaxBytes = 64 << 10

// Release rights.
const (
	ReleaseNone    = "none"
	ReleaseRequest = "request"
	ReleaseFull    = "full"
)

// Passport is one agent's manifest. Absent sections mean "not declared":
// Bindle doesn't check what a passport doesn't say. An empty list is a
// declaration ("tools": {"allow": []} allows no tools).
type Passport struct {
	Version       string       `json:"version"`
	Agent         string       `json:"agent"`
	Owner         string       `json:"owner"`
	Identity      *Identity    `json:"identity,omitempty"`
	AutonomyTier  *int         `json:"autonomy_tier,omitempty"`
	Tools         *Tools       `json:"tools,omitempty"`
	DataClasses   *DataClasses `json:"data_classes,omitempty"`
	Budget        *Budget      `json:"budget,omitempty"`
	Limits        []Limit      `json:"limits,omitempty"`
	ReleaseRights string       `json:"release_rights,omitempty"`
	ReleaseTools  []string     `json:"release_tools,omitempty"`
}

// Identity is who the agent acts as.
type Identity struct {
	Kind string `json:"kind"` // iam_role, oauth_client or service
	ID   string `json:"id"`
}

// Tools lists the tool names the agent may call. Patterns may use * as a
// wildcard ("crm_*", "mcp:orders/*").
type Tools struct {
	Allow []string `json:"allow"`
}

// DataClasses are redaction labels: built-in (EMAIL, CARD, ...) or the
// workspace's own pattern names.
type DataClasses struct {
	// Allow is what the agent may handle; nil (absent or null) means not
	// declared, and [] allows none.
	Allow []string `json:"allow"`
	// EgressBlock must never reach a tool that sends data out.
	EgressBlock []string `json:"egress_block,omitempty"`
}

// Budget caps priced spend. Zero means no cap.
type Budget struct {
	PerRunUSD float64 `json:"per_run_usd,omitempty"`
	PerDayUSD float64 `json:"per_day_usd,omitempty"`
}

// Limit caps how often a tool is called in a sliding window.
type Limit struct {
	Tool   string `json:"tool"`
	Max    int64  `json:"max"`
	Window string `json:"window"` // e.g. "30s", "5m", "1h"; 1s to 24h
}

// Duration is the window as a time.Duration (Parse has validated it).
func (l Limit) Duration() time.Duration {
	d, _ := parseWindow(l.Window)
	return d
}

// Limits on what a passport can declare.
const (
	maxAgentLen    = 128
	maxOwnerLen    = 256
	maxIdentityLen = 256
	maxPatterns    = 200
	maxLimits      = 50
	maxBudgetUSD   = 1_000_000
	maxPerWindow   = 1_000_000
	maxWindow      = 24 * time.Hour
)

var (
	patternRe = regexp.MustCompile(`^[A-Za-z0-9_.:/@*-]{1,128}$`)
	labelRe   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)
	windowRe  = regexp.MustCompile(`^([1-9][0-9]{0,4})(s|m|h)$`)
)

// ValidationError is a problem with a passport document, safe to show.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ValidationError{Msg: fmt.Sprintf(format, a...)} }

// IsInvalid reports whether err is a validation error.
func IsInvalid(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}

// Parse reads and validates a passport. knownLabels are the data-class
// labels the workspace has (built-in plus its own redaction patterns);
// unknown labels are rejected so a typo can't silently disable a check.
func Parse(data []byte, knownLabels map[string]bool) (*Passport, error) {
	if len(data) > MaxBytes {
		return nil, invalid("a passport must be at most %d KiB", MaxBytes>>10)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Passport
	if err := dec.Decode(&p); err != nil {
		return nil, invalid("not a valid passport: %s", jsonError(err))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid("not a valid passport: unexpected data after the JSON object")
	}
	if err := p.validate(knownLabels); err != nil {
		return nil, err
	}
	return &p, nil
}

func jsonError(err error) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		return fmt.Sprintf("JSON syntax error at byte %d", se.Offset)
	case errors.As(err, &te):
		return fmt.Sprintf("field %q must be %s", te.Field, te.Type.String())
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "json: unknown field ") {
		return "unknown field " + strings.TrimPrefix(msg, "json: unknown field ")
	}
	return "invalid JSON"
}

func (p *Passport) validate(known map[string]bool) error {
	if p.Version != Version {
		return invalid("version must be %q", Version)
	}
	if !cleanText(p.Agent, maxAgentLen) {
		return invalid("agent must be 1-%d characters without control characters or surrounding spaces", maxAgentLen)
	}
	if !cleanText(p.Owner, maxOwnerLen) {
		return invalid("owner must be 1-%d characters (a person, team or email)", maxOwnerLen)
	}
	if p.Identity != nil {
		switch p.Identity.Kind {
		case "iam_role", "oauth_client", "service":
		default:
			return invalid("identity.kind must be iam_role, oauth_client or service")
		}
		if !cleanText(p.Identity.ID, maxIdentityLen) {
			return invalid("identity.id must be 1-%d characters", maxIdentityLen)
		}
	}
	if p.AutonomyTier != nil && (*p.AutonomyTier < 0 || *p.AutonomyTier > 3) {
		return invalid("autonomy_tier must be 0, 1, 2 or 3")
	}
	if p.Tools != nil {
		if p.Tools.Allow == nil {
			return invalid(`tools.allow is required when tools is present (use [] to allow no tools)`)
		}
		if err := patterns("tools.allow", p.Tools.Allow); err != nil {
			return err
		}
	}
	if d := p.DataClasses; d != nil {
		if err := labels("data_classes.allow", d.Allow, known); err != nil {
			return err
		}
		if err := labels("data_classes.egress_block", d.EgressBlock, known); err != nil {
			return err
		}
	}
	if b := p.Budget; b != nil {
		for name, v := range map[string]float64{"budget.per_run_usd": b.PerRunUSD, "budget.per_day_usd": b.PerDayUSD} {
			if v != v || v < 0 || v > maxBudgetUSD {
				return invalid("%s must be between 0 and %d", name, maxBudgetUSD)
			}
		}
	}
	if len(p.Limits) > maxLimits {
		return invalid("at most %d limits", maxLimits)
	}
	for i, l := range p.Limits {
		if !patternRe.MatchString(l.Tool) {
			return invalid("limits[%d].tool must be a tool name or pattern of up to 128 characters (letters, digits, _ . : / @ - and *)", i)
		}
		if l.Max < 1 || l.Max > maxPerWindow {
			return invalid("limits[%d].max must be between 1 and %d", i, maxPerWindow)
		}
		if _, err := parseWindow(l.Window); err != nil {
			return invalid("limits[%d].window %s", i, err.Error())
		}
	}
	switch p.ReleaseRights {
	case "", ReleaseNone, ReleaseRequest, ReleaseFull:
	default:
		return invalid("release_rights must be none, request or full")
	}
	if err := patterns("release_tools", p.ReleaseTools); err != nil {
		return err
	}
	return nil
}

func cleanText(s string, max int) bool {
	return s != "" && len(s) <= max && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0
}

func patterns(field string, list []string) error {
	if len(list) > maxPatterns {
		return invalid("%s: at most %d entries", field, maxPatterns)
	}
	for i, v := range list {
		if !patternRe.MatchString(v) {
			return invalid("%s[%d] must be a tool name or pattern of up to 128 characters (letters, digits, _ . : / @ - and * as a wildcard)", field, i)
		}
	}
	return nil
}

func labels(field string, list []string, known map[string]bool) error {
	if len(list) > maxPatterns {
		return invalid("%s: at most %d labels", field, maxPatterns)
	}
	for i, v := range list {
		if !labelRe.MatchString(v) {
			return invalid("%s[%d] must be a data-class label such as EMAIL or CARD", field, i)
		}
		if known != nil && !known[v] {
			return invalid("%s[%d]: %s is not a built-in data class or one of this workspace's redaction patterns", field, i, v)
		}
	}
	return nil
}

func parseWindow(s string) (time.Duration, error) {
	m := windowRe.FindStringSubmatch(s)
	if m == nil {
		return 0, errors.New(`must be a number with s, m or h, e.g. "30s", "5m" or "1h"`)
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour}[m[2]]
	d := time.Duration(n) * unit
	if d > maxWindow {
		return 0, errors.New("must be at most 24h")
	}
	return d, nil
}

// Match reports whether name matches pattern, where * matches any run of
// characters (including none) and everything else is literal.
func Match(pattern, name string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(name, mid)
		if i < 0 {
			return false
		}
		name = name[i+len(mid):]
	}
	return strings.HasSuffix(name, last)
}

// MatchAny reports whether name matches any pattern.
func MatchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if Match(p, name) {
			return true
		}
	}
	return false
}
