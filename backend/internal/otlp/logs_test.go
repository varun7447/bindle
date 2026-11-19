package otlp

import (
	"strings"
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/otlp/otlptest"
)

const (
	tid = "0af7651916cd43dd8448eb211c80319c"
	sid = "b7ad6b7169203331"
)

func TestDecodeLogsProto(t *testing.T) {
	b := otlptest.LogsRequest([]otlptest.KV{{Key: "service.name", Value: "judge"}}, "azure.ai.evaluation",
		otlptest.LogRecord{TraceID: tid, SpanID: sid, Time: 100, EventName: EvaluationEvent, Attrs: []otlptest.KV{
			{Key: "gen_ai.evaluation.name", Value: "Relevance"}, {Key: "gen_ai.evaluation.score.value", Value: 4.0}}},
		otlptest.LogRecord{TraceID: tid, SpanID: sid, Time: 101, Attrs: []otlptest.KV{{Key: "event.name", Value: EvaluationEvent}, {Key: "gen_ai.evaluation.name", Value: "Fluency"}}},
		otlptest.LogRecord{TraceID: tid, Time: 102, EventName: "gen_ai.client.inference.operation.details"}, // not an evaluation
		otlptest.LogRecord{Time: 103, EventName: EvaluationEvent},                                           // no trace
	)
	spans, err := DecodeLogsProto(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans: %+v", len(spans), spans)
	}
	s := spans[0]
	if s.TraceID != tid || s.ParentSpanID != sid || s.Name != "evaluation Relevance" || s.Attributes["gen_ai.evaluation.score.value"] != 4.0 ||
		s.Attributes["bindle.step.type"] != "eval" || s.ResourceAttrs["service.name"] != "judge" || len(s.SpanID) != 16 || s.SpanID == spans[1].SpanID {
		t.Errorf("span %+v", s)
	}
	again, _ := DecodeLogsProto(b)
	if again[0].SpanID != s.SpanID {
		t.Error("derived span IDs must be stable so re-sent batches don't duplicate")
	}
}

func TestDecodeLogsJSON(t *testing.T) {
	body := `{"resourceLogs":[{"resource":{"attributes":[]},"scopeLogs":[{"logRecords":[
		{"timeUnixNano":"200","traceId":"` + tid + `","spanId":"` + sid + `","eventName":"gen_ai.evaluation.result",
		 "attributes":[{"key":"gen_ai.evaluation.name","value":{"stringValue":"Coherence"}},{"key":"gen_ai.evaluation.score.label","value":{"stringValue":"pass"}}]},
		{"timeUnixNano":"201","traceId":"` + tid + `","eventName":"other"}]}]}]}`
	spans, err := DecodeLogsJSON([]byte(body))
	if err != nil || len(spans) != 1 || spans[0].Name != "evaluation Coherence" || spans[0].StartUnixNano != 200 {
		t.Fatalf("%+v %v", spans, err)
	}
	if _, err := DecodeLogsJSON([]byte("nope")); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("bad JSON: %v", err)
	}
}
