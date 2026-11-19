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
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bindle-ai/bindle/backend/internal/cost"
	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/otlp"
	"github.com/bindle-ai/bindle/backend/internal/redact"
	"github.com/bindle-ai/bindle/backend/internal/security"
)

const (
	// Per-field cap when there is no payload store. With one, ingest sets
	// Normalizer.MaxPayloadBytes higher and large fields go to S3.
	defaultMaxPayload = 32 * 1024
	maxAttrValue      = 2 * 1024
	maxAttrs          = 256
)

// Normalizer converts spans to steps.
type Normalizer struct {
	Prices          *cost.Table
	Redactor        *redact.Redactor
	MaxPayloadBytes int // input/output/reasoning cap; default 32 KiB
}

// Keys pulled into first-class fields; everything else lands in Attributes.
// These are content: they become Input/Output (so the workspace's capture
// setting and redaction apply) and never stay in Attributes.
var extracted = map[string]bool{
	"input.value": true, "output.value": true, "gen_ai.prompt": true, "gen_ai.completion": true,
	"bindle.input": true, "bindle.output": true, "bindle.reasoning": true,
	// OpenTelemetry GenAI conventions (Microsoft Agent Framework, Strands
	// with the latest conventions, others).
	"gen_ai.input.messages": true, "gen_ai.output.messages": true, "gen_ai.system_instructions": true,
	"gen_ai.tool.call.arguments": true, "gen_ai.tool.call.result": true,
}

// Span events that carry message content (GenAI event conventions: Strands,
// Azure AI Foundry Agent Service). Inputs are joined in order.
var inputEvents = map[string]bool{
	"gen_ai.system.message": true, "gen_ai.user.message": true, "gen_ai.assistant.message": true, "gen_ai.tool.message": true,
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
		// The name given to bindle.init() wins: frameworks also tag spans with
		// their own (sub-)agent names, which differ within one run.
		AgentName:    first(str(a, "bindle.agent.name"), str(res, "bindle.agent.name"), str(a, "gen_ai.agent.name")),
		AgentVersion: first(str(a, "bindle.agent.version"), str(res, "bindle.agent.version", "service.version")),
		SessionID:    str(a, "bindle.session.id", "session.id", "gen_ai.conversation.id"),
		Environment:  str(res, "deployment.environment.name", "deployment.environment"),
		Provider:     str(a, "gen_ai.provider.name", "gen_ai.system", "llm.provider", "llm.system"),
		Model:        str(a, "gen_ai.response.model", "gen_ai.request.model", "llm.model_name"),
		ToolName:     str(a, "gen_ai.tool.name", "tool.name", "bindle.tool.name"),
		InputTokens:  num(a, "gen_ai.usage.input_tokens", "gen_ai.usage.prompt_tokens", "llm.token_count.prompt"),
		OutputTokens: num(a, "gen_ai.usage.output_tokens", "gen_ai.usage.completion_tokens", "llm.token_count.completion"),
		CachedTokens: num(a, "gen_ai.usage.cache_read_input_tokens", "llm.token_count.prompt_details.cache_read"),
		Attempt:      num(a, "bindle.attempt"),
		Input:        str(a, "bindle.input", "input.value", "gen_ai.prompt", "gen_ai.input.messages", "gen_ai.tool.call.arguments"),
		Output:       str(a, "bindle.output", "output.value", "gen_ai.completion", "gen_ai.output.messages", "gen_ai.tool.call.result"),
		Reasoning:    str(a, "bindle.reasoning"),
	}
	if s.EndUnixNano > s.StartUnixNano {
		st.DurationMs = float64(s.EndUnixNano-s.StartUnixNano) / 1e6
	}

	rawInput := st.Input
	if sys := str(a, "gen_ai.system_instructions"); sys != "" {
		st.Input = joinNonEmpty("system: "+sys, st.Input)
	}

	// Older GenAI conventions put prompt/completion on events.
	var evIn []eventMessage
	for _, e := range s.Events {
		if inputEvents[e.Name] {
			if c := str(e.Attributes, "content", "gen_ai.event.content"); c != "" {
				evIn = append(evIn, eventMessage{Role: eventRole(e.Name), Content: rawOrString(c)})
			}
			continue
		}
		switch e.Name {
		case "gen_ai.choice":
			if st.Output == "" {
				st.Output = str(e.Attributes, "message", "gen_ai.event.content")
			}
			continue
		}
		if st.Input == "" {
			st.Input = str(e.Attributes, "gen_ai.input.messages")
		}
		if st.Output == "" {
			st.Output = str(e.Attributes, "gen_ai.output.messages")
		}
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

	if st.Input == "" && len(evIn) > 0 {
		// One JSON array of {role, content}, so the console can show it as a
		// conversation (the events themselves don't repeat the role).
		if b, err := json.Marshal(evIn); err == nil {
			st.Input = string(b)
		}
	}

	if rawInput == "" && !strings.HasPrefix(st.Input, "system: ") {
		rawInput = st.Input // from events
	}

	st.Framework = detectFramework(s.ScopeName, res, a)
	if st.Framework == "unknown" && isAgentCore(s) {
		st.Framework = "Amazon Bedrock AgentCore"
	}
	st.Type = classify(s, st)
	if st.Type != model.StepLLM {
		// Agent spans often repeat their LLM calls' total usage; counting it
		// again would double the run's tokens. The raw values stay in
		// Attributes.
		st.InputTokens, st.OutputTokens, st.CachedTokens = 0, 0, 0
	}
	applyScore(&st, a)
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
	maxPayload := n.MaxPayloadBytes
	if maxPayload <= 0 {
		maxPayload = defaultMaxPayload
	}
	st.UserInput = truncate(r.Apply(userInput(st.Type, rawInput, evIn, a)), maxUserInput)
	in, out := r.Apply(st.Input), r.Apply(st.Output)
	sec := security.Detect(st.Type, st.ToolName, in, out)
	st.Input = truncate(in, maxPayload)
	st.Output = truncate(out, maxPayload)
	st.Reasoning = truncate(r.Apply(st.Reasoning), maxPayload)
	st.ErrorMessage = truncate(r.Apply(st.ErrorMessage), maxAttrValue)
	st.ErrorPattern = ErrorPattern(st.ErrorMessage)
	for k, v := range st.Attributes {
		if strings.HasPrefix(k, securityAttrPrefix) {
			delete(st.Attributes, k) // set by Bindle only, never by the payload
			continue
		}
		st.Attributes[k] = r.Apply(v)
	}
	if !sec.Empty() {
		st.SecurityFlags, st.SecurityReasons = sec.Flags, sec.Reasons
		st.Attributes[SecurityReasonsAttr] = strings.Join(sec.Reasons, ",")
		if sec.Secrets > 0 {
			st.Attributes[SecuritySecretsAttr] = strconv.Itoa(sec.Secrets)
		}
	}
	return st
}

