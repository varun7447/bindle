// Package normalize maps spans from any framework or semantic convention onto
// Bindle's canonical Step. This mapping is Bindle's core interoperability
// asset: version it per framework and cover it with contract tests.
//
// Conventions understood today:
//   - Bindle SDK attributes (bindle.*)
//   - OpenTelemetry GenAI semantic conventions (gen_ai.*), still evolving
//   - OpenInference (openinference.span.kind, llm.*, tool.*, input/output.value)
package normalize

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bindle-ai/bindle/services/internal/cost"
	"github.com/bindle-ai/bindle/services/internal/model"
	"github.com/bindle-ai/bindle/services/internal/redact"
)

const (
	maxPayloadChars = 32 * 1024 // larger payloads go to object storage (v0.2)
	maxAttrValue    = 2 * 1024
	maxAttrs        = 256
)

// Normalizer converts spans to steps.
type Normalizer struct {
	Prices   *cost.Table
	Redactor *redact.Redactor
}

// Keys pulled into first-class fields; everything else lands in Attributes.
var extracted = map[string]bool{
	"input.value": true, "output.value": true, "gen_ai.prompt": true, "gen_ai.completion": true,
	"bindle.input": true, "bindle.output": true, "bindle.reasoning": true,
}

// Normalize converts one span. tenantID comes from the authenticated API key.
func (n *Normalizer) Normalize(tenantID string, s model.Span) model.Step {
	a := s.Attributes
	res := s.ResourceAttrs

	st := model.Step{
		TenantID:      tenantID,
		TraceID:       s.TraceID,
		StepID:        s.SpanID,
		ParentStepID:  s.ParentSpanID,
		Name:          s.Name,
		StartUnixNano: s.StartUnixNano,
		ServiceName:   str(res, "service.name"),
		AgentName:     first(str(a, "bindle.agent.name", "gen_ai.agent.name"), str(res, "bindle.agent.name")),
		AgentVersion:  first(str(a, "bindle.agent.version"), str(res, "bindle.agent.version", "service.version")),
		SessionID:     str(a, "bindle.session.id", "session.id", "gen_ai.conversation.id"),
		Environment:   str(res, "deployment.environment.name", "deployment.environment"),
		Provider:      str(a, "gen_ai.provider.name", "gen_ai.system", "llm.provider", "llm.system"),
		Model:         str(a, "gen_ai.response.model", "gen_ai.request.model", "llm.model_name"),
		ToolName:      str(a, "gen_ai.tool.name", "tool.name", "bindle.tool.name"),
		InputTokens:   num(a, "gen_ai.usage.input_tokens", "gen_ai.usage.prompt_tokens", "llm.token_count.prompt"),
		OutputTokens:  num(a, "gen_ai.usage.output_tokens", "gen_ai.usage.completion_tokens", "llm.token_count.completion"),
		CachedTokens:  num(a, "gen_ai.usage.cache_read_input_tokens", "llm.token_count.prompt_details.cache_read"),
		Attempt:       num(a, "bindle.attempt"),
		Input:         str(a, "bindle.input", "input.value", "gen_ai.prompt"),
		Output:        str(a, "bindle.output", "output.value", "gen_ai.completion"),
		Reasoning:     str(a, "bindle.reasoning"),
	}
	if s.EndUnixNano > s.StartUnixNano {
		st.DurationMs = float64(s.EndUnixNano-s.StartUnixNano) / 1e6
	}

	// Older GenAI conventions put prompt/completion on events.
	for _, e := range s.Events {
		switch e.Name {
		case "gen_ai.content.prompt":
			if st.Input == "" {
				st.Input = str(e.Attributes, "gen_ai.prompt")
			}
		case "gen_ai.content.completion":
			if st.Output == "" {
				st.Output = str(e.Attributes, "gen_ai.completion")
			}
		case "exception":
			if st.ErrorMessage == "" {
				st.ErrorMessage = first(str(e.Attributes, "exception.message"), str(e.Attributes, "exception.type"))
			}
		}
	}

	st.Framework = detectFramework(s.ScopeName, res, a)
	st.Type = classify(s, st)
	st.Status = status(s)
	if st.Status == model.StatusFailure && st.ErrorMessage == "" {
		st.ErrorMessage = s.StatusMessage
	}
	if st.Type == model.StepLLM && n.Prices != nil {
		st.CostUSD, st.CostKnown = n.Prices.Compute(st.Provider, st.Model, st.InputTokens, st.OutputTokens, st.CachedTokens)
	}

	st.Attributes = flatten(a)
	for _, l := range s.Links {
		st.Links = append(st.Links, l.TraceID+":"+l.SpanID)
	}

	// Server-side second-pass redaction, then truncation.
	r := n.Redactor
	st.Input = truncate(r.Apply(st.Input), maxPayloadChars)
	st.Output = truncate(r.Apply(st.Output), maxPayloadChars)
	st.Reasoning = truncate(r.Apply(st.Reasoning), maxPayloadChars)
	st.ErrorMessage = truncate(r.Apply(st.ErrorMessage), maxAttrValue)
	for k, v := range st.Attributes {
		st.Attributes[k] = r.Apply(v)
	}
	return st
}

