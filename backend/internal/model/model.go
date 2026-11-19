// Package model holds Bindle's internal data types: the raw span decoded from
// OTLP, and the canonical Step that every framework is normalized into.
package model

// Span is a framework-agnostic view of one OTLP span, before normalization.
type Span struct {
	TraceID       string // lowercase hex
	SpanID        string // lowercase hex
	ParentSpanID  string // lowercase hex, empty for root spans
	Name          string
	Kind          int32
	StartUnixNano uint64
	EndUnixNano   uint64
	StatusCode    int32 // 0 unset, 1 ok, 2 error
	StatusMessage string
	Attributes    map[string]any
	ResourceAttrs map[string]any
	ScopeName     string
	ScopeVersion  string
	Events        []Event
	Links         []Link
}

// Event is an OTLP span event.
type Event struct {
	TimeUnixNano uint64
	Name         string
	Attributes   map[string]any
}

// Link is an OTLP span link (used for multi-agent handoffs across traces).
type Link struct {
	TraceID    string
	SpanID     string
	Attributes map[string]any
}

// Step types in Bindle's canonical schema.
const (
	StepAgent     = "agent"
	StepLLM       = "llm_call"
	StepTool      = "tool_call"
	StepRetrieval = "retrieval"
	StepDecision  = "decision"
	StepHandoff   = "handoff"
	StepEval      = "eval"
	StepGuardrail = "guardrail"
	StepOther     = "other"
)

// Step statuses.
const (
	StatusSuccess = "success"
	StatusFailure = "failure"
	StatusUnset   = "unset"
)

// Step is one normalized agent step. The tenant is always set by the ingest
// edge from the API key and never read from the payload.
type Step struct {
	TenantID     string
	TraceID      string
	StepID       string
	ParentStepID string
	Name         string
	Type         string
	Status       string
	Attempt      int64

	Framework    string
	ServiceName  string
	AgentName    string
	AgentVersion string
	SessionID    string
	Environment  string

	Provider     string
	Model        string
	ToolName     string
	InputTokens  int64
	OutputTokens int64
	CachedTokens int64
	CostUSD      float64
	CostKnown    bool

	StartUnixNano uint64
	DurationMs    float64

	ErrorMessage string
	// ErrorPattern is ErrorMessage with IDs, numbers and quoted values
	// replaced, so the same failure groups together across runs.
	ErrorPattern string
	// Scores (eval steps from bindle.score()).
	ScoreName   string
	ScoreValue  *float64
	ScorePass   string // "pass", "fail" or ""
	ScoreSource string // "user" for bindle.feedback(), else ""
	Input       string
	Output      string
	Reasoning   string

	Attributes map[string]string
	Links      []string // "traceid:spanid"
}
