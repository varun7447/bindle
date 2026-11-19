// Package query defines what the console API reads from storage: the Store
// interface and its result types. internal/search implements it.
package query

import (
	"context"
	"errors"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/evals"
)

// ErrNoTenant is returned when a Store method is called without a tenant.
var ErrNoTenant = errors.New("query: tenant is required")

// Store is what the API needs from storage. Every method takes the tenant
// resolved from the caller's session and must scope all reads to it.
type Store interface {
	ListRuns(ctx context.Context, tenant string, f RunFilter) ([]Run, error)
	GetTrace(ctx context.Context, tenant, traceID string, maxSteps int) (*Trace, error)
	GetStep(ctx context.Context, tenant, traceID, stepID string) (*StepDetail, error)
	Overview(ctx context.Context, tenant string, from, to time.Time, bucket time.Duration) (*Overview, error)
	Loops(ctx context.Context, tenant string, from, to time.Time, minRepeats int) ([]Loop, error)
	Insights(ctx context.Context, tenant string, from, to time.Time) (*Insights, error)
	CostByAgent(ctx context.Context, tenant string, from, to time.Time) (map[string]float64, error)
	Models(ctx context.Context, tenant string, from, to time.Time) ([]ModelUsage, error)
	Tools(ctx context.Context, tenant string, from, to time.Time) ([]ToolHealth, error)
	// RunsForEval loads what checks need of each run (at most 500 runs).
	RunsForEval(ctx context.Context, tenant string, traceIDs []string) ([]evals.Run, error)
	// Scores aggregates bindle.score() results in a range per agent version.
	Scores(ctx context.Context, tenant, agent string, from, to time.Time) ([]VersionScore, error)
	ListSessions(ctx context.Context, tenant string, f SessionFilter) ([]Session, error)
	Search(ctx context.Context, tenant string, f SearchFilter) ([]SearchHit, error)
	// Feedback returns user feedback (bindle.feedback) per trace for the given runs.
	Feedback(ctx context.Context, tenant string, traceIDs []string) (map[string]FeedbackSummary, error)
	// RecentFeedback returns traces with user feedback in [from, to) (at most 1000).
	RecentFeedback(ctx context.Context, tenant string, from, to time.Time) (map[string]FeedbackSummary, error)
	// RunsByIDs returns run summaries for trace IDs (at most 1000).
	RunsByIDs(ctx context.Context, tenant string, traceIDs []string) ([]Run, error)
	AgentVersions(ctx context.Context, tenant, agent string, from, to time.Time) ([]VersionStats, error)
}

// ---- insights (the console's home page) ------------------------------------

// Insights answers "what needs attention?" for a time range.
type Insights struct {
	Agents   []AgentSummary `json:"agents"`
	Issues   []Issue        `json:"issues"`
	Outliers []Outlier      `json:"outliers"`
	Previous PeriodTotals   `json:"previous"` // the same-length range just before
}

// AgentSummary is one agent's runs in the range.
type AgentSummary struct {
	Agent        string  `json:"agent"`
	Runs         uint64  `json:"runs"`
	FailedRuns   uint64  `json:"failed_runs"`
	CostUSD      float64 `json:"cost_usd"`
	CostPartial  bool    `json:"cost_partial"`
	P95RunMs     float64 `json:"p95_run_ms"`
	LastRunStart Time    `json:"last_run_start"`
	MedianSteps  float64 `json:"median_steps"`
	MedianCost   float64 `json:"median_cost_usd"`
}

// Issue groups failed steps with the same name: one row per thing that
// keeps breaking, with a recent example to open.
type Issue struct {
	StepName     string `json:"step_name"`
	StepType     string `json:"step_type"`
	Failures     uint64 `json:"failures"`
	Runs         uint64 `json:"runs"` // distinct runs affected
	LastSeen     Time   `json:"last_seen"`
	SampleError  string `json:"sample_error"`
	SampleTrace  string `json:"sample_trace_id"`
	SampleStepID string `json:"sample_step_id"`
	// Patterns splits the failures by error shape (see normalize.ErrorPattern),
	// most frequent first; at most 3.
	Patterns []ErrorPattern `json:"patterns"`
}

// ErrorPatternCount is how often one error pattern occurred in a range.
type ErrorPatternCount struct {
	Pattern     string `json:"pattern"`
	StepName    string `json:"step_name"`
	Failures    uint64 `json:"failures"`
	SampleTrace string `json:"sample_trace_id"`
	SampleStep  string `json:"sample_step_id"`
}

// ErrorPattern is one distinct error within an Issue.
type ErrorPattern struct {
	Pattern     string `json:"pattern"`
	Failures    uint64 `json:"failures"`
	SampleTrace string `json:"sample_trace_id"`
	SampleStep  string `json:"sample_step_id"`
}

// Outlier is a run far above its agent's typical step count or cost: a
// likely runaway or stuck agent.
type Outlier struct {
	Run     Run     `json:"run"`
	Reason  string  `json:"reason"`  // "steps" or "cost"
	Value   float64 `json:"value"`   // this run's steps or cost
	Typical float64 `json:"typical"` // the agent's median
}

