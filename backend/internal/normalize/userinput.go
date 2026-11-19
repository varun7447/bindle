package normalize

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/bindle-ai/bindle/backend/internal/model"
)

// maxUserInput caps the stored end-user input: enough to see what was
// asked, small enough to aggregate over (topics and drift).
const maxUserInput = 2 * 1024

// userInput is what the end user asked in this step: the last user
// message of an LLM call's prompt, or an agent span's plain-text input.
// raw is the step input before system instructions are joined in; evIn
// are message events. Returns "" when no user message can be told apart
// (for example a plain-text prompt that may hold system text and history).
func userInput(stepType, raw string, evIn []eventMessage, attrs map[string]any) string {
	switch stepType {
	case model.StepLLM, model.StepAgent:
	default:
		return ""
	}
	if u := lastUserJSON(raw); u != "" {
		return u
	}
	for i := len(evIn) - 1; i >= 0; i-- {
		if evIn[i].Role == "user" {
			if t := strings.TrimSpace(contentText(evIn[i].Content)); t != "" {
				return t
			}
		}
	}
	if u := lastUserOpenInference(attrs); u != "" {
		return u
	}
	if stepType == model.StepAgent {
		t := strings.TrimSpace(raw)
		if t != "" && !strings.HasPrefix(t, "{") && !strings.HasPrefix(t, "[") {
			return t
		}
	}
	return ""
}

// lastUserJSON reads a message list in the shapes frameworks send:
// [{role, content|parts}], {messages: [...]}, LangChain's serialized
// messages ({id: [..., "HumanMessage"], kwargs: {content}}).
func lastUserJSON(raw string) string {
	t := strings.TrimSpace(raw)
	if t == "" || (t[0] != '[' && t[0] != '{') {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(t), &v); err != nil {
		return ""
	}
	if m, ok := v.(map[string]any); ok {
		for _, k := range []string{"messages", "input", "inputs"} {
			if inner, ok := m[k]; ok {
				v = inner
				break
			}
		}
		// {"messages": [[...]]} (LangChain batches)
		if arr, ok := v.([]any); ok && len(arr) == 1 {
			if inner, ok := arr[0].([]any); ok {
				v = inner
			}
		}
	}
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	for i := len(arr) - 1; i >= 0; i-- {
		m, ok := arr[i].(map[string]any)
		if !ok {
			continue
		}
		if !isUserRole(m) {
			continue
		}
		var c any = m["content"]
		if c == nil {
			c = m["parts"]
		}
		if kw, ok := m["kwargs"].(map[string]any); ok && c == nil {
			c = kw["content"]
		}
		if s := strings.TrimSpace(contentText(c)); s != "" {
			return s
		}
	}
	return ""
}

func isUserRole(m map[string]any) bool {
	switch r := strings.ToLower(asString(m["role"])); r {
	case "user", "human":
		return true
	case "":
	default:
		return false
	}
	if t := strings.ToLower(asString(m["type"])); t == "human" || t == "user" {
		return true
	}
	if ids, ok := m["id"].([]any); ok && len(ids) > 0 {
		return asString(ids[len(ids)-1]) == "HumanMessage"
	}
	return false
}

// contentText joins the text parts of a message's content: a string, or
// parts such as {type: "text", text|content}. Tool results, images and
// tool calls are skipped.
func contentText(c any) string {
	switch t := c.(type) {
	case string:
		return t
	case json.RawMessage:
		var v any
		if json.Unmarshal(t, &v) == nil {
			return contentText(v)
		}
		return string(t)
	case []any:
		var parts []string
		for _, p := range t {
			if s := contentText(p); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		switch strings.ToLower(asString(t["type"])) {
		case "", "text", "input_text":
		default:
			return ""
		}
		for _, k := range []string{"text", "content"} {
			if s, ok := t[k].(string); ok {
				return s
			}
		}
	}
	return ""
}

// lastUserOpenInference reads OpenInference's flattened messages:
// llm.input_messages.<i>.message.role / .content.
func lastUserOpenInference(attrs map[string]any) string {
	best, text := -1, ""
	for k, v := range attrs {
		if !strings.HasPrefix(k, "llm.input_messages.") || !strings.HasSuffix(k, ".message.role") {
			continue
		}
		idx := strings.TrimSuffix(strings.TrimPrefix(k, "llm.input_messages."), ".message.role")
		i, err := strconv.Atoi(idx)
		if err != nil || i <= best || !strings.EqualFold(asString(v), "user") {
			continue
		}
		if c := strings.TrimSpace(asString(attrs["llm.input_messages."+idx+".message.content"])); c != "" {
			best, text = i, c
		}
	}
	return text
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
