// Package sink contains storage backends for normalized steps.
package sink

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	"github.com/bindle-ai/bindle/services/internal/model"
)

// Row is the JSON shape Stdout prints for each step (local debugging).
type Row struct {
	TenantID     string            `json:"tenant_id"`
	TraceID      string            `json:"trace_id"`
	StepID       string            `json:"step_id"`
	ParentStepID string            `json:"parent_step_id"`
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	Status       string            `json:"status"`
	Attempt      int64             `json:"attempt"`
	Framework    string            `json:"framework"`
	ServiceName  string            `json:"service_name"`
	AgentName    string            `json:"agent_name"`
	AgentVersion string            `json:"agent_version"`
	SessionID    string            `json:"session_id"`
	Environment  string            `json:"environment"`
	Provider     string            `json:"provider"`
	Model        string            `json:"model"`
	ToolName     string            `json:"tool_name"`
	InputTokens  int64             `json:"input_tokens"`
	OutputTokens int64             `json:"output_tokens"`
	CachedTokens int64             `json:"cached_tokens"`
	CostUSD      float64           `json:"cost_usd"`
	CostKnown    bool              `json:"cost_known"`
	StartTime    string            `json:"start_time"`
	DurationMs   float64           `json:"duration_ms"`
	ErrorMessage string            `json:"error_message"`
	Input        string            `json:"input"`
	Output       string            `json:"output"`
	Reasoning    string            `json:"reasoning"`
	Attributes   map[string]string `json:"attributes"`
	Links        []string          `json:"links"`
}

// ToRow converts a step to its storage row.
func ToRow(s model.Step) Row {
	attrs := s.Attributes
	if attrs == nil {
		attrs = map[string]string{}
	}
	links := s.Links
	if links == nil {
		links = []string{}
	}
	start := time.Unix(0, int64(s.StartUnixNano)).UTC().Format("2006-01-02 15:04:05.000000000")
	return Row{
		TenantID: s.TenantID, TraceID: s.TraceID, StepID: s.StepID, ParentStepID: s.ParentStepID,
		Name: s.Name, Type: s.Type, Status: s.Status, Attempt: s.Attempt,
		Framework: s.Framework, ServiceName: s.ServiceName, AgentName: s.AgentName,
		AgentVersion: s.AgentVersion, SessionID: s.SessionID, Environment: s.Environment,
		Provider: s.Provider, Model: s.Model, ToolName: s.ToolName,
		InputTokens: s.InputTokens, OutputTokens: s.OutputTokens, CachedTokens: s.CachedTokens,
		CostUSD: s.CostUSD, CostKnown: s.CostKnown, StartTime: start, DurationMs: s.DurationMs,
		ErrorMessage: s.ErrorMessage, Input: s.Input, Output: s.Output, Reasoning: s.Reasoning,
		Attributes: attrs, Links: links,
	}
}

// Stdout writes one JSON row per line (local debugging).
type Stdout struct {
	mu sync.Mutex
	W  io.Writer
}

// Write implements pipeline.Sink.
func (s *Stdout) Write(_ context.Context, steps []model.Step) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.W
	if w == nil {
		w = os.Stdout
	}
	enc := json.NewEncoder(w)
	for _, st := range steps {
		if err := enc.Encode(ToRow(st)); err != nil {
			return err
		}
	}
	return nil
}
