package outcomes

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// RunFacts is what grading needs to know about one run.
type RunFacts struct {
	TraceID  string
	Agent    string
	Version  string
	Failed   bool // a step failed or the run's root failed
	EvalPass int  // passed check and score results (not user feedback or outcomes)
	EvalFail int
}

// OutcomeFact is one outcome attached to a run: from bindle.outcome() in the
// run, or a later event matched by correlation.
type OutcomeFact struct {
	Name    string
	Success bool
}

// ReversedRun is a run that passed its evals but was later reversed.
type ReversedRun struct {
	TraceID string `json:"trace_id"`
	Outcome string `json:"outcome"`
}

// Grade is the outcome-graded quality of one agent version.
type Grade struct {
	Agent   string `json:"agent"`
	Version string `json:"version"`
	Runs    int    `json:"runs"`
	// Successes: no step failed, no outcome failed and no reversal.
	Successes   int     `json:"successes"`
	SuccessRate float64 `json:"success_rate"`
	// EvaluatedRuns have at least one check or score result; PassedEvals of
	// them had none fail.
	EvaluatedRuns int `json:"evaluated_runs"`
	PassedEvals   int `json:"passed_evals"`
	// Reversed runs have a reversal outcome; ReversedAfterPassing of them
	// had passed their evals.
	Reversed             int           `json:"reversed"`
	ReversedAfterPassing int           `json:"reversed_after_passing"`
	TopReversal          string        `json:"top_reversal,omitempty"`
	Headline             string        `json:"headline"`
	ReversedRuns         []ReversedRun `json:"reversed_runs"`
}

// MaxReversedRuns caps the runs listed per version.
const MaxReversedRuns = 50

// GradeRuns grades each agent version: a run counts as successful when no
// step failed, no outcome failed and no reversal outcome arrived, even if
// every step and eval passed. Versions are sorted by agent, newest version
// name first (as on the Agents page).
func GradeRuns(runs []RunFacts, outcomes map[string][]OutcomeFact, s Settings) []Grade {
	type key struct{ agent, version string }
	by := map[key]*Grade{}
	tops := map[key]map[string]int{}
	var order []key
	for _, r := range runs {
		k := key{r.Agent, r.Version}
		g := by[k]
		if g == nil {
			g = &Grade{Agent: r.Agent, Version: r.Version, ReversedRuns: []ReversedRun{}}
			by[k] = g
			tops[k] = map[string]int{}
			order = append(order, k)
		}
		g.Runs++
		evaluated := r.EvalPass+r.EvalFail > 0
		passed := evaluated && r.EvalFail == 0
		if evaluated {
			g.EvaluatedRuns++
		}
		if passed {
			g.PassedEvals++
		}
		reversal, failedOutcome := "", false
		for _, o := range outcomes[r.TraceID] {
			if s.IsReversal(o.Name) {
				if reversal == "" || o.Name < reversal {
					reversal = o.Name
				}
			} else if !o.Success {
				failedOutcome = true
			}
		}
		if reversal != "" {
			g.Reversed++
			tops[k][reversal]++
			if passed {
				g.ReversedAfterPassing++
				if len(g.ReversedRuns) < MaxReversedRuns {
					g.ReversedRuns = append(g.ReversedRuns, ReversedRun{TraceID: r.TraceID, Outcome: reversal})
				}
			}
		}
		if !r.Failed && !failedOutcome && reversal == "" {
			g.Successes++
		}
	}
	out := make([]Grade, 0, len(order))
	for _, k := range order {
		g := by[k]
		g.SuccessRate = ratio(g.Successes, g.Runs)
		g.TopReversal = top(tops[k])
		g.Headline = headline(*g)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return out[i].Version > out[j].Version
	})
	return out
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func top(counts map[string]int) string {
	best, n := "", 0
	for name, c := range counts {
		if c > n || (c == n && name < best) {
			best, n = name, c
		}
	}
	return best
}

func pct(n, d int) string { return fmt.Sprintf("%d%%", int(math.Round(100*ratio(n, d)))) }

func headline(g Grade) string {
	v := g.Version
	if v == "" {
		v = "Unversioned"
	}
	why := ""
	if g.TopReversal != "" {
		why = " (" + strings.ReplaceAll(g.TopReversal, "_", " ") + ")"
	}
	switch {
	case g.Runs == 0:
		return v + ": no runs in this period."
	case g.EvaluatedRuns > 0 && g.Reversed > 0:
		return fmt.Sprintf("%s: %s passed evals, but %s of runs were later reversed%s.", v, pct(g.PassedEvals, g.EvaluatedRuns), pct(g.Reversed, g.Runs), why)
	case g.EvaluatedRuns > 0:
		return fmt.Sprintf("%s: %s passed evals, and no runs were reversed.", v, pct(g.PassedEvals, g.EvaluatedRuns))
	case g.Reversed > 0:
		return fmt.Sprintf("%s: %s of runs were later reversed%s.", v, pct(g.Reversed, g.Runs), why)
	default:
		return fmt.Sprintf("%s: %s of runs succeeded, and none were reversed.", v, pct(g.Successes, g.Runs))
	}
}
