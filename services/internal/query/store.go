package query

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// Store is what the API needs from storage. Implemented by *Client.
type Store interface {
	ListRuns(ctx context.Context, tenant string, f RunFilter) ([]Run, error)
	GetTrace(ctx context.Context, tenant, traceID string, maxSteps int) (*Trace, error)
	GetStep(ctx context.Context, tenant, traceID, stepID string) (*StepDetail, error)
	Overview(ctx context.Context, tenant string, from, to time.Time, bucket time.Duration) (*Overview, error)
	Loops(ctx context.Context, tenant string, from, to time.Time, minRepeats int) ([]Loop, error)
}

var _ Store = (*Client)(nil)

// Time is a ClickHouse DateTime64 read in ISO format.
type Time struct{ time.Time }

// UnmarshalJSON parses "2026-11-16T23:03:58.503969000Z".
func (t *Time) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = v.UTC()
	return nil
}

// chTime formats a time as a DateTime64(9, 'UTC') parameter value.
func chTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000000000")
}

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

const runsSelect = `
SELECT trace_id, min(first_start) AS started, sum(steps) AS steps, sum(failed_steps) AS failed,
       sum(cost_usd) AS cost_usd, sum(cost_unknown_steps) AS cost_unknown_steps,
       sum(input_tokens) AS input_tokens, sum(output_tokens) AS output_tokens,
       max(agent_name) AS agent, max(agent_version) AS agent_version, max(framework) AS framework,
       max(environment) AS environment, max(session_id) AS session_id,
       max(root_status) AS root_status, max(root_duration) AS duration_ms
FROM bindle.traces
WHERE tenant_id = {tenant:String}
  AND start_hour >= toStartOfHour({from:DateTime64(9, 'UTC')}) AND start_hour <= {to:DateTime64(9, 'UTC')}
GROUP BY trace_id
HAVING started >= {from:DateTime64(9, 'UTC')} AND started < {to:DateTime64(9, 'UTC')}`

// Optional HAVING fragments. Each is constant SQL; only parameters vary.
const (
	havingAgent  = ` AND agent = {agent:String}`
	havingEnv    = ` AND environment = {env:String}`
	havingFailed = ` AND (failed > 0 OR root_status = 'failure')`
	havingOK     = ` AND failed = 0 AND root_status != 'failure'`
	havingCursor = ` AND (started < {after_start:DateTime64(9, 'UTC')} OR (started = {after_start:DateTime64(9, 'UTC')} AND trace_id < {after_trace:String}))`
	runsOrder    = `
ORDER BY started DESC, trace_id DESC
LIMIT {limit:UInt32}`
)

