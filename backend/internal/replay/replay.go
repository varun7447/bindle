// Package replay runs one captured LLM step again, with edits, against a
// model the user picks, using the user's own provider key for that request
// only. The key is never stored or logged, and error messages are scrubbed
// of it. Providers are reached at fixed hosts; nothing is user-addressable.
package replay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Message is one turn of the conversation sent to the model.
type Message struct {
	Role    string `json:"role"` // system | user | assistant | tool
	Content string `json:"content"`
}

// Request is a replay. APIKey is used for this call only.
type Request struct {
	Provider    string    `json:"provider"` // "anthropic" or "openai"
	Model       string    `json:"model"`
	APIKey      string    `json:"-"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature *float64  `json:"temperature,omitempty"`
}

// Result is the model's answer and its usage.
type Result struct {
	Output       string  `json:"output"`
	Model        string  `json:"model"`
	StopReason   string  `json:"stop_reason"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	LatencyMs    float64 `json:"latency_ms"`
}

// Provider calls one model API.
type Provider interface {
	Run(ctx context.Context, model, apiKey, system string, msgs []Message, maxTokens int, temperature *float64) (Result, error)
}

// ProviderError is an error answer from the provider; Message is safe to show.
type ProviderError struct {
	Status  int
	Message string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider returned HTTP %d: %s", e.Status, e.Message)
}

// ErrUnsupported means the provider isn't available in this build.
var ErrUnsupported = errors.New("replay: provider not available")

const (
	MaxMessages      = 200
	MaxTotalChars    = 400_000
	DefaultMaxTokens = 2048
	MaxMaxTokens     = 16_000
	Timeout          = 2 * time.Minute
)

// Validate checks a request before any network call.
func Validate(r Request) error {
	if r.Provider != "anthropic" && r.Provider != "openai" {
		return errors.New("provider must be anthropic or openai")
	}
	if r.Model == "" || len(r.Model) > 200 || strings.ContainsAny(r.Model, " \t\r\n/?#") {
		return errors.New("model must be a model ID without spaces")
	}
	if len(r.APIKey) < 8 || len(r.APIKey) > 400 || strings.ContainsAny(r.APIKey, " \t\r\n") {
		return errors.New("enter your provider API key")
	}
	if len(r.Messages) == 0 || len(r.Messages) > MaxMessages {
		return fmt.Errorf("send 1 to %d messages", MaxMessages)
	}
	total := 0
	hasUser := false
	for i, m := range r.Messages {
		switch m.Role {
		case "system", "user", "assistant", "tool":
		default:
			return fmt.Errorf("message %d: role must be system, user, assistant or tool", i+1)
		}
		if !utf8.ValidString(m.Content) {
			return fmt.Errorf("message %d: invalid text", i+1)
		}
		total += len(m.Content)
		hasUser = hasUser || m.Role == "user" || m.Role == "tool"
	}
	if total > MaxTotalChars {
		return fmt.Errorf("the conversation is longer than %d characters", MaxTotalChars)
	}
	if !hasUser {
		return errors.New("add at least one user message")
	}
	if r.MaxTokens < 0 || r.MaxTokens > MaxMaxTokens {
		return fmt.Errorf("max_tokens must be between 1 and %d", MaxMaxTokens)
	}
	if t := r.Temperature; t != nil && (*t < 0 || *t > 2) {
		return errors.New("temperature must be between 0 and 2")
	}
	return nil
}

// Prepare turns captured messages into a system prompt and alternating
// user/assistant turns: system messages join the system prompt, tool
// results become user text (the tools themselves aren't replayed), and
// consecutive turns from the same side are merged. The conversation must
// start with the user.
func Prepare(msgs []Message) (string, []Message, error) {
	var system []string
	var out []Message
	for _, m := range msgs {
		role, text := m.Role, m.Content
		switch role {
		case "system":
			if strings.TrimSpace(text) != "" {
				system = append(system, text)
			}
			continue
		case "tool":
			role, text = "user", "Tool result:\n"+text
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content += "\n\n" + text
			continue
		}
		out = append(out, Message{Role: role, Content: text})
	}
	if len(out) == 0 || out[0].Role != "user" {
		return "", nil, errors.New("the conversation must start with a user message")
	}
	return strings.Join(system, "\n\n"), out, nil
}

// Runner dispatches to providers.
type Runner struct {
	Providers map[string]Provider
}

// Run validates, prepares and runs a replay. Errors never contain the key.
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	if err := Validate(req); err != nil {
		return Result{}, err
	}
	p := r.Providers[req.Provider]
	if p == nil {
		return Result{}, ErrUnsupported
	}
	system, msgs, err := Prepare(req.Messages)
	if err != nil {
		return Result{}, err
	}
	max := req.MaxTokens
	if max == 0 {
		max = DefaultMaxTokens
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	start := time.Now()
	res, err := p.Run(ctx, req.Model, req.APIKey, system, msgs, max, req.Temperature)
	if err != nil {
		var pe *ProviderError
		if errors.As(err, &pe) {
			return Result{}, &ProviderError{Status: pe.Status, Message: scrub(pe.Message, req.APIKey)}
		}
		return Result{}, errors.New(scrub(err.Error(), req.APIKey))
	}
	res.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	if res.Model == "" {
		res.Model = req.Model
	}
	return res, nil
}

func scrub(s, key string) string {
	if key != "" {
		s = strings.ReplaceAll(s, key, "[REDACTED:KEY]")
	}
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
