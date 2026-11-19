package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// OpenAI calls the Chat Completions API.
type OpenAI struct {
	HTTP    *http.Client
	BaseURL string // default https://api.openai.com; tests override
}

func (o *OpenAI) Run(ctx context.Context, model, apiKey, system string, msgs []Message, maxTokens int, temperature *float64) (Result, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	body := map[string]any{"model": model, "max_completion_tokens": maxTokens}
	var list []msg
	if system != "" {
		list = append(list, msg{"system", system})
	}
	for _, m := range msgs {
		list = append(list, msg{m.Role, m.Content})
	}
	body["messages"] = list
	if temperature != nil {
		body["temperature"] = *temperature
	}
	b, _ := json.Marshal(body)
	base := o.BaseURL
	if base == "" {
		base = "https://api.openai.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: Timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("could not reach OpenAI: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Result{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error.Message == "" {
			e.Error.Message = http.StatusText(resp.StatusCode)
		}
		return Result{}, &ProviderError{Status: resp.StatusCode, Message: e.Error.Message}
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("unreadable answer from OpenAI")
	}
	r := Result{Model: out.Model, InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens, CachedTokens: out.Usage.PromptTokensDetails.CachedTokens}
	if len(out.Choices) > 0 {
		r.Output, r.StopReason = out.Choices[0].Message.Content, out.Choices[0].FinishReason
	}
	return r, nil
}