// ListRuns returns up to f.Limit runs, newest first.
func (c *Client) ListRuns(ctx context.Context, tenant string, f RunFilter) ([]Run, error) {
	if tenant == "" {
		return nil, ErrNoTenant
	}
	p := map[string]string{
		"tenant": tenant,
		"from":   chTime(f.From),
		"to":     chTime(f.To),
		"limit":  strconv.Itoa(f.Limit),
	}
	var sql strings.Builder
	sql.WriteString(runsSelect)
	if f.Agent != "" {
		sql.WriteString(havingAgent)
		p["agent"] = f.Agent
	}
	if f.Environment != "" {
		sql.WriteString(havingEnv)
		p["env"] = f.Environment
	}
	switch f.Status {
	case "failed":
		sql.WriteString(havingFailed)
	case "ok":
		sql.WriteString(havingOK)
	}
	if f.AfterTrace != "" {
		sql.WriteString(havingCursor)
		p["after_start"] = chTime(f.AfterStart)
		p["after_trace"] = f.AfterTrace
	}
	sql.WriteString(runsOrder)
	return selectRows[Run](ctx, c, sql.String(), p)
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

const traceSummarySQL = `
SELECT trace_id, min(start_time) AS started, count() AS steps, countIf(status = 'failure') AS failed,
       sum(cost_usd) AS cost_usd,
       countIf(NOT cost_known AND (model != '' OR type = 'llm_call')) AS cost_unknown_steps,
       sum(input_tokens) AS input_tokens, sum(output_tokens) AS output_tokens,
       max(agent_name) AS agent, max(agent_version) AS agent_version,
       maxIf(framework, framework != 'unknown') AS framework,
       max(environment) AS environment, max(session_id) AS session_id,
       maxIf(status, parent_step_id = '') AS root_status,
       maxIf(duration_ms, parent_step_id = '') AS duration_ms
FROM bindle.steps
WHERE tenant_id = {tenant:String} AND trace_id = {trace:String}
GROUP BY trace_id`

const traceStepsSQL = `
SELECT step_id, parent_step_id, name, type, status, attempt, start_time, duration_ms,
       provider, model, tool_name, input_tokens, output_tokens, cached_tokens, cost_usd, cost_known,
       error_message, length(input) AS input_bytes, length(output) AS output_bytes,
       length(reasoning) AS reasoning_bytes,
       toBool(position(input, '[REDACTED:') > 0 OR position(output, '[REDACTED:') > 0
         OR position(reasoning, '[REDACTED:') > 0 OR position(error_message, '[REDACTED:') > 0) AS redacted
FROM bindle.steps
WHERE tenant_id = {tenant:String} AND trace_id = {trace:String}
ORDER BY start_time, step_id
LIMIT {limit:UInt32}`

// GetTrace returns nil, nil when the trace does not exist for this tenant.
func (c *Client) GetTrace(ctx context.Context, tenant, traceID string, maxSteps int) (*Trace, error) {
	if tenant == "" {
		return nil, ErrNoTenant
	}
	p := map[string]string{"tenant": tenant, "trace": traceID}
	runs, err := selectRows[Run](ctx, c, traceSummarySQL, p)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	p["limit"] = strconv.Itoa(maxSteps + 1)
	steps, err := selectRows[StepSummary](ctx, c, traceStepsSQL, p)
	if err != nil {
		return nil, err
	}
	t := &Trace{Run: runs[0], Steps: steps}
	if len(steps) > maxSteps {
		t.Steps, t.Truncated = steps[:maxSteps], true
	}
	return t, nil
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
}

const stepDetailSQL = `
SELECT step_id, parent_step_id, name, type, status, attempt, start_time, duration_ms,
       provider, model, tool_name, input_tokens, output_tokens, cached_tokens, cost_usd, cost_known,
       error_message, length(input) AS input_bytes, length(output) AS output_bytes,
       length(reasoning) AS reasoning_bytes,
       trace_id, framework, input, output, reasoning, attributes, links,
       arraySort(arrayDistinct(extractAll(concat(input, '\n', output, '\n', reasoning, '\n', error_message),
         '\\[REDACTED:([A-Z0-9_]+)\\]'))) AS redacted_kinds,
       toBool(notEmpty(redacted_kinds)) AS redacted
FROM bindle.steps
WHERE tenant_id = {tenant:String} AND trace_id = {trace:String} AND step_id = {step:String}
LIMIT 1`

// GetStep returns nil, nil when the step does not exist for this tenant.
func (c *Client) GetStep(ctx context.Context, tenant, traceID, stepID string) (*StepDetail, error) {
	if tenant == "" {
		return nil, ErrNoTenant
	}
	rows, err := selectRows[StepDetail](ctx, c, stepDetailSQL,
		map[string]string{"tenant": tenant, "trace": traceID, "step": stepID})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
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

// runsInWindow is the per-run rollup the overview aggregates over.
const runsInWindow = `
SELECT trace_id, min(first_start) AS started, sum(steps) AS steps, sum(failed_steps) AS failed,
       sum(cost_usd) AS cost_usd, sum(cost_unknown_steps) AS cost_unknown_steps,
       sum(input_tokens) AS input_tokens, sum(output_tokens) AS output_tokens,
       max(root_status) AS root_status, max(root_duration) AS duration_ms
FROM bindle.traces
WHERE tenant_id = {tenant:String}
  AND start_hour >= toStartOfHour({from:DateTime64(9, 'UTC')}) AND start_hour <= {to:DateTime64(9, 'UTC')}
GROUP BY trace_id
HAVING started >= {from:DateTime64(9, 'UTC')} AND started < {to:DateTime64(9, 'UTC')}`

const overviewKPISQL = `
SELECT count() AS runs,
       countIf(failed > 0 OR root_status = 'failure') AS failed_runs,
       ifNotFinite(quantile(0.5)(duration_ms), 0) AS p50_run_ms,
       ifNotFinite(quantile(0.95)(duration_ms), 0) AS p95_run_ms,
       sum(cost_usd) AS cost_usd, sum(cost_unknown_steps) AS cost_unknown_steps,
       sum(input_tokens) AS input_tokens, sum(output_tokens) AS output_tokens,
       sum(steps) AS steps, sum(failed) AS failed_steps
FROM (` + runsInWindow + `)`

const overviewSeriesSQL = `
SELECT toStartOfInterval(started, INTERVAL {bucket_s:UInt32} SECOND) AS bucket,
       count() AS runs,
       countIf(failed > 0 OR root_status = 'failure') AS failed_runs,
       sum(cost_usd) AS cost_usd
FROM (` + runsInWindow + `)
GROUP BY bucket
ORDER BY bucket`

const topFailingToolsSQL = `
SELECT tool_name, countIf(status = 'failure') AS failures, count() AS calls
FROM bindle.steps
WHERE tenant_id = {tenant:String} AND type = 'tool_call' AND tool_name != ''
  AND start_time >= {from:DateTime64(9, 'UTC')} AND start_time < {to:DateTime64(9, 'UTC')}
GROUP BY tool_name
HAVING failures > 0
ORDER BY failures DESC, tool_name
LIMIT 10`

// Overview aggregates runs started in [from, to).
func (c *Client) Overview(ctx context.Context, tenant string, from, to time.Time, bucket time.Duration) (*Overview, error) {
	if tenant == "" {
		return nil, ErrNoTenant
	}
	p := map[string]string{
		"tenant":   tenant,
		"from":     chTime(from),
		"to":       chTime(to),
		"bucket_s": strconv.Itoa(int(bucket / time.Second)),
	}
	kpi, err := selectRows[Overview](ctx, c, overviewKPISQL, p)
	if err != nil {
		return nil, err
	}
	o := &Overview{}
	if len(kpi) > 0 {
		o = &kpi[0]
	}
	if o.Series, err = selectRows[SeriesPoint](ctx, c, overviewSeriesSQL, p); err != nil {
		return nil, err
	}
	if o.TopFailingTools, err = selectRows[ToolFailure](ctx, c, topFailingToolsSQL, p); err != nil {
		return nil, err
	}
	return o, nil
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

const loopsSQL = `
SELECT trace_id, tool_name, count() AS repeats, min(start_time) AS first_seen, max(start_time) AS last_seen
FROM bindle.steps
WHERE tenant_id = {tenant:String} AND type = 'tool_call'
  AND start_time >= {from:DateTime64(9, 'UTC')} AND start_time < {to:DateTime64(9, 'UTC')}
GROUP BY trace_id, tool_name, cityHash64(input)
HAVING repeats >= {min_repeats:UInt32}
ORDER BY repeats DESC, last_seen DESC
LIMIT 100`

// Loops finds runaway tool loops among steps started in [from, to).
func (c *Client) Loops(ctx context.Context, tenant string, from, to time.Time, minRepeats int) ([]Loop, error) {
	if tenant == "" {
		return nil, ErrNoTenant
	}
	return selectRows[Loop](ctx, c, loopsSQL, map[string]string{
		"tenant":      tenant,
		"from":        chTime(from),
		"to":          chTime(to),
		"min_repeats": strconv.Itoa(minRepeats),
	})
}
