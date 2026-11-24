// Package cassette builds a recorded run for replay with recorded tool
// outputs: its input, final output and every tool call with its arguments
// and recorded result. The SDKs (bindle.Cassette) read it. It holds only
// what Bindle stored, which was redacted at ingest.
package cassette

import (
	"context"
	"sync"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/evals"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

// Version is the cassette format version.
const Version = 1

// MaxTools caps the tool calls in one cassette.
const MaxTools = 500

// Call is one recorded tool call, in the order the run made them.
type Call struct {
	StepID    string `json:"step_id"`
	Tool      string `json:"tool"`
	Arguments string `json:"arguments"` // as captured (usually JSON text)
	Result    string `json:"result"`
	Status    string `json:"status"` // success, failure or unset
	Error     string `json:"error,omitempty"`
}

// Cassette is a recorded run.
type Cassette struct {
	Format       string `json:"format"` // "bindle.cassette"
	Version      int    `json:"version"`
	TraceID      string `json:"trace_id"`
	Agent        string `json:"agent"`
	AgentVersion string `json:"agent_version"`
	RecordedAt   string `json:"recorded_at"`
	Input        string `json:"input"`
	Output       string `json:"output"`
	ToolCalls    []Call `json:"tool_calls"`
	// Incomplete lists why the cassette may not cover the whole run:
	// "steps_truncated", "tool_calls_capped", "content_missing",
	// "content_truncated".
	Incomplete []string `json:"incomplete"`
}

// Reader is what Build reads (query.Store).
type Reader interface {
	GetTrace(ctx context.Context, tenant, traceID string, maxSteps int) (*query.Trace, error)
	GetStep(ctx context.Context, tenant, traceID, stepID string) (*query.StepDetail, error)
	RunsForEval(ctx context.Context, tenant string, traceIDs []string) ([]evals.Run, error)
}

// Build returns the cassette for one run, or nil when the run isn't stored.
func Build(ctx context.Context, rd Reader, tenant, traceID string, maxSteps int) (*Cassette, error) {
	if maxSteps <= 0 {
		maxSteps = 5000
	}
	tr, err := rd.GetTrace(ctx, tenant, traceID, maxSteps)
	if err != nil || tr == nil {
		return nil, err
	}
	c := &Cassette{Format: "bindle.cassette", Version: Version, TraceID: traceID, Agent: tr.Run.Agent,
		AgentVersion: tr.Run.AgentVersion, RecordedAt: tr.Run.Started.Format(time.RFC3339Nano),
		ToolCalls: []Call{}, Incomplete: []string{}}
	if tr.Truncated {
		c.Incomplete = append(c.Incomplete, "steps_truncated")
	}
	runs, err := rd.RunsForEval(ctx, tenant, []string{traceID})
	if err != nil {
		return nil, err
	}
	if len(runs) > 0 {
		c.Input, c.Output = runs[0].Input, runs[0].Output
	}

	var tools []query.StepSummary
	for _, st := range tr.Steps { // GetTrace returns steps in start order
		if st.Type == "tool_call" {
			tools = append(tools, st)
		}
	}
	if len(tools) > MaxTools {
		tools = tools[:MaxTools]
		c.Incomplete = append(c.Incomplete, "tool_calls_capped")
	}
	details := make([]*query.StepDetail, len(tools))
	errs := make([]error, len(tools))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range tools {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			details[i], errs[i] = rd.GetStep(ctx, tenant, traceID, tools[i].StepID)
		}(i)
	}
	wg.Wait()
	missing, partial := false, false
	calls := make([]Call, len(tools))
	for i, st := range tools {
		if errs[i] != nil {
			return nil, errs[i]
		}
		call := Call{StepID: st.StepID, Tool: st.ToolName, Status: st.Status, Error: st.ErrorMessage}
		if call.Tool == "" {
			call.Tool = st.Name
		}
		if d := details[i]; d != nil {
			call.Arguments, call.Result = d.Input, d.Output
			if len(d.TruncatedFields) > 0 {
				partial = true
			}
		}
		if call.Arguments == "" && call.Result == "" {
			missing = true
		}
		calls[i] = call
	}
	c.ToolCalls = calls
	if missing {
		c.Incomplete = append(c.Incomplete, "content_missing")
	}
	if partial {
		c.Incomplete = append(c.Incomplete, "content_truncated")
	}
	return c, nil
}
