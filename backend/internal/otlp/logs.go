package otlp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/bindle-ai/bindle/backend/internal/model"
)

// EvaluationEvent is the OpenTelemetry GenAI event for an evaluation score
// (gen_ai.evaluation.result). It is the only log record Bindle keeps.
const EvaluationEvent = "gen_ai.evaluation.result"

// Logs are decoded only for evaluation events: each becomes a span-shaped
// record parented to the span it evaluates, so it is stored as an eval step
// of that run. Field numbers follow opentelemetry/proto/logs/v1/logs.proto.

// DecodeLogsProto decodes an OTLP ExportLogsServiceRequest (protobuf).
func DecodeLogsProto(b []byte) ([]model.Span, error) {
	var out []model.Span
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return nil, err
		}
		if f != 1 {
			if err := r.skip(wt); err != nil {
				return nil, err
			}
			continue
		}
		if err := expect(wt, wtBytes); err != nil {
			return nil, err
		}
		rl, err := r.bytes()
		if err != nil {
			return nil, err
		}
		if err := decodeResourceLogs(rl, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func decodeResourceLogs(b []byte, out *[]model.Span) error {
	var res map[string]any
	var scopes [][]byte
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return err
		}
		switch f {
		case 1:
			if err := expect(wt, wtBytes); err != nil {
				return err
			}
			rb, err := r.bytes()
			if err != nil {
				return err
			}
			if res, err = decodeAttrsMessage(rb, 1); err != nil {
				return err
			}
		case 2:
			if err := expect(wt, wtBytes); err != nil {
				return err
			}
			sb, err := r.bytes()
			if err != nil {
				return err
			}
			scopes = append(scopes, sb)
		default:
			if err := r.skip(wt); err != nil {
				return err
			}
		}
	}
	for _, sb := range scopes {
		if err := decodeScopeLogs(sb, res, out); err != nil {
			return err
		}
	}
	return nil
}

func decodeScopeLogs(b []byte, res map[string]any, out *[]model.Span) error {
	var name, version string
	var records [][]byte
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return err
		}
		switch f {
		case 1:
			if err := expect(wt, wtBytes); err != nil {
				return err
			}
			sb, err := r.bytes()
			if err != nil {
				return err
			}
			if name, version, err = decodeScope(sb); err != nil {
				return err
			}
		case 2:
			if err := expect(wt, wtBytes); err != nil {
				return err
			}
			lb, err := r.bytes()
			if err != nil {
				return err
			}
			records = append(records, lb)
		default:
			if err := r.skip(wt); err != nil {
				return err
			}
		}
	}
	for i, lb := range records {
		rec, err := decodeLogRecord(lb)
		if err != nil {
			return err
		}
		if s, ok := evaluationSpan(rec, res, name, version, i); ok {
			*out = append(*out, s)
		}
	}
	return nil
}

type logRecord struct {
	time, observed uint64
	traceID        string
	spanID         string
	eventName      string
	attrs          map[string]any
}

func decodeLogRecord(b []byte) (logRecord, error) {
	rec := logRecord{attrs: map[string]any{}}
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return rec, err
		}
		switch f {
		case 1, 11:
			if err := expect(wt, wtFixed64); err != nil {
				return rec, err
			}
			v, err := r.fixed64()
			if err != nil {
				return rec, err
			}
			if f == 1 {
				rec.time = v
			} else {
				rec.observed = v
			}
		case 6:
			if err := expect(wt, wtBytes); err != nil {
				return rec, err
			}
			kb, err := r.bytes()
			if err != nil {
				return rec, err
			}
			k, v, err := decodeKeyValue(kb, 0)
			if err != nil {
				return rec, err
			}
			rec.attrs[k] = v
		case 9, 10, 12:
			if err := expect(wt, wtBytes); err != nil {
				return rec, err
			}
			v, err := r.bytes()
			if err != nil {
				return rec, err
			}
			switch f {
			case 9:
				rec.traceID = hex.EncodeToString(v)
			case 10:
				rec.spanID = hex.EncodeToString(v)
			case 12:
				rec.eventName = string(v)
			}
		default:
			if err := r.skip(wt); err != nil {
				return rec, err
			}
		}
	}
	return rec, nil
}

// evaluationSpan turns an evaluation log record into a span-shaped record
// parented to the evaluated span. Records that aren't evaluation events, or
// that aren't tied to a trace, are dropped.
func evaluationSpan(rec logRecord, res map[string]any, scope, version string, i int) (model.Span, bool) {
	name := rec.eventName
	if name == "" {
		if v, ok := rec.attrs["event.name"].(string); ok {
			name = v
		}
	}
	if name != EvaluationEvent || len(rec.traceID) != 32 || rec.traceID == "00000000000000000000000000000000" {
		return model.Span{}, false
	}
	t := rec.time
	if t == 0 {
		t = rec.observed
	}
	attrs := map[string]any{}
	for k, v := range rec.attrs {
		attrs[k] = v
	}
	attrs["bindle.step.type"] = "eval"
	// Evaluation records often come from a separate evaluator service: its
	// resource (service name, version) must not relabel the evaluated run.
	attrs["bindle.score.external"] = true
	evName, _ := attrs["gen_ai.evaluation.name"].(string)
	parent := rec.spanID
	if parent == "0000000000000000" {
		parent = ""
	}
	return model.Span{
		TraceID: rec.traceID, SpanID: DerivedSpanID(rec.traceID, parent, evName, strconv.FormatUint(t, 10), strconv.Itoa(i)),
		ParentSpanID: parent, Name: "evaluation " + evName, StartUnixNano: t, EndUnixNano: t,
		Attributes: attrs, ResourceAttrs: res, ScopeName: scope, ScopeVersion: version,
	}, true
}

// DerivedSpanID is a stable span ID for a record that has none of its own,
// so a re-sent batch maps to the same step.
func DerivedSpanID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0x1f})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// ---- JSON encoding ---------------------------------------------------------------

type jLogsRequest struct {
	ResourceLogs []struct {
		Resource *struct {
			Attributes []jKV `json:"attributes"`
		} `json:"resource"`
		ScopeLogs []struct {
			Scope *struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"scope"`
			LogRecords []struct {
				TimeUnixNano         u64    `json:"timeUnixNano"`
				ObservedTimeUnixNano u64    `json:"observedTimeUnixNano"`
				TraceID              string `json:"traceId"`
				SpanID               string `json:"spanId"`
				EventName            string `json:"eventName"`
				Attributes           []jKV  `json:"attributes"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

// DecodeLogsJSON decodes an OTLP ExportLogsServiceRequest in JSON encoding.
func DecodeLogsJSON(b []byte) ([]model.Span, error) {
	var req jLogsRequest
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&req); err != nil {
		return nil, err
	}
	var out []model.Span
	for _, rl := range req.ResourceLogs {
		var res map[string]any
		if rl.Resource != nil {
			res = kvsToMap(rl.Resource.Attributes, 0)
		}
		for _, sl := range rl.ScopeLogs {
			var sn, sv string
			if sl.Scope != nil {
				sn, sv = sl.Scope.Name, sl.Scope.Version
			}
			for i, lr := range sl.LogRecords {
				rec := logRecord{time: uint64(lr.TimeUnixNano), observed: uint64(lr.ObservedTimeUnixNano), traceID: normHex(lr.TraceID),
					spanID: normHex(lr.SpanID), eventName: lr.EventName, attrs: kvsToMap(lr.Attributes, 0)}
				if s, ok := evaluationSpan(rec, res, sn, sv, i); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out, nil
}