// PeriodTotals are headline numbers for comparison.
type PeriodTotals struct {
	Runs       uint64  `json:"runs"`
	FailedRuns uint64  `json:"failed_runs"`
	CostUSD    float64 `json:"cost_usd"`
	P95RunMs   float64 `json:"p95_run_ms"`
}

// Time is a UTC timestamp with nanosecond precision.
type Time struct{ time.Time }

// ---- runs ----------------------------------------------------------------

// RunFilter selects runs. From/To are required; zero-value strings mean "any".
type RunFilter struct {
	From, To    time.Time
	Agent       string
	Version     string // agent version; "" = any
	SessionID   string // "" = any
	Environment string
	Status      string // "", "failed", "ok"
	Limit       int
	// Keyset cursor: return runs strictly older than (AfterStart, AfterTrace).
	AfterStart time.Time
	AfterTrace string
}

// Run is one row of the runs list / trace summary.
type Run struct {
	TraceID          string  `json:"trace_id"`
	Started          Time    `json:"started"`
	Steps            uint64  `json:"steps"`
	FailedSteps      uint64  `json:"failed"`
	CostUSD          float64 `json:"cost_usd"`
	CostUnknownSteps uint64  `json:"cost_unknown_steps"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	LLMCalls         int64   `json:"llm_calls"`
	ToolCalls        int64   `json:"tool_calls"`
	PeakInputTokens  int64   `json:"peak_input_tokens"`
	Agent            string  `json:"agent"`
	AgentVersion     string  `json:"agent_version"`
	Framework        string  `json:"framework"`
	Environment      string  `json:"environment"`
	SessionID        string  `json:"session_id"`
	RootStatus       string  `json:"root_status"`
	DurationMs       float64 `json:"duration_ms"`
}

// ---- trace waterfall -----------------------------------------------------

// StepSummary is a waterfall row: everything but the payloads.
type StepSummary struct {
	StepID         string  `json:"step_id"`
	ParentStepID   string  `json:"parent_step_id"`
	Name           string  `json:"name"`
	Type           string  `json:"type"`
	Status         string  `json:"status"`
	Attempt        int64   `json:"attempt"`
	StartTime      Time    `json:"start_time"`
	DurationMs     float64 `json:"duration_ms"`
	Provider       string  `json:"provider"`
	Model          string  `json:"model"`
	ToolName       string  `json:"tool_name"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	CachedTokens   int64   `json:"cached_tokens"`
	CostUSD        float64 `json:"cost_usd"`
	CostKnown      bool    `json:"cost_known"`
	ErrorMessage   string  `json:"error_message"`
	InputBytes     uint64  `json:"input_bytes"`
	OutputBytes    uint64  `json:"output_bytes"`
	ReasoningBytes uint64  `json:"reasoning_bytes"`
	Redacted       bool    `json:"redacted"`
}

// Trace is a run summary plus its steps.
type Trace struct {
	Run       Run
	Steps     []StepSummary
	Truncated bool
}

// ---- step detail ---------------------------------------------------------

// StepDetail is one step with payloads.
type StepDetail struct {
	StepSummary
	TraceID       string            `json:"trace_id"`
	Framework     string            `json:"framework"`
	Input         string            `json:"input"`
	Output        string            `json:"output"`
	Reasoning     string            `json:"reasoning"`
	Attributes    map[string]string `json:"attributes"`
	Links         []string          `json:"links"`
	RedactedKinds []string          `json:"redacted_kinds"`
	// TruncatedFields lists fields shown only in part (very large, or the
	// full payload could not be loaded).
	TruncatedFields []string `json:"truncated_fields,omitempty"`
}

// ---- overview ------------------------------------------------------------

// Overview is the dashboard KPI block for a window.
type Overview struct {
	Runs             uint64        `json:"runs"`
	FailedRuns       uint64        `json:"failed_runs"`
	P50RunMs         float64       `json:"p50_run_ms"`
	P95RunMs         float64       `json:"p95_run_ms"`
	CostUSD          float64       `json:"cost_usd"`
	CostUnknownSteps uint64        `json:"cost_unknown_steps"`
	InputTokens      int64         `json:"input_tokens"`
	OutputTokens     int64         `json:"output_tokens"`
	Steps            uint64        `json:"steps"`
	FailedSteps      uint64        `json:"failed_steps"`
	Series           []SeriesPoint `json:"-"`
	TopFailingTools  []ToolFailure `json:"-"`
}

// SeriesPoint is one time bucket.
type SeriesPoint struct {
	Bucket     Time    `json:"bucket"`
	Runs       uint64  `json:"runs"`
	FailedRuns uint64  `json:"failed_runs"`
	CostUSD    float64 `json:"cost_usd"`
}

// ToolFailure counts failures for one tool.
type ToolFailure struct {
	ToolName string `json:"tool_name"`
	Failures uint64 `json:"failures"`
	Calls    uint64 `json:"calls"`
}

// ---- loop detection ------------------------------------------------------

