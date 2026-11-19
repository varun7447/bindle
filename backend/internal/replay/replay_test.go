package replay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func req(msgs ...Message) Request {
	return Request{Provider: "openai", Model: "gpt-x", APIKey: "fake-openai-key-for-tests", Messages: msgs}
}

func TestValidate(t *testing.T) {
	if err := Validate(req(Message{"user", "hi"})); err != nil {
		t.Fatal(err)
	}
	bad := []Request{
		{Provider: "other", Model: "m", APIKey: "fake-key-1", Messages: []Message{{"user", "x"}}},
		{Provider: "openai", Model: "a b", APIKey: "fake-key-1", Messages: []Message{{"user", "x"}}},
		{Provider: "openai", Model: "m", APIKey: "", Messages: []Message{{"user", "x"}}},
		{Provider: "openai", Model: "m", APIKey: "fake-key-1"},
		{Provider: "openai", Model: "m", APIKey: "fake-key-1", Messages: []Message{{"robot", "x"}}},
		{Provider: "openai", Model: "m", APIKey: "fake-key-1", Messages: []Message{{"assistant", "x"}}},
		{Provider: "openai", Model: "m", APIKey: "fake-key-1", Messages: []Message{{"user", strings.Repeat("x", MaxTotalChars+1)}}},
		{Provider: "openai", Model: "m", APIKey: "fake-key-1", Messages: []Message{{"user", "x"}}, MaxTokens: MaxMaxTokens + 1},
	}
	for i, r := range bad {
		if Validate(r) == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestPrepare(t *testing.T) {
	sys, msgs, err := Prepare([]Message{
		{"system", "You are helpful."}, {"user", "Where is my refund?"}, {"user", "Order 1234."},
		{"assistant", "Let me check."}, {"tool", `{"status":"issued"}`}, {"system", "Be brief."}, {"assistant", ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sys != "You are helpful.\n\nBe brief." {
		t.Errorf("system %q", sys)
	}
	want := []Message{{"user", "Where is my refund?\n\nOrder 1234."}, {"assistant", "Let me check."}, {"user", "Tool result:\n{\"status\":\"issued\"}"}}
	if len(msgs) != len(want) {
		t.Fatalf("got %+v", msgs)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Errorf("msg %d: %+v", i, msgs[i])
		}
	}
	if _, _, err := Prepare([]Message{{"assistant", "hi"}, {"user", "x"}}); err == nil {
		t.Error("must start with the user")
	}
}

func TestOpenAIRun(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		if strings.Contains(string(b), "fail") {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: fake-openai-key-for-tests"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"gpt-x-2026","choices":[{"finish_reason":"stop","message":{"content":"Refund issued."}}],
			"usage":{"prompt_tokens":40,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":10}}}`))
	}))
	defer srv.Close()
	run := &Runner{Providers: map[string]Provider{"openai": &OpenAI{BaseURL: srv.URL}}}
	res, err := run.Run(context.Background(), req(Message{"system", "Be brief."}, Message{"user", "Where is my refund?"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "Refund issued." || res.Model != "gpt-x-2026" || res.InputTokens != 40 || res.CachedTokens != 10 || res.StopReason != "stop" || res.LatencyMs <= 0 {
		t.Errorf("result %+v", res)
	}
	if auth != "Bearer fake-openai-key-for-tests" || got["max_completion_tokens"] != float64(DefaultMaxTokens) || len(got["messages"].([]any)) != 2 {
		t.Errorf("request %v auth %q", got, auth)
	}
	_, err = run.Run(context.Background(), req(Message{"user", "fail"}))
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Status != 401 || strings.Contains(err.Error(), "fake-openai-key-for-tests") || !strings.Contains(pe.Message, "[REDACTED:KEY]") {
		t.Errorf("provider error must not echo the key: %v", err)
	}
	if _, err := (&Runner{Providers: map[string]Provider{}}).Run(context.Background(), req(Message{"user", "x"})); !errors.Is(err, ErrUnsupported) {
		t.Errorf("unsupported: %v", err)
	}
}

func TestAnthropicRun(t *testing.T) {
	var got map[string]any
	var key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		if strings.Contains(string(b), "fail") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":30,"cache_read_input_tokens":10,"cache_creation_input_tokens":0,"output_tokens":1}}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Refund "}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"issued."}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e + "\n\n"))
		}
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "server-env-key-must-not-be-used")
	run := &Runner{Providers: map[string]Provider{"anthropic": &Anthropic{BaseURL: srv.URL}}}
	req := Request{Provider: "anthropic", Model: "claude-opus-5-5", APIKey: "fake-anthropic-key-for-tests", MaxTokens: 512,
		Messages: []Message{{"system", "Be brief."}, {"user", "Where is my refund?"}, {"assistant", "Which order?"}, {"user", "1234"}}}
	res, err := run.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "Refund issued." || res.Model != "claude-opus-5-5" || res.StopReason != "end_turn" || res.InputTokens != 40 || res.CachedTokens != 10 || res.OutputTokens != 5 {
		t.Errorf("result %+v", res)
	}
	if key != "fake-anthropic-key-for-tests" || got["max_tokens"] != float64(512) || got["stream"] != true || len(got["messages"].([]any)) != 3 {
		t.Errorf("request %v key %q", got, key)
	}
	if sys := got["system"].([]any); sys[0].(map[string]any)["text"] != "Be brief." {
		t.Errorf("system %v", got["system"])
	}
	req.Messages = []Message{{"user", "fail"}}
	_, err = run.Run(context.Background(), req)
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Status != 401 || !strings.Contains(pe.Message, "invalid x-api-key") || strings.Contains(err.Error(), "fake-anthropic-key-for-tests") {
		t.Errorf("provider error: %v", err)
	}
}
