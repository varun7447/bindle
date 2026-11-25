// Package skillship is phase 2 of the learning loop: proving a skill by
// replaying the failures it was written for, shipping approved skills to
// agents, and measuring each shipped version so a worse one rolls back.
//
// Everything that runs the customer's agent happens in the customer's
// environment (bindle skills prove); Bindle only stores the report, judges
// it against the workspace's threshold and serves approved text.
package skillship

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/skills"
)

// Proof settings.
const (
	DefaultProofThreshold = 80 // percent of replayed failures a skill must fix
	MinProofThreshold     = 1
	MaxProofThreshold     = 100
	MaxProofRuns          = 50  // replayed runs per report
	MaxGaps               = 200 // coverage gaps listed per report
	MaxReportBytes        = 256 << 10
	MaxCassettes          = 5         // evidence runs recorded per lesson
	MaxCassetteBytes      = 512 << 10 // one stored cassette (JSON)
)

// RunResult is one replayed failing run.
type RunResult struct {
	TraceID string `json:"trace_id"`
	// Fixed: the agent finished, every tool call was answered from the
	// recording, and the lesson's failure didn't happen again.
	Fixed   bool   `json:"fixed"`
	Covered bool   `json:"covered"` // no call was missing from the recording
	Detail  string `json:"detail,omitempty"`
}

// Report is what bindle skills prove uploads.
type Report struct {
	Agent  string      `json:"agent"`
	Path   string      `json:"path"`
	SHA256 string      `json:"sha256"` // the exact skill text proven
	Lesson string      `json:"lesson"`
	Runs   []RunResult `json:"runs"`
	// Evals: the agent's eval dataset run with the skill; Regressions are
	// (run, check) pairs that passed in the recorded run and fail with it.
	EvalRuns    int `json:"eval_runs"`
	Regressions int `json:"regressions"`
	// Gaps is what replay couldn't cover: "<trace>: tool <name> not in the
	// recording", "<trace>: content_missing", ...
	Gaps []string `json:"gaps"`
}

