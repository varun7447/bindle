// Package query defines what the console API reads from storage: the Store
// interface and its result types. internal/search implements it.
package query

import (
	"context"
	"errors"
	"time"
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
}

// ---- insights (the console's home page) ------------------------------------

// Insights answers "what needs attention?" for a time range.
type Insights struct {
	Agents   []AgentSummary `json:"agents"`
	Issues   []Issue        `json:"issues"`
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
