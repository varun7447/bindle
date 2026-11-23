package passport

import (
	"sort"
	"strings"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/security"
)

// Drift kinds. A drift reason is "kind:detail", e.g.
// "tool_not_allowed:refund_payment".
const (
	ToolNotAllowed      = "tool_not_allowed"
	DataClassNotAllowed = "data_class_not_allowed"
	EgressBlocked       = "egress_blocked"
	ReleaseNotAllowed   = "release_not_allowed"
	RateOverLimit       = "rate_over_limit"
	RunOverBudget       = "run_over_budget"
	DayOverBudget       = "day_over_budget"
)

// Explain is the plain-language meaning of each drift kind.
var Explain = map[string]string{
	ToolNotAllowed:      "the agent called a tool its passport doesn't list",
	DataClassNotAllowed: "a step handled a kind of data its passport doesn't allow",
	EgressBlocked:       "data its passport blocks from leaving was passed to a tool that sends data out",
	ReleaseNotAllowed:   "the agent took a release action (deploy, merge, push, publish) and its passport gives it no release rights",
	RateOverLimit:       "a tool was called more often than its passport allows in the window",
	RunOverBudget:       "one run cost more than the passport's per-run budget",
	DayOverBudget:       "the agent's spend today is over the passport's daily budget",
}

// Kinds lists the drift kinds in display order.
var Kinds = []string{ToolNotAllowed, DataClassNotAllowed, EgressBlocked, ReleaseNotAllowed, RateOverLimit, RunOverBudget, DayOverBudget}

// DefaultReleaseTools name release actions when a passport doesn't list its
// own release_tools.
var DefaultReleaseTools = []string{"deploy*", "release*", "publish*", "merge*", "git_push", "*config_push*"}

const maxDetail = 64

// StepDrift returns the step's drift reasons against p, sorted. It needs
// only the step itself, so ingest computes it as steps arrive. Rate and
// budget drift depend on other steps; see Stateful.
func (p *Passport) StepDrift(s model.Step) []string {
	if p == nil {
		return nil
	}
	seen := map[string]bool{}
	add := func(kind, detail string) {
		if len(detail) > maxDetail {
			detail = detail[:maxDetail]
		}
		seen[kind+":"+detail] = true
	}
	tool := s.Type == model.StepTool && s.ToolName != ""
	if tool && p.Tools != nil && !MatchAny(p.Tools.Allow, s.ToolName) {
		add(ToolNotAllowed, s.ToolName)
	}
	if d := p.DataClasses; d != nil {
		if d.Allow != nil {
			for _, c := range s.DataClasses {
				if !contains(d.Allow, c) {
					add(DataClassNotAllowed, c)
				}
			}
		}
		if tool && len(d.EgressBlock) > 0 && security.IsOutboundTool(s.ToolName) {
			for _, c := range s.InputDataClasses {
				if contains(d.EgressBlock, c) {
					add(EgressBlocked, c)
				}
			}
		}
	}
	if tool && p.ReleaseRights == ReleaseNone {
		release := p.ReleaseTools
		if len(release) == 0 {
			release = DefaultReleaseTools
		}
		if MatchAny(release, s.ToolName) {
			add(ReleaseNotAllowed, s.ToolName)
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// KindOf returns the kind part of a drift reason.
func KindOf(reason string) string {
	k, _, _ := strings.Cut(reason, ":")
	return k
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Set is a workspace's active passports by agent name.
type Set map[string]*Passport

// Agents returns the agents with a passport, sorted.
func (s Set) Agents() []string {
	out := make([]string, 0, len(s))
	for a := range s {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// For returns the agent's passport, or nil.
func (s Set) For(agent string) *Passport {
	if s == nil {
		return nil
	}
	return s[agent]
}
