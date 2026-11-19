package evals

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// Suggestion is a check Bindle proposes for a set of problem runs. It is
// not saved until the user confirms it. Catches is how many of the problem
// runs fail it today; suggestions that catch none are never proposed.
type Suggestion struct {
	Scorer  Scorer `json:"scorer"`
	Reason  string `json:"reason"`
	Catches int    `json:"catches"`
	Graded  int    `json:"graded"` // problem runs the check could grade (not skipped)
	// Exists is set by the caller when an equivalent check is already saved.
	Exists bool `json:"exists"`
}

// SuggestInput is what suggestions are based on: the problem runs, good
// runs of the same agent for comparison, and the error shape (for failure
// groups).
type SuggestInput struct {
	Problem      []Run
	Baseline     []Run  // recent successful runs of the same agent
	ErrorPattern string // e.g. "timeout after <n>s on order <n>"
	Agent        string // set on every suggestion ("" = all agents)
}

var placeholderRe = regexp.MustCompile(`<(?:n|id|str)>`)

const (
	minFragment    = 8
	baselineShare  = 0.9 // a "normal" behavior is seen in at least 90% of good runs
	minBaselineRun = 5
)

// Suggest proposes checks that would have caught the problem runs: no step
// fails, the error text doesn't reach the user, the agent calls the tool its
// good runs always call, the run stays within a step budget (runaway loops),
// and the output stays valid JSON when good runs always return JSON. Each
// suggestion is graded against the problem runs; only those that fail at
// least one are returned, most catches first.
func Suggest(in SuggestInput) []Suggestion {
	var cands []Suggestion
	add := func(s Scorer, reason string) {
		s.Agent, s.Enabled = in.Agent, true
		if len(s.Name) > maxName {
			s.Name = strings.TrimSpace(s.Name[:maxName])
		}
		if Validate(s) != nil {
			return
		}
		cands = append(cands, Suggestion{Scorer: s, Reason: reason})
	}

	add(Scorer{Name: "No step fails", Kind: KindSucceeded}, "these runs had a failed step")

	if frag := literalFragment(in.ErrorPattern); frag != "" {
		add(Scorer{Name: "Doesn't say: " + frag, Kind: KindNotContains, Value: frag},
			"the error text reached the final output")
	}

	if len(in.Baseline) >= minBaselineRun {
		n := float64(len(in.Baseline))
		// Tools that good runs almost always call.
		called := map[string]int{}
		for _, r := range in.Baseline {
			seen := map[string]bool{}
			for _, t := range r.Tools {
				if !seen[t] {
					seen[t] = true
					called[t]++
				}
			}
		}
		tools := make([]string, 0, len(called))
		for t, c := range called {
			if float64(c)/n >= baselineShare {
				tools = append(tools, t)
			}
		}
		sort.Strings(tools)
		for _, t := range tools {
			add(Scorer{Name: "Calls " + t, Kind: KindToolCalled, Value: t},
				fmt.Sprintf("good runs call %s; these runs didn't", t))
		}
		// Step budget: well above what good runs take.
		steps := make([]float64, 0, len(in.Baseline))
		for _, r := range in.Baseline {
			steps = append(steps, float64(r.Steps))
		}
		sort.Float64s(steps)
		med := steps[len(steps)/2]
		if len(steps)%2 == 0 {
			med = (steps[len(steps)/2-1] + steps[len(steps)/2]) / 2
		}
		if med > 0 {
			limit := math.Ceil(math.Max(2*med, med+5))
			add(Scorer{Name: fmt.Sprintf("At most %s steps", num(limit)), Kind: KindMaxSteps, Number: limit},
				fmt.Sprintf("good runs take about %s steps; these took far more", num(med)))
		}
		// JSON output, when good runs always return JSON.
		withOut, valid := 0, 0
		for _, r := range in.Baseline {
			if o := strings.TrimSpace(r.Output); o != "" {
				withOut++
				if json.Valid([]byte(o)) {
					valid++
				}
			}
		}
		if withOut >= minBaselineRun && float64(valid)/float64(withOut) >= baselineShare {
			add(Scorer{Name: "Valid JSON output", Kind: KindJSONValid}, "good runs return JSON; these didn't")
		}
	}

	out := make([]Suggestion, 0, len(cands))
	for _, c := range cands {
		for _, r := range in.Problem {
			res := Evaluate(c.Scorer, r)
			if res.Skipped {
				continue
			}
			c.Graded++
			if !res.Pass {
				c.Catches++
			}
		}
		if c.Catches > 0 {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Catches > out[j].Catches })
	return out
}

// literalFragment is the longest run of literal text in an error pattern
// (the parts between <n>, <id> and <str> placeholders), if long enough to be
// a useful "doesn't contain" check.
func literalFragment(pattern string) string {
	best := ""
	for _, part := range placeholderRe.Split(pattern, -1) {
		part = strings.Trim(part, " :,;.-'\"()[]{}")
		if len(part) > len(best) {
			best = part
		}
	}
	if len(best) < minFragment {
		return ""
	}
	if len(best) > maxValue {
		best = best[:maxValue]
	}
	return best
}

// Same returns true when two checks test the same thing (used to flag
// suggestions that are already saved).
func Same(a, b Scorer) bool {
	return a.Kind == b.Kind && a.Agent == b.Agent && strings.EqualFold(a.Value, b.Value) && a.Number == b.Number
}