// Verdict is the server's judgement of a report.
type Verdict struct {
	Total       int     `json:"total"`
	Fixed       int     `json:"fixed"`
	FixedRate   float64 `json:"fixed_rate"`
	Threshold   int     `json:"threshold"`
	Regressions int     `json:"regressions"`
	Passed      bool    `json:"passed"`
	Reason      string  `json:"reason"`
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsSkillPath reports whether p is a skill's vault path (skills/...).
func IsSkillPath(p string) bool {
	return skills.ValidPath(p) && skills.KindForPath(p) == skills.KindSkill
}

// Validate checks a report's shape (not whether it passes).
func Validate(r *Report) error {
	switch {
	case r.Agent == "" || len(r.Agent) > 128:
		return &skills.Error{Msg: "agent is required (at most 128 characters)"}
	case !IsSkillPath(r.Path):
		return &skills.Error{Msg: "path must be a skill path such as skills/<name>/SKILL.md"}
	case !shaRe.MatchString(r.SHA256):
		return &skills.Error{Msg: "sha256 must be the skill text's 64-character hex SHA-256"}
	case len(r.Runs) == 0:
		return &skills.Error{Msg: "the report has no replayed runs"}
	case len(r.Runs) > MaxProofRuns:
		return &skills.Error{Msg: fmt.Sprintf("at most %d replayed runs per report", MaxProofRuns)}
	case r.Regressions < 0 || r.EvalRuns < 0 || r.Regressions > r.EvalRuns*1000:
		return &skills.Error{Msg: "eval_runs and regressions must be counts"}
	}
	seen := map[string]bool{}
	for _, run := range r.Runs {
		if len(run.TraceID) != 32 || seen[run.TraceID] {
			return &skills.Error{Msg: "every run needs a distinct 32-character trace_id"}
		}
		seen[run.TraceID] = true
		if run.Fixed && !run.Covered {
			return &skills.Error{Msg: "a run can't be fixed if the recording didn't cover it"}
		}
	}
	if len(r.Gaps) > MaxGaps {
		r.Gaps = r.Gaps[:MaxGaps]
	}
	for i, g := range r.Gaps {
		if len(g) > 300 {
			r.Gaps[i] = g[:300]
		}
	}
	return nil
}

// Judge decides whether a report proves the skill: it fixes at least
// threshold percent of the replayed failures (an uncovered run counts as
// not fixed) and causes no eval regressions.
func Judge(r Report, threshold int) Verdict {
	if threshold < MinProofThreshold || threshold > MaxProofThreshold {
		threshold = DefaultProofThreshold
	}
	v := Verdict{Total: len(r.Runs), Threshold: threshold, Regressions: r.Regressions}
	for _, run := range r.Runs {
		if run.Fixed && run.Covered {
			v.Fixed++
		}
	}
	if v.Total > 0 {
		v.FixedRate = float64(v.Fixed) / float64(v.Total)
	}
	fixedEnough := v.Total > 0 && v.Fixed*100 >= threshold*v.Total
	v.Passed = fixedEnough && r.Regressions == 0
	switch {
	case v.Passed:
		v.Reason = fmt.Sprintf("fixed %d of %d replayed failures (%s, threshold %d%%) with no eval regressions",
			v.Fixed, v.Total, pct(v.FixedRate), threshold)
	case !fixedEnough:
		v.Reason = fmt.Sprintf("fixed %d of %d replayed failures (%s); %d%% needed", v.Fixed, v.Total, pct(v.FixedRate), threshold)
	default:
		v.Reason = fmt.Sprintf("fixed %d of %d replayed failures, but %d eval result(s) regressed", v.Fixed, v.Total, r.Regressions)
	}
	return v
}

func pct(f float64) string { return strconv.Itoa(int(math.Round(f*100))) + "%" }

// ---- shipping ----------------------------------------------------------------

// VersionsKey is the SKILL.md metadata key that limits a skill to some
// agent versions (space-separated, e.g. "v12 v13"). Absent: every version.
const VersionsKey = "bindle-agent-versions"

// Release is one approved skill text shipped to an agent.
type Release struct {
	Agent      string    `json:"agent"`
	Path       string    `json:"path"`
	Release    int       `json:"release"`
	SHA256     string    `json:"sha256"`
	Body       string    `json:"-"`
	ReleasedAt time.Time `json:"released_at"`
}

// Served is a skill as an agent receives it from GET /v1/skills.
type Served struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Path        string   `json:"path"`
	Release     int      `json:"release"`
	SHA256      string   `json:"sha256"`
	Body        string   `json:"body"`
	Versions    []string `json:"agent_versions,omitempty"`
}

// Ref names a shipped release in span events and step data: "<path>@<release>".
func Ref(path string, release int) string { return path + "@" + strconv.Itoa(release) }

