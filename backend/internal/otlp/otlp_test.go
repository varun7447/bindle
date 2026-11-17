package otlp_test

import (
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/otlp"
	"github.com/bindle-ai/bindle/backend/internal/otlp/otlptest"
)

const (
	traceID = "0af7651916cd43dd8448eb211c80319c"
	rootID  = "b7ad6b7169203331"
	childID = "00f067aa0ba902b7"
)

func TestDecodeProto(t *testing.T) {
	payload := otlptest.Request(
		[]otlptest.KV{{Key: "service.name", Value: "support-triage"}},
		"openinference.instrumentation.langchain",
		otlptest.Span{TraceID: traceID, SpanID: rootID, Name: "agent run", Start: 1, End: 5},
		otlptest.Span{
			TraceID: traceID, SpanID: childID, ParentID: rootID, Name: "ChatModel",
			Start: 2, End: 4, StatusCode: 2, StatusMsg: "boom",
			Attrs: []otlptest.KV{
				{Key: "openinference.span.kind", Value: "LLM"},
				{Key: "llm.token_count.prompt", Value: int64(1204)},
				{Key: "temperature", Value: 0.3},
				{Key: "stream", Value: true},
			},
			Events: []otlptest.Event{{Name: "exception", Time: 3, Attrs: []otlptest.KV{{Key: "exception.message", Value: "timeout"}}}},
		},
	)
	spans, err := otlp.DecodeProto(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	c := spans[1]
	if c.TraceID != traceID || c.SpanID != childID || c.ParentSpanID != rootID {
		t.Fatalf("ids wrong: %+v", c)
	}
	if c.Name != "ChatModel" || c.StartUnixNano != 2 || c.EndUnixNano != 4 {
		t.Fatalf("fields wrong: %+v", c)
	}
	if c.StatusCode != 2 || c.StatusMessage != "boom" {
		t.Fatalf("status wrong: %d %q", c.StatusCode, c.StatusMessage)
	}
	if c.Attributes["llm.token_count.prompt"] != int64(1204) || c.Attributes["temperature"] != 0.3 || c.Attributes["stream"] != true {
		t.Fatalf("attrs wrong: %#v", c.Attributes)
	}
	if c.ResourceAttrs["service.name"] != "support-triage" || c.ScopeName != "openinference.instrumentation.langchain" {
		t.Fatalf("resource/scope wrong")
	}
	if len(c.Events) != 1 || c.Events[0].Attributes["exception.message"] != "timeout" {
		t.Fatalf("events wrong: %#v", c.Events)
	}
}

func TestDecodeProtoTruncated(t *testing.T) {
	payload := otlptest.Request(nil, "x", otlptest.Span{TraceID: traceID, SpanID: rootID, Name: "a"})
	if _, err := otlp.DecodeProto(payload[:len(payload)-3]); err == nil {
		t.Fatal("expected error for truncated payload")
	}
}

func TestDecodeJSON(t *testing.T) {
	body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"docs-qa"}}]},
	"scopeSpans":[{"scope":{"name":"strands.telemetry"},"spans":[{"traceId":"0AF7651916CD43DD8448EB211C80319C","spanId":"b7ad6b7169203331",
	"name":"chat","kind":3,"startTimeUnixNano":"1700000000000000000","endTimeUnixNano":1700000000500000000,
	"attributes":[{"key":"gen_ai.usage.input_tokens","value":{"intValue":"42"}},{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}}],
	"status":{"code":1}}]}]}]}`
	spans, err := otlp.DecodeJSON([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	s := spans[0]
	if s.TraceID != traceID {
		t.Fatalf("trace id not normalized: %s", s.TraceID)
	}
	if s.EndUnixNano-s.StartUnixNano != 500000000 {
		t.Fatalf("times wrong: %d %d", s.StartUnixNano, s.EndUnixNano)
	}
	if s.Attributes["gen_ai.usage.input_tokens"] != int64(42) || s.StatusCode != 1 {
		t.Fatalf("attrs/status wrong: %#v %d", s.Attributes, s.StatusCode)
	}
}