func classify(s model.Span, st model.Step) string {
	a := s.Attributes
	if t := strings.ToLower(str(a, "bindle.step.type")); t != "" {
		return t
	}
	switch strings.ToUpper(str(a, "openinference.span.kind")) {
	case "LLM", "EMBEDDING":
		return model.StepLLM
	case "TOOL":
		return model.StepTool
	case "RETRIEVER", "RERANKER":
		return model.StepRetrieval
	case "AGENT":
		return model.StepAgent
	case "EVALUATOR":
		return model.StepEval
	case "GUARDRAIL":
		return model.StepGuardrail
	}
	switch strings.ToLower(str(a, "gen_ai.operation.name")) {
	case "chat", "text_completion", "generate_content", "embeddings":
		return model.StepLLM
	case "execute_tool":
		return model.StepTool
	case "invoke_agent", "create_agent":
		return model.StepAgent
	}
	switch {
	case str(a, "bindle.handoff.to") != "":
		return model.StepHandoff
	case str(a, "bindle.decision.chosen") != "":
		return model.StepDecision
	case st.ToolName != "":
		return model.StepTool
	case st.Model != "" || st.InputTokens > 0:
		return model.StepLLM
	}
	return model.StepOther
}

func status(s model.Span) string {
	if strings.EqualFold(str(s.Attributes, "bindle.status"), "failure") {
		return model.StatusFailure
	}
	switch s.StatusCode {
	case 1:
		return model.StatusSuccess
	case 2:
		return model.StatusFailure
	}
	for _, e := range s.Events {
		if e.Name == "exception" {
			return model.StatusFailure
		}
	}
	return model.StatusSuccess
}

// detectFramework uses the instrumentation scope first, then resource hints.
// Scope names below are a starting point: verify against each framework release.
func detectFramework(scope string, res, attrs map[string]any) string {
	if f := str(attrs, "bindle.framework"); f != "" {
		return f
	}
	if f := str(res, "bindle.framework"); f != "" {
		return f
	}
	sc := strings.ToLower(scope)
	switch {
	case strings.Contains(sc, "strands"):
		return "Strands"
	case strings.Contains(sc, "langgraph"), strings.Contains(sc, "langchain"):
		return "LangChain/LangGraph"
	case strings.Contains(sc, "openai_agents"), strings.Contains(sc, "openai-agents"), strings.Contains(sc, "agents.tracing"):
		return "OpenAI Agents SDK"
	case strings.Contains(sc, "crewai"):
		return "CrewAI"
	case strings.Contains(sc, "llama_index"), strings.Contains(sc, "llamaindex"):
		return "LlamaIndex"
	case strings.Contains(sc, "pydantic"):
		return "Pydantic AI"
	case strings.Contains(sc, "autogen"):
		return "AutoGen"
	case strings.Contains(sc, "google.adk"), strings.Contains(sc, "gcp.vertex.agent"):
		return "Google ADK"
	case sc == "ai":
		return "Vercel AI SDK"
	case strings.HasPrefix(sc, "bindle"):
		return "Bindle SDK"
	}
	return "unknown"
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch t := v.(type) {
			case string:
				if t != "" {
					return t
				}
			case int64, float64, bool:
				return fmt.Sprint(t)
			default:
				b, err := json.Marshal(t)
				if err == nil {
					return string(b)
				}
			}
		}
	}
	return ""
}

func num(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch t := m[k].(type) {
		case int64:
			return t
		case float64:
			return int64(t)
		case string:
			if v, err := strconv.ParseInt(t, 10, 64); err == nil {
				return v
			}
		}
	}
	return 0
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func flatten(a map[string]any) map[string]string {
	keys := make([]string, 0, len(a))
	for k := range a {
		if !extracted[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > maxAttrs {
		keys = keys[:maxAttrs]
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = truncate(str(a, k), maxAttrValue)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// avoid cutting a UTF-8 sequence in half
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…[truncated]"
}
