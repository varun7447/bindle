// Package lessons turns repeated production failures into lessons: the same
// agent, the same tool (or step), the same error shape and the same likely
// cause, seen in at least N runs (a workspace setting, default 5).
//
// A lesson is deterministic: it is computed from failure groups Bindle
// already keeps (error patterns per step, see normalize.ErrorPattern), and
// its likely cause is classified from the error shape with fixed rules, so
// the same data always gives the same lessons. The console's per-run
// "likely cause" reasoning (console/lib/blame.ts) needs a whole trace and
// isn't ported here; a lesson's cause is the class of the error itself.
//
// A lesson pack is what the customer's own model sees when drafting a
// skill: only already-redacted normalized data, redacted again on the way
// out, clipped, and empty where content capture is off.
package lessons

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bindle-ai/bindle/backend/internal/redact"
)

// Defaults and bounds.
const (
	DefaultMinRuns = 5
	MinMinRuns     = 2
	MaxMinRuns     = 1000
	MaxEvidence    = 20      // trace IDs kept per lesson
	PackEvidence   = 5       // evidence steps in a pack
	PackField      = 2 << 10 // bytes per input/output preview in a pack
	Window         = 7 * 24 * time.Hour
)

// Evidence is one failing step behind a lesson.
type Evidence struct {
	TraceID  string    `json:"trace_id"`
	StepID   string    `json:"step_id"`
	StepType string    `json:"step_type"`
	Name     string    `json:"name"`
	Tool     string    `json:"tool,omitempty"`
	At       time.Time `json:"at"`
	Input    string    `json:"input,omitempty"`
	Output   string    `json:"output,omitempty"`
}

// Group is one failure group from the trace store: failed steps of one
// agent with the same step, tool and error pattern.
type Group struct {
	Agent     string
	StepName  string
	StepType  string
	Tool      string
	Pattern   string
	Runs      int // distinct runs
	Failures  int
	FirstSeen time.Time
	LastSeen  time.Time
	Traces    []string   // most recent first
	Samples   []Evidence // most recent first
}

// Lesson is a repeated failure worth learning from.
type Lesson struct {
	ID        string    `json:"id"`
	Agent     string    `json:"agent"`
	StepName  string    `json:"step_name"`
	StepType  string    `json:"step_type"`
	Tool      string    `json:"tool,omitempty"`
	Pattern   string    `json:"error_pattern"`
	Cause     string    `json:"cause"`
	Title     string    `json:"title"`
	Runs      int       `json:"runs"`
	Failures  int       `json:"failures"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Evidence  []string  `json:"evidence"` // trace IDs
	Pack      Pack      `json:"-"`
}

// Likely causes, classified from the error shape.
const (
	CauseBadArguments = "bad_arguments"
	CauseTimeout      = "timeout"
	CauseRateLimited  = "rate_limited"
	CauseNotFound     = "not_found"
	CausePermission   = "permission"
	CauseUnavailable  = "unavailable"
	CauseError        = "error"
)

var causeRules = []struct {
	cause string
	re    *regexp.Regexp
}{
	{CauseRateLimited, regexp.MustCompile(`(?i)rate.?limit|too many requests|\b429\b|throttl`)},
	{CauseTimeout, regexp.MustCompile(`(?i)timed? ?out|time-?out|deadline exceeded|\b504\b`)},
	{CausePermission, regexp.MustCompile(`(?i)unauthori[sz]ed|forbidden|permission|access denied|\b40[13]\b`)},
	{CauseBadArguments, regexp.MustCompile(`(?i)invalid (?:argument|parameter|input|request|value)|validation|schema|missing required|unexpected keyword|required (?:field|argument|parameter)|bad request|\b400\b|typeerror|valueerror`)},
	{CauseNotFound, regexp.MustCompile(`(?i)not found|no such|does not exist|doesn't exist|\b404\b`)},
	{CauseUnavailable, regexp.MustCompile(`(?i)unavailable|connection (?:refused|reset)|\b50[23]\b|bad gateway`)},
}

// Cause classifies an error pattern.
func Cause(pattern string) string {
	for _, r := range causeRules {
		if r.re.MatchString(pattern) {
			return r.cause
		}
	}
	return CauseError
}