// Attributes Bindle sets on flagged steps: why each flag was raised
// ("flag:reason,...") and how many credentials were redacted from the
// output. They carry no content.
const (
	securityAttrPrefix  = "bindle.security."
	SecurityReasonsAttr = "bindle.security.reasons"
	SecuritySecretsAttr = "bindle.security.secrets"
)

func classify(s model.Span, st model.Step) string {
	a := s.Attributes
	if t := strings.ToLower(str(a, "bindle.step.type")); t != "" {
		return t
	}
	if isAgentCore(s) {
		if t, ok := agentCoreType(s); ok {
			return t
		}
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
	case "CHAIN":
		// Frameworks such as LangGraph mark the whole run as a CHAIN; the
		// root of a trace is the agent run. Nested chains (graph nodes)
		// stay generic steps.
		if s.ParentSpanID == "" {
			return model.StepAgent
		}
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
	case "invoke_workflow": // Google ADK's top-level span
		if s.ParentSpanID == "" {
			return model.StepAgent
		}
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
	case sc == "agent_framework", strings.HasPrefix(sc, "agent_framework."):
		return "Microsoft Agent Framework"
	case str(attrs, "gen_ai.system", "gen_ai.provider.name") == "az.ai.agents":
		return "Microsoft Foundry Agent Service"
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

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

// eventMessage is one GenAI message event, kept with its role.
type eventMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// eventRole maps gen_ai.user.message -> user, gen_ai.tool.message -> tool, ...
func eventRole(name string) string {
	return strings.TrimSuffix(strings.TrimPrefix(name, "gen_ai."), ".message")
}

// rawOrString keeps valid JSON as-is and quotes anything else.
func rawOrString(s string) json.RawMessage {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

// healthPaths are probe endpoints that platforms call constantly (AgentCore
// Runtime's GET /ping, Kubernetes and load balancer checks).
var healthPaths = map[string]bool{"/ping": true, "/health": true, "/healthz": true, "/livez": true, "/readyz": true, "/ready": true}

// IsHealthCheck reports whether a span is a standalone health probe: a root
// HTTP server span for a health path. Auto-instrumentation records one per
// probe; as runs they would only be noise.
func IsHealthCheck(s model.Span) bool {
	if s.ParentSpanID != "" {
		return false
	}
	a := s.Attributes
	path := str(a, "http.route", "url.path", "http.target")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if !healthPaths[strings.TrimRight(path, "/")] && !healthPaths[path] {
		return false
	}
	m := strings.ToUpper(str(a, "http.request.method", "http.method"))
	return m == "" || m == "GET" || m == "HEAD"
}

// AgentCore service spans (Gateway, Policy, Identity, built-in tools) reach
// Bindle through the CloudWatch span connector. They're named
// "AgentCore.<Service>.<Operation>"; aws.xray.origin is a fallback.
func isAgentCore(s model.Span) bool {
	return strings.HasPrefix(s.Name, "AgentCore.") ||
		strings.HasPrefix(str(s.Attributes, "aws.xray.origin"), "AWS::BedrockAgentCore")
}

func agentCoreType(s model.Span) (string, bool) {
	switch {
	case strings.HasPrefix(s.Name, "AgentCore.Policy."):
		return model.StepGuardrail, true
	case s.Kind == 3 && str(s.Attributes, "tool.name") != "":
		// A Gateway tool call emits a SERVER span (the call) and a CLIENT
		// span (the target it reached). Count the call once.
		return model.StepOther, true
	}
	return "", false
}

var (
	errUUID   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	errHex    = regexp.MustCompile(`\b(?:0x)?[0-9a-fA-F]{8,}\b`)
	errQuoted = regexp.MustCompile(`"[^"\n]{0,200}"|'[^'\n]{0,200}'`)
	errNumber = regexp.MustCompile(`\d+(?:\.\d+)?`)
	errSpace  = regexp.MustCompile(`\s+`)
)

const maxErrorPattern = 200

// ErrorPattern reduces an error message to its shape: the first line, with
// UUIDs, hex IDs, quoted values and numbers replaced by placeholders, so
// "timeout after 30s on order 1234" and "timeout after 31s on order 99"
// group together.
func ErrorPattern(msg string) string {
	if msg == "" {
		return ""
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	msg = errUUID.ReplaceAllString(msg, "<id>")
	msg = errQuoted.ReplaceAllString(msg, "<str>")
	msg = errHex.ReplaceAllStringFunc(msg, func(m string) string {
		// IDs mix digits and letters; plain words and numbers are left alone.
		h := strings.TrimPrefix(m, "0x")
		if strings.ContainsAny(h, "0123456789") && strings.ContainsAny(strings.ToLower(h), "abcdef") {
			return "<id>"
		}
		return m
	})
	msg = errNumber.ReplaceAllString(msg, "<n>")
	msg = strings.TrimSpace(errSpace.ReplaceAllString(msg, " "))
	if len(msg) > maxErrorPattern {
		cut := maxErrorPattern
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = msg[:cut]
	}
	return msg
}

// applyScore reads a bindle.score() span. A score sent after the run (from
// CI, with the run's trace ID) carries the sender's resource, not the run's:
// its agent fields are cleared so they never relabel the run.
func applyScore(st *model.Step, a map[string]any) {
	name := str(a, "bindle.score.name", "gen_ai.evaluation.name")
	if name == "" {
		return
	}
	if len(name) > 80 {
		name = name[:80]
	}
	st.Type = model.StepEval
	st.ScoreName = name
	v := a["bindle.score.value"]
	if v == nil {
		v = a["gen_ai.evaluation.score.value"]
	}
	switch v := v.(type) {
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			st.ScoreValue = &v
		}
	case int64:
		f := float64(v)
		st.ScoreValue = &f
	}
	switch p := a["bindle.score.passed"].(type) {
	case bool:
		if p {
			st.ScorePass = "pass"
		} else {
			st.ScorePass = "fail"
		}
	}
	// OpenTelemetry evaluation events carry a free-form label; read the
	// common pass/fail words, keep the rest as an attribute only.
	if st.ScorePass == "" {
		switch strings.ToLower(str(a, "gen_ai.evaluation.score.label")) {
		case "pass", "passed", "true", "yes", "good", "correct", "relevant":
			st.ScorePass = "pass"
		case "fail", "failed", "false", "no", "bad", "incorrect", "irrelevant":
			st.ScorePass = "fail"
		}
	}
	if st.Output == "" {
		st.Output = str(a, "gen_ai.evaluation.explanation")
	}
	if src := str(a, "bindle.score.source"); src == "user" {
		st.ScoreSource = "user"
	}
	if b, _ := a["bindle.score.external"].(bool); b {
		st.AgentName, st.AgentVersion, st.Environment, st.SessionID, st.Framework = "", "", "", "", ""
	}
}

// EvaluationSpans turns gen_ai.evaluation.result span events (the
// OpenTelemetry GenAI evaluation event) into span-shaped records parented to
// the evaluated span, so each is stored as an eval step of the run.
func EvaluationSpans(s model.Span) []model.Span {
	var out []model.Span
	for i, e := range s.Events {
		if e.Name != otlp.EvaluationEvent {
			continue
		}
		attrs := make(map[string]any, len(e.Attributes)+1)
		for k, v := range e.Attributes {
			attrs[k] = v
		}
		attrs["bindle.step.type"] = "eval"
		name, _ := attrs["gen_ai.evaluation.name"].(string)
		t := e.TimeUnixNano
		if t == 0 {
			t = s.EndUnixNano
		}
		out = append(out, model.Span{
			TraceID: s.TraceID, SpanID: otlp.DerivedSpanID(s.TraceID, s.SpanID, name, strconv.Itoa(i)), ParentSpanID: s.SpanID,
			Name: "evaluation " + name, StartUnixNano: t, EndUnixNano: t, Attributes: attrs, ResourceAttrs: s.ResourceAttrs,
			ScopeName: s.ScopeName, ScopeVersion: s.ScopeVersion,
		})
	}
	return out
}