// Serve turns an agent's current releases into what it may load for one
// agent version (empty: any). Releases that don't parse, or are limited to
// other versions, are left out.
func Serve(rs []Release, version string) []Served {
	out := []Served{}
	for _, r := range rs {
		sk, err := skills.Parse(r.Body)
		if err != nil {
			continue
		}
		var versions []string
		if v := strings.TrimSpace(sk.Metadata[VersionsKey]); v != "" {
			versions = strings.Fields(v)
			if version != "" && !contains(versions, version) {
				continue
			}
		}
		out = append(out, Served{Name: sk.Name, Description: sk.Description, Path: r.Path, Release: r.Release,
			SHA256: r.SHA256, Body: r.Body, Versions: versions})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ---- measuring ---------------------------------------------------------------

// Rollback rule. A shipped version is compared with the agent's runs that
// didn't load this skill (before or after it shipped). It rolls back only
// with enough runs on both sides and a clear, not marginal, difference:
//   - its outcome-graded success rate is MaxSuccessDrop (10 points) or more
//     below the runs without it, or
//   - the failure it was written for recurs MaxRecurrenceRise (5 points)
//     more often than without it.
//
// Token and latency overhead are measured and shown, but never roll a skill
// back on their own: a slower, correct agent is a choice for its owner.
const (
	MinRunsEach       = 20
	MaxSuccessDrop    = 0.10
	MaxRecurrenceRise = 0.05
	MeasureLookback   = 7 * 24 * time.Hour // runs before the release used as the baseline
	MeasureWindow     = 14 * 24 * time.Hour
)

// Side is one group of runs: with the release loaded, or without the skill.
type Side struct {
	Runs        int     `json:"runs"`
	Successes   int     `json:"successes"`
	SuccessRate float64 `json:"success_rate"`
	Recurred    int     `json:"recurred"` // runs where the lesson's failure happened
	Recurrence  float64 `json:"recurrence"`
	AvgTokens   float64 `json:"avg_tokens"`
	AvgMs       float64 `json:"avg_ms"`
}

// Measurement compares a shipped release with runs without the skill.
type Measurement struct {
	Agent    string    `json:"agent"`
	Path     string    `json:"path"`
	Release  int       `json:"release"`
	With     Side      `json:"with"`
	Without  Side      `json:"without"`
	Decision string    `json:"decision"` // "rollback", "keep" or "too_few_runs"
	Reason   string    `json:"reason"`
	At       time.Time `json:"computed_at"`
}

// RunFact is one run of the agent, for measuring.
type RunFact struct {
	TraceID  string
	Loaded   bool // loaded this release
	Other    bool // loaded another release of the same skill (left out)
	Success  bool // outcome-graded (no failed step, outcome or reversal)
	Recurred bool
	Tokens   int64
	Ms       float64
}

// Measure groups runs and applies the rollback rule.
func Measure(agent, path string, release int, runs []RunFact, now time.Time) Measurement {
	m := Measurement{Agent: agent, Path: path, Release: release, At: now.UTC()}
	var w, wo struct {
		Side
		tokens, ms float64
	}
	for _, r := range runs {
		if r.Other {
			continue
		}
		s := &wo
		if r.Loaded {
			s = &w
		}
		s.Runs++
		if r.Success {
			s.Successes++
		}
		if r.Recurred {
			s.Recurred++
		}
		s.tokens += float64(r.Tokens)
		s.ms += r.Ms
	}
	finish := func(s *Side, tokens, ms float64) {
		if s.Runs > 0 {
			n := float64(s.Runs)
			s.SuccessRate, s.Recurrence = float64(s.Successes)/n, float64(s.Recurred)/n
			s.AvgTokens, s.AvgMs = tokens/n, ms/n
		}
	}
	finish(&w.Side, w.tokens, w.ms)
	finish(&wo.Side, wo.tokens, wo.ms)
	m.With, m.Without = w.Side, wo.Side
	m.Decision, m.Reason = decide(m.With, m.Without)
	return m
}

func decide(with, without Side) (string, string) {
	if with.Runs < MinRunsEach || without.Runs < MinRunsEach {
		return "too_few_runs", fmt.Sprintf("needs %d runs with and without the skill (has %d and %d)", MinRunsEach, with.Runs, without.Runs)
	}
	if drop := without.SuccessRate - with.SuccessRate; drop >= MaxSuccessDrop-1e-9 {
		return "rollback", fmt.Sprintf("success fell from %s to %s with this version", pct(without.SuccessRate), pct(with.SuccessRate))
	}
	if rise := with.Recurrence - without.Recurrence; rise >= MaxRecurrenceRise-1e-9 {
		return "rollback", fmt.Sprintf("the failure it was written for happened in %s of runs with it, %s without", pct(with.Recurrence), pct(without.Recurrence))
	}
	return "keep", fmt.Sprintf("success %s with it, %s without; the failure recurred in %s with it, %s without",
		pct(with.SuccessRate), pct(without.SuccessRate), pct(with.Recurrence), pct(without.Recurrence))
}
