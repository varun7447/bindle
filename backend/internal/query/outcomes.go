package query

import "time"

// ---- problem runs -> eval cases -------------------------------------------

// Problem-run sources: where runs to add to a dataset come from.
const (
	SourceFailure          = "failure"           // failed steps (a Home failure group)
	SourceNegativeFeedback = "negative_feedback" // users gave a thumbs down
	SourceFailedScore      = "failed_score"      // a score or check result failed
	SourceFailedRuns       = "failed_runs"       // any failed run of an agent
)

// ProblemFilter selects problem runs. StepName and Pattern narrow failures
// (as grouped on Home); Score narrows failed scores to one score name.
type ProblemFilter struct {
	Kind     string
	StepName string
	Pattern  string
	Agent    string
	Score    string
	From, To time.Time
	Limit    int // at most 200
}

// ---- outcome economics -------------------------------------------------------

// How a successful outcome was decided for an agent.
const (
	// OutcomeExplicit: the agent reports outcomes with bindle.outcome(); a
	// run succeeded when its outcomes passed.
	OutcomeExplicit = "outcome"
	// OutcomeInferred: no outcomes were sent; a run succeeded when no step
	// failed and no user gave it negative feedback.
	OutcomeInferred = "no_failure_or_negative_feedback"
)

// Economics is what one agent version spent per successful outcome.
// Cost is priced cost only: runs with unpriced model calls are counted in
// UnknownCostRuns and their unpriced part is never estimated.
type Economics struct {
	Agent      string `json:"agent"`
	Version    string `json:"version"`
	Definition string `json:"definition"` // OutcomeExplicit or OutcomeInferred
	Runs       uint64 `json:"runs"`
	// Successes are runs with a successful outcome.
	Successes uint64 `json:"successes"`
	// Wasted are runs that cost money without a good outcome: failed runs,
	// runs whose outcome failed, and (inferred) runs users disliked.
	Wasted          uint64   `json:"wasted_runs"`
	OutcomeRuns     uint64   `json:"outcome_runs"` // runs that reported an outcome
	CostUSD         float64  `json:"cost_usd"`
	WastedUSD       float64  `json:"wasted_usd"`
	CostPerSuccess  *float64 `json:"cost_per_success_usd"` // nil without successes
	UnknownCostRuns uint64   `json:"unknown_cost_runs"`
}
