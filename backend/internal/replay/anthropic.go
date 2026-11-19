package replay

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// AnthropicBaseURL is the only host replays reach for Anthropic.
const AnthropicBaseURL = "https://api.anthropic.com"

// Anthropic calls the Messages API through Anthropic's official Go SDK,
// streaming so long answers don't hit request timeouts.
type Anthropic struct {
	HTTP    *http.Client
	BaseURL string // default AnthropicBaseURL; tests override
}

func (a *Anthropic) Run(ctx context.Context, model, apiKey, system string, msgs []Message, maxTokens int, _ *float64) (Result, error) {
	base := a.BaseURL
	if base == "" {
		// Set explicitly so no environment variable can send the key elsewhere.
		base = AnthropicBaseURL
	}
	// Nothing from the server's environment (keys, tokens, base URL) may
	// apply to a replay: only the caller's key, to Anthropic's host.
	opts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithAPIKey(apiKey), option.WithBaseURL(base), option.WithMaxRetries(1)}
	if a.HTTP != nil {
		opts = append(opts, option.WithHTTPClient(a.HTTP))
	}
	client := anthropic.NewClient(opts...)

	params := anthropic.MessageNewParams{Model: anthropic.Model(model), MaxTokens: int64(maxTokens)}
	if system != "" {
		params.System = []anthropic.TextBlockParam{{Text: system}}
	}
	for _, m := range msgs {
		if m.Role == "assistant" {
			params.Messages = append(params.Messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
		} else {
			params.Messages = append(params.Messages, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		}
	}
	// Sampling parameters aren't sent: this SDK's Messages API takes none,
	// and the replay reproduces the call's prompt, not its settings.

	stream := client.Messages.NewStreaming(ctx, params)
	var msg anthropic.Message
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return Result{}, errors.New("unreadable answer from Anthropic")
		}
	}
	if err := stream.Err(); err != nil {
		var ae *anthropic.Error
		if errors.As(err, &ae) {
			text := ae.Message
			if text == "" {
				text = http.StatusText(ae.StatusCode)
			}
			return Result{}, &ProviderError{Status: ae.StatusCode, Message: text}
		}
		return Result{}, errors.New("could not reach Anthropic")
	}
	var out strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(t.Text)
		}
	}
	u := msg.Usage
	// Bindle counts all input tokens (cached included); Anthropic reports
	// cache reads and writes separately from input_tokens.
	return Result{
		Output: out.String(), Model: string(msg.Model), StopReason: string(msg.StopReason),
		InputTokens:  u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		OutputTokens: u.OutputTokens, CachedTokens: u.CacheReadInputTokens,
	}, nil
}