var causeText = map[string]string{
	CauseBadArguments: "the call's arguments were rejected",
	CauseTimeout:      "the call timed out",
	CauseRateLimited:  "the call was rate limited",
	CauseNotFound:     "what it asked for wasn't found",
	CausePermission:   "the call wasn't permitted",
	CauseUnavailable:  "the service was unavailable",
	CauseError:        "the call failed",
}

// CauseText is a cause in plain words.
func CauseText(c string) string {
	if t, ok := causeText[c]; ok {
		return t
	}
	return causeText[CauseError]
}

// ID is a lesson's stable identifier: the same agent, step, tool and error
// pattern always give the same ID.
func ID(agent, stepName, tool, pattern string) string {
	h := sha256.Sum256([]byte(agent + "\x00" + stepName + "\x00" + tool + "\x00" + pattern))
	return "les_" + hex.EncodeToString(h[:8])
}

// Build keeps the groups seen in at least minRuns runs, as lessons, most
// runs first.
func Build(groups []Group, minRuns int, capture bool) []Lesson {
	if minRuns < MinMinRuns {
		minRuns = DefaultMinRuns
	}
	out := []Lesson{}
	for _, g := range groups {
		if g.Runs < minRuns || g.Pattern == "" {
			continue
		}
		what := g.Tool
		if what == "" {
			what = g.StepName
		}
		cause := Cause(g.Pattern)
		l := Lesson{
			ID: ID(g.Agent, g.StepName, g.Tool, g.Pattern), Agent: g.Agent, StepName: g.StepName, StepType: g.StepType,
			Tool: g.Tool, Pattern: clip(redact.Default().Apply(g.Pattern), 500), Cause: cause,
			Runs: g.Runs, Failures: g.Failures, FirstSeen: g.FirstSeen.UTC(), LastSeen: g.LastSeen.UTC(),
		}
		l.Title = fmt.Sprintf("%s failed in %d runs: %s (likely cause: %s)", what, g.Runs, l.Pattern, CauseText(cause))
		for _, t := range g.Traces {
			if len(l.Evidence) == MaxEvidence {
				break
			}
			l.Evidence = append(l.Evidence, t)
		}
		l.Pack = BuildPack(l, g.Samples, capture)
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Runs != out[j].Runs {
			return out[i].Runs > out[j].Runs
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Pack is what drafting a skill gets about a lesson. It holds only
// redacted, normalized data: the error pattern (not the raw message), step
// and tool names, and clipped previews of the failing steps' inputs and
// outputs when content capture is on.
type Pack struct {
	Format    string     `json:"format"` // "bindle.lesson-pack/v1"
	Lesson    string     `json:"lesson"`
	Agent     string     `json:"agent"`
	StepName  string     `json:"step_name"`
	StepType  string     `json:"step_type"`
	Tool      string     `json:"tool,omitempty"`
	Pattern   string     `json:"error_pattern"`
	Cause     string     `json:"cause"`
	CauseText string     `json:"cause_text"`
	Runs      int        `json:"runs"`
	Failures  int        `json:"failures"`
	Evidence  []Evidence `json:"evidence"`
	// Content is false when the workspace doesn't capture prompts and
	// outputs: the evidence then has names only.
	Content bool `json:"content"`
}

// PackFormat names the lesson pack format.
const PackFormat = "bindle.lesson-pack/v1"

// BuildPack assembles a lesson's pack from sample steps. Every text field is
// redacted again (the steps were redacted at ingest; this is a second
// line), and inputs and outputs are dropped when capture is off.
func BuildPack(l Lesson, samples []Evidence, capture bool) Pack {
	r := redact.Default()
	p := Pack{Format: PackFormat, Lesson: l.ID, Agent: l.Agent, StepName: l.StepName, StepType: l.StepType, Tool: l.Tool,
		Pattern: l.Pattern, Cause: l.Cause, CauseText: CauseText(l.Cause), Runs: l.Runs, Failures: l.Failures,
		Evidence: []Evidence{}, Content: capture}
	for _, s := range samples {
		if len(p.Evidence) == PackEvidence {
			break
		}
		e := Evidence{TraceID: s.TraceID, StepID: s.StepID, StepType: s.StepType, Name: r.Apply(s.Name), Tool: r.Apply(s.Tool), At: s.At.UTC()}
		if capture {
			e.Input = clip(r.Apply(s.Input), PackField)
			e.Output = clip(r.Apply(s.Output), PackField)
		}
		p.Evidence = append(p.Evidence, e)
	}
	return p
}

// clip cuts s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return strings.TrimSpace(s) + "…"
}