// Loop is a tool called with the same input repeatedly within one trace.
type Loop struct {
	TraceID   string `json:"trace_id"`
	ToolName  string `json:"tool_name"`
	Repeats   uint64 `json:"repeats"`
	FirstSeen Time   `json:"first_seen"`
	LastSeen  Time   `json:"last_seen"`
}

// ---- models ----------------------------------------------------------------

// ModelUsage is one provider/model's LLM calls in a range.
type ModelUsage struct {
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	Calls        uint64  `json:"calls"`
	Unpriced     uint64  `json:"unpriced_calls"`
	CostUSD      float64 `json:"cost_usd"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
}

// ---- tools -------------------------------------------------------------------

// ToolHealth is one tool's calls in a range.
type ToolHealth struct {
	ToolName       string  `json:"tool_name"`
	Framework      string  `json:"framework"` // most common framework that called it
	Calls          uint64  `json:"calls"`
	Failures       uint64  `json:"failures"`
	ArgErrors      uint64  `json:"argument_errors"` // failures that mention invalid arguments or schema validation
	Runs           uint64  `json:"runs"`
	P50Ms          float64 `json:"p50_ms"`
	P95Ms          float64 `json:"p95_ms"`
	AvgOutputBytes float64 `json:"avg_output_bytes"`
	MaxOutputBytes float64 `json:"max_output_bytes"`
	TopError       string  `json:"top_error"` // most common error pattern
	SampleTrace    string  `json:"sample_trace_id"`
	SampleStep     string  `json:"sample_step_id"` // latest failure, else latest call
}

// ---- agent versions ------------------------------------------------------------

// VersionStats compares one agent version's runs.
type VersionStats struct {
	Version       string  `json:"version"`
	Runs          uint64  `json:"runs"`
	FailedRuns    uint64  `json:"failed_runs"`
	CostUSD       float64 `json:"cost_usd"`
	CostPartial   bool    `json:"cost_partial"`
	P50RunMs      float64 `json:"p50_run_ms"`
	P95RunMs      float64 `json:"p95_run_ms"`
	AvgSteps      float64 `json:"avg_steps"`
	AvgLLMCalls   float64 `json:"avg_llm_calls"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	CachedTokens  int64   `json:"cached_tokens"`
	ToolCalls     uint64  `json:"tool_calls"`
	ToolFailures  uint64  `json:"tool_failures"`
	FirstRunStart Time    `json:"first_run_start"`
	LastRunStart  Time    `json:"last_run_start"`
}

// VersionScore is one score name's results for one agent version
// (bindle.score() from the SDKs).
type VersionScore struct {
	Version string  `json:"version"`
	Name    string  `json:"name"`
	Count   uint64  `json:"count"`
	Passed  uint64  `json:"passed"`
	Failed  uint64  `json:"failed"`
	Average float64 `json:"average"` // of numeric values; 0 when none
	Valued  uint64  `json:"valued"`  // how many had a numeric value
}

// ---- sessions ----------------------------------------------------------------

// SessionFilter selects sessions by their latest run in [From, To).
type SessionFilter struct {
	From, To time.Time
	Agent    string
	Limit    int
}

// Session is a conversation: the runs that share a session ID.
type Session struct {
	SessionID    string  `json:"session_id"`
	Agent        string  `json:"agent"`
	Runs         uint64  `json:"runs"`
	FailedRuns   uint64  `json:"failed_runs"`
	CostUSD      float64 `json:"cost_usd"`
	CostPartial  bool    `json:"cost_partial"`
	Steps        int64   `json:"steps"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	DurationMs   float64 `json:"duration_ms"` // sum of run durations
	FirstStart   Time    `json:"first_start"`
	LastStart    Time    `json:"last_start"`
	LastFailed   bool    `json:"last_failed"` // the latest run failed
}

// FeedbackSummary counts user feedback on one run.
type FeedbackSummary struct {
	Positive uint64   `json:"positive"`
	Negative uint64   `json:"negative"`
	Count    uint64   `json:"count"`
	Average  *float64 `json:"average,omitempty"` // of ratings, when any
}

// ---- search ------------------------------------------------------------------

// SearchFilter is a full-text search over step content and errors.
type SearchFilter struct {
	Query    string // simple query syntax: words, "phrases", -exclusions
	From, To time.Time
	Agent    string
	Type     string // step type; "" = any
	Status   string // "failure" or "" = any
	Limit    int
}

// SearchHit is the best-matching step of one run.
type SearchHit struct {
	TraceID      string  `json:"trace_id"`
	StepID       string  `json:"step_id"`
	StepName     string  `json:"step_name"`
	StepType     string  `json:"step_type"`
	Status       string  `json:"status"`
	Agent        string  `json:"agent"`
	AgentVersion string  `json:"agent_version"`
	SessionID    string  `json:"session_id"`
	Start        Time    `json:"start"`
	Field        string  `json:"field"`   // input, output, error or reasoning
	Snippet      string  `json:"snippet"` // matches wrapped in \ue000 … \ue001
	Score        float64 `json:"score"`
}
