// Package evals holds Bindle's checks: deterministic scorers that grade a
// run (its final output, the tools it called, its steps, cost and time)
// without calling a model. They run on datasets saved from runs and on a
// sample of recent runs per agent version. Model-graded scores come from the
// customer's own environment through bindle.score() in the SDKs; Bindle
// never holds model credentials.
package evals

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bindle-ai/bindle/backend/internal/sdkconfig"
)

// Scorer kinds.
const (
	KindContains      = "contains"        // Value appears in the output
	KindNotContains   = "not_contains"    // Value doesn't appear in the output
	KindRegex         = "regex"           // Value (RE2) matches the output
	KindJSONValid     = "json_valid"      // the output parses as JSON
	KindToolCalled    = "tool_called"     // tool Value was called
	KindToolNotCalled = "tool_not_called" // tool Value wasn't called
	KindMaxSteps      = "max_steps"       // steps <= Number
	KindMaxCost       = "max_cost_usd"    // priced cost <= Number
	KindMaxDuration   = "max_duration_ms" // duration <= Number
	KindSucceeded     = "run_succeeded"   // no step failed
)

var kinds = map[string]bool{KindContains: true, KindNotContains: true, KindRegex: true, KindJSONValid: true,
	KindToolCalled: true, KindToolNotCalled: true, KindMaxSteps: true, KindMaxCost: true, KindMaxDuration: true, KindSucceeded: true}

