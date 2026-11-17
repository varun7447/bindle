package otlp

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/bindle-ai/bindle/backend/internal/model"
)

// OTLP/JSON encoding: field names are lowerCamelCase, trace and span IDs are
// hex strings, and 64-bit integers may arrive as strings or numbers.

type u64 uint64

func (u *u64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*u = 0
		return nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	*u = u64(v)
	return err
}

type i64 int64

func (i *i64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*i = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	*i = i64(v)
	return err
}

type jAny struct {
	StringValue *string  `json:"stringValue"`
	BoolValue   *bool    `json:"boolValue"`
	IntValue    *i64     `json:"intValue"`
	DoubleValue *float64 `json:"doubleValue"`
	ArrayValue  *struct {
		Values []jAny `json:"values"`
	} `json:"arrayValue"`
	KvlistValue *struct {
		Values []jKV `json:"values"`
	} `json:"kvlistValue"`
	BytesValue *string `json:"bytesValue"`
}

type jKV struct {
	Key   string `json:"key"`
	Value jAny   `json:"value"`
}

type jEvent struct {
	TimeUnixNano u64    `json:"timeUnixNano"`
	Name         string `json:"name"`
	Attributes   []jKV  `json:"attributes"`
}

type jLink struct {
	TraceID    string `json:"traceId"`
	SpanID     string `json:"spanId"`
	Attributes []jKV  `json:"attributes"`
}

type jSpan struct {
	TraceID           string   `json:"traceId"`
	SpanID            string   `json:"spanId"`
	ParentSpanID      string   `json:"parentSpanId"`
	Name              string   `json:"name"`
	Kind              int32    `json:"kind"`
	StartTimeUnixNano u64      `json:"startTimeUnixNano"`
	EndTimeUnixNano   u64      `json:"endTimeUnixNano"`
	Attributes        []jKV    `json:"attributes"`
	Events            []jEvent `json:"events"`
	Links             []jLink  `json:"links"`
	Status            *struct {
		Message string `json:"message"`
		Code    int32  `json:"code"`
	} `json:"status"`
}

type jRequest struct {
	ResourceSpans []struct {
		Resource *struct {
			Attributes []jKV `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Scope *struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"scope"`
			Spans []jSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

// DecodeJSON decodes an OTLP ExportTraceServiceRequest in JSON encoding.
func DecodeJSON(b []byte) ([]model.Span, error) {
	var req jRequest
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&req); err != nil {
		return nil, err
	}
	var out []model.Span
	for _, rs := range req.ResourceSpans {
		var res map[string]any
		if rs.Resource != nil {
			res = kvsToMap(rs.Resource.Attributes, 0)
		}
		for _, ss := range rs.ScopeSpans {
			var sn, sv string
			if ss.Scope != nil {
				sn, sv = ss.Scope.Name, ss.Scope.Version
			}
			for _, js := range ss.Spans {
				s := model.Span{
					TraceID:       normHex(js.TraceID),
					SpanID:        normHex(js.SpanID),
					ParentSpanID:  normHex(js.ParentSpanID),
					Name:          js.Name,
					Kind:          js.Kind,
					StartUnixNano: uint64(js.StartTimeUnixNano),
					EndUnixNano:   uint64(js.EndTimeUnixNano),
					Attributes:    kvsToMap(js.Attributes, 0),
					ResourceAttrs: res,
					ScopeName:     sn,
					ScopeVersion:  sv,
				}
				if js.Status != nil {
					s.StatusCode, s.StatusMessage = js.Status.Code, js.Status.Message
				}
				for _, e := range js.Events {
					s.Events = append(s.Events, model.Event{
						TimeUnixNano: uint64(e.TimeUnixNano),
						Name:         e.Name,
						Attributes:   kvsToMap(e.Attributes, 0),
					})
				}
				for _, l := range js.Links {
					s.Links = append(s.Links, model.Link{
						TraceID:    normHex(l.TraceID),
						SpanID:     normHex(l.SpanID),
						Attributes: kvsToMap(l.Attributes, 0),
					})
				}
				out = append(out, s)
			}
		}
	}
	return out, nil
}

func normHex(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if _, err := hex.DecodeString(s); err == nil {
		return s
	}
	// Some exporters send base64 IDs; convert to hex.
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return hex.EncodeToString(b)
	}
	return s
}

func kvsToMap(kvs []jKV, depth int) map[string]any {
	m := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = anyToValue(kv.Value, depth+1)
	}
	return m
}

func anyToValue(a jAny, depth int) any {
	if depth > maxValueDepth {
		return nil
	}
	switch {
	case a.StringValue != nil:
		return *a.StringValue
	case a.BoolValue != nil:
		return *a.BoolValue
	case a.IntValue != nil:
		return int64(*a.IntValue)
	case a.DoubleValue != nil:
		return *a.DoubleValue
	case a.ArrayValue != nil:
		arr := make([]any, 0, len(a.ArrayValue.Values))
		for _, v := range a.ArrayValue.Values {
			arr = append(arr, anyToValue(v, depth+1))
		}
		return arr
	case a.KvlistValue != nil:
		return kvsToMap(a.KvlistValue.Values, depth+1)
	case a.BytesValue != nil:
		if b, err := base64.StdEncoding.DecodeString(*a.BytesValue); err == nil {
			return hex.EncodeToString(b)
		}
		return *a.BytesValue
	}
	return nil
}
