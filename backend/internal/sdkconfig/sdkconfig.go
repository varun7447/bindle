// Package sdkconfig is a tenant's remote SDK configuration: sampling,
// content capture and extra redaction patterns. The console edits it, the
// SDK polls it from ingest (GET /v1/config), and ingest enforces it too, so
// it also applies to apps the SDK cannot control.
//
// Remote settings are a ceiling: code may only be stricter (lower sample
// rate, capture off, more patterns). Built-in redaction cannot be disabled.
package sdkconfig

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/cost"
)

// Pattern is a named extra redaction rule. Matches become [REDACTED:NAME].
type Pattern struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
}

// Config is what the SDK receives. Version increases on every change.
type Config struct {
	Version        int64     `json:"version"`
	SampleRate     float64   `json:"sample_rate"`
	CaptureContent bool      `json:"capture_content"`
	RedactPatterns []Pattern `json:"redact_patterns"`
	// Limits are edited separately from the fields above (PUT /api/limits).
	Limits []Limit `json:"limits"`
	// OverBudget lists agents whose daily budget was exceeded today (UTC).
	// Set by Bindle; read-only. Their limit's PauseOverBudget decides
	// whether they are paused.
	OverBudget []string `json:"over_budget"`
	// Prices are the workspace's model price overrides. Used by ingest;
	// never sent to the SDK.
	Prices    []PriceOverride `json:"-"`
	UpdatedAt time.Time       `json:"updated_at,omitempty"`
}

// Default applies to tenants that never changed their settings.
func Default() Config {
	return Config{SampleRate: 1, CaptureContent: true, RedactPatterns: []Pattern{}, Limits: []Limit{}, OverBudget: []string{}}
}

const (
	MaxPatterns      = 20
	MaxPatternLength = 200
	PollSeconds      = 60 // suggested SDK poll interval
)

var nameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)

// Validate checks a config before it is saved. Patterns must compile as
// RE2 (linear time, used by ingest) and must not nest repetition, which
// would let Python's backtracking engine in the SDK blow up on hostile
// input; this rejects the classic catastrophic forms such as (a+)+.
func Validate(c Config) error {
	if math.IsNaN(c.SampleRate) || c.SampleRate < 0 || c.SampleRate > 1 {
		return errors.New("sample_rate must be between 0 and 1")
	}
	if len(c.RedactPatterns) > MaxPatterns {
		return fmt.Errorf("at most %d redaction patterns", MaxPatterns)
	}
	seen := map[string]bool{}
	for i, p := range c.RedactPatterns {
		if !nameRe.MatchString(p.Name) {
			return fmt.Errorf("pattern %d: name must be 1-32 characters of A-Z, 0-9 and _, starting with a letter", i+1)
		}
		if seen[p.Name] {
			return fmt.Errorf("pattern %d: duplicate name %s", i+1, p.Name)
		}
		seen[p.Name] = true
		if err := ValidatePattern(p.Pattern); err != nil {
			return fmt.Errorf("pattern %s: %w", p.Name, err)
		}
	}
	return nil
}

// ValidatePattern checks one regular expression.
func ValidatePattern(expr string) error {
	if expr == "" || len(expr) > MaxPatternLength {
		return fmt.Errorf("must be 1-%d characters", MaxPatternLength)
	}
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return fmt.Errorf("invalid regular expression: %v", err)
	}
	if nestedRepeat(re, false) {
		return errors.New("nested repetition such as (a+)+ is not allowed")
	}
	if re.Simplify().MaxCap() > 9 {
		return errors.New("too many capture groups")
	}
	if m, err := regexp.Compile(expr); err != nil {
		return fmt.Errorf("invalid regular expression: %v", err)
	} else if m.MatchString("") {
		return errors.New("pattern must not match empty text")
	}
	return nil
}

func nestedRepeat(re *syntax.Regexp, inRepeat bool) bool {
	isRepeat := false
	switch re.Op {
	case syntax.OpStar, syntax.OpPlus, syntax.OpQuest, syntax.OpRepeat:
		// x? and x{0,1} cannot cause blow-up on their own.
		isRepeat = re.Op != syntax.OpQuest && !(re.Op == syntax.OpRepeat && re.Max == 1)
		if isRepeat && inRepeat {
			return true
		}
	}
	for _, sub := range re.Sub {
		if nestedRepeat(sub, inRepeat || isRepeat) {
			return true
		}
	}
	return false
}

// Policy is a tenant's config with its extra patterns compiled.
type Policy struct {
	Config   Config
	Redactor *Redactor
	// Prices are the workspace's price overrides (nil: none).
	Prices *cost.Table
}

// Redactor applies a config's extra patterns. Built-in redaction runs
// separately (internal/redact) and is never affected by remote settings.
type Redactor struct {
	rules []rule
}

type rule struct {
	marker string
	re     *regexp.Regexp
}

// NewRedactor compiles validated patterns; invalid ones are skipped.
func NewRedactor(patterns []Pattern) *Redactor {
	r := &Redactor{}
	for _, p := range patterns {
		if ValidatePattern(p.Pattern) != nil || !nameRe.MatchString(p.Name) {
			continue
		}
		r.rules = append(r.rules, rule{marker: "[REDACTED:" + p.Name + "]", re: regexp.MustCompile(p.Pattern)})
	}
	return r
}

// Apply redacts s.
func (r *Redactor) Apply(s string) string {
	if r == nil || s == "" {
		return s
	}
	for _, ru := range r.rules {
		s = ru.re.ReplaceAllLiteralString(s, ru.marker)
	}
	return s
}

// Empty reports whether there is nothing to apply.
func (r *Redactor) Empty() bool { return r == nil || len(r.rules) == 0 }

// Sampled reports whether a trace is kept at rate, using the same rule as
// OpenTelemetry's TraceIdRatioBased sampler (and so the Bindle SDK): the
// low 64 bits of the trace ID must be below round(rate * 2^64). The SDK and
// ingest therefore keep or drop the same traces, always whole.
func Sampled(traceIDHex string, rate float64) bool {
	if rate >= 1 {
		return true
	}
	if rate <= 0 {
		return false
	}
	if len(traceIDHex) < 16 {
		return true // malformed IDs are not ours to drop
	}
	low, err := strconv.ParseUint(strings.ToLower(traceIDHex[len(traceIDHex)-16:]), 16, 64)
	if err != nil {
		return true
	}
	bound := new(big.Float).Mul(big.NewFloat(rate), new(big.Float).SetInt(new(big.Int).Lsh(big.NewInt(1), 64)))
	bi, _ := bound.Add(bound, big.NewFloat(0.5)).Int(nil) // round half up, as Python's round() on these values
	return new(big.Int).SetUint64(low).Cmp(bi) < 0
}