// Scorer is one check. Agent limits it to one agent ("" = all agents).
type Scorer struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	Agent         string    `json:"agent"`
	Value         string    `json:"value"`
	Number        float64   `json:"number"`
	CaseSensitive bool      `json:"case_sensitive"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
}

// Run is what a check sees of one run.
type Run struct {
	TraceID    string   `json:"trace_id"`
	Agent      string   `json:"agent"`
	Version    string   `json:"version"`
	Input      string   `json:"input"`
	Output     string   `json:"output"`
	Tools      []string `json:"tools"`
	Steps      int64    `json:"steps"`
	CostUSD    float64  `json:"cost_usd"`
	CostKnown  bool     `json:"cost_known"` // every model step was priced
	DurationMs float64  `json:"duration_ms"`
	Failed     bool     `json:"failed"`
	// Source says where a dataset item came from ("" = added by hand).
	Source string `json:"source,omitempty"`
}

// Result is one check on one run. Skipped means the check couldn't apply
// (no output captured, or an unpriced run for a cost check); skipped
// results don't count toward pass rates.
type Result struct {
	Pass    bool   `json:"pass"`
	Skipped bool   `json:"skipped"`
	Detail  string `json:"detail"`
}

const (
	MaxScorers    = 50
	maxName       = 80
	maxValue      = 500
	maxNumber     = 1e9
	maxOutputScan = 1 << 20
)

// ValidationError is a problem with user input; its message is safe to show.
type ValidationError struct{ Msg string }

func (e ValidationError) Error() string { return e.Msg }

// Invalid returns a ValidationError.
func Invalid(format string, args ...any) error {
	return ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// IsInvalid reports whether err is a ValidationError.
func IsInvalid(err error) bool {
	var v ValidationError
	return errors.As(err, &v)
}

// Validate checks a scorer before it is saved.
func Validate(s Scorer) error {
	if err := validate(s); err != nil {
		return ValidationError{Msg: err.Error()}
	}
	return nil
}

func validate(s Scorer) error {
	if n := strings.TrimSpace(s.Name); n == "" || len(n) > maxName || n != s.Name {
		return fmt.Errorf("name must be 1-%d characters without leading or trailing spaces", maxName)
	}
	if !kinds[s.Kind] {
		return fmt.Errorf("unknown check kind %q", s.Kind)
	}
	if len(s.Agent) > 128 || strings.TrimSpace(s.Agent) != s.Agent {
		return errors.New("agent must be at most 128 characters without leading or trailing spaces")
	}
	if !utf8.ValidString(s.Value) || len(s.Value) > maxValue {
		return fmt.Errorf("value must be at most %d characters", maxValue)
	}
	switch s.Kind {
	case KindContains, KindNotContains, KindToolCalled, KindToolNotCalled:
		if s.Value == "" {
			return errors.New("this check needs a value")
		}
	case KindRegex:
		if err := sdkconfig.ValidatePattern(s.Value); err != nil {
			return fmt.Errorf("pattern: %w", err)
		}
	case KindMaxSteps, KindMaxCost, KindMaxDuration:
		if math.IsNaN(s.Number) || s.Number <= 0 || s.Number > maxNumber {
			return errors.New("this check needs a limit greater than 0")
		}
	}
	return nil
}

// Applies reports whether s checks runs of agent.
func (s Scorer) Applies(agent string) bool { return s.Agent == "" || s.Agent == agent }

// Evaluate runs one check. Regex patterns are validated on save; an invalid
// one here is skipped, never a failure.
func Evaluate(s Scorer, r Run) Result {
	out := r.Output
	if len(out) > maxOutputScan {
		out = out[:maxOutputScan]
	}
	fold := func(v string) string {
		if s.CaseSensitive {
			return v
		}
		return strings.ToLower(v)
	}
	needsOutput := s.Kind == KindContains || s.Kind == KindNotContains || s.Kind == KindRegex || s.Kind == KindJSONValid
	if needsOutput && strings.TrimSpace(out) == "" {
		return Result{Skipped: true, Detail: "no output captured"}
	}
	switch s.Kind {
	case KindContains:
		if strings.Contains(fold(out), fold(s.Value)) {
			return Result{Pass: true}
		}
		return Result{Detail: "output doesn't contain " + strconv.Quote(s.Value)}
	case KindNotContains:
		if !strings.Contains(fold(out), fold(s.Value)) {
			return Result{Pass: true}
		}
		return Result{Detail: "output contains " + strconv.Quote(s.Value)}
	case KindRegex:
		expr := s.Value
		if !s.CaseSensitive {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return Result{Skipped: true, Detail: "invalid pattern"}
		}
		if re.MatchString(out) {
			return Result{Pass: true}
		}
		return Result{Detail: "output doesn't match the pattern"}
	case KindJSONValid:
		if json.Valid([]byte(strings.TrimSpace(out))) {
			return Result{Pass: true}
		}
		return Result{Detail: "output isn't valid JSON"}
	case KindToolCalled, KindToolNotCalled:
		called := false
		for _, t := range r.Tools {
			if t == s.Value {
				called = true
				break
			}
		}
		if called == (s.Kind == KindToolCalled) {
			return Result{Pass: true}
		}
		if called {
			return Result{Detail: s.Value + " was called"}
		}
		return Result{Detail: s.Value + " wasn't called"}
	case KindMaxSteps:
		if float64(r.Steps) <= s.Number {
			return Result{Pass: true}
		}
		return Result{Detail: fmt.Sprintf("%d steps, over %s", r.Steps, num(s.Number))}
	case KindMaxCost:
		if !r.CostKnown {
			return Result{Skipped: true, Detail: "some model calls have no known price"}
		}
		if r.CostUSD <= s.Number {
			return Result{Pass: true}
		}
		return Result{Detail: fmt.Sprintf("cost $%s, over $%s", num(r.CostUSD), num(s.Number))}
	case KindMaxDuration:
		if r.DurationMs <= s.Number {
			return Result{Pass: true}
		}
		return Result{Detail: fmt.Sprintf("took %s ms, over %s ms", num(math.Round(r.DurationMs)), num(s.Number))}
	case KindSucceeded:
		if !r.Failed {
			return Result{Pass: true}
		}
		return Result{Detail: "a step failed"}
	}
	return Result{Skipped: true, Detail: "unknown check"}
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// Tally counts results for one scorer.
type Tally struct {
	ScorerID string `json:"scorer_id"`
	Name     string `json:"name"`
	Passed   int    `json:"passed"`
	Failed   int    `json:"failed"`
	Skipped  int    `json:"skipped"`
}

// Add records one result.
func (t *Tally) Add(r Result) {
	switch {
	case r.Skipped:
		t.Skipped++
	case r.Pass:
		t.Passed++
	default:
		t.Failed++
	}
}

// PassRate is passed / (passed + failed), or -1 when nothing was graded.
func (t Tally) PassRate() float64 {
	if n := t.Passed + t.Failed; n > 0 {
		return float64(t.Passed) / float64(n)
	}
	return -1
}
