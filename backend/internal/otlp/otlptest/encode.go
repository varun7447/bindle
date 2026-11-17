// Package otlptest builds OTLP protobuf payloads for tests without depending
// on generated protobuf code.
package otlptest

import (
	"encoding/binary"
	"encoding/hex"
	"math"
)

// KV is a test attribute.
type KV struct {
	Key   string
	Value any // string, bool, int64, int, float64
}

// Span is a test span.
type Span struct {
	TraceID, SpanID, ParentID string // hex
	Name                      string
	Start, End                uint64
	Attrs                     []KV
	StatusCode                int32
	StatusMsg                 string
	Events                    []Event
}

// Event is a test event.
type Event struct {
	Name  string
	Time  uint64
	Attrs []KV
}

// Request encodes one ResourceSpans with one ScopeSpans.
func Request(resource []KV, scopeName string, spans ...Span) []byte {
	var scope []byte
	scope = appendBytes(scope, 1, appendBytes(nil, 1, []byte(scopeName)))
	for _, s := range spans {
		scope = appendBytes(scope, 2, encodeSpan(s))
	}
	var res []byte
	for _, kv := range resource {
		res = appendBytes(res, 1, encodeKV(kv))
	}
	var rs []byte
	rs = appendBytes(rs, 1, res)
	rs = appendBytes(rs, 2, scope)
	return appendBytes(nil, 1, rs)
}

func encodeSpan(s Span) []byte {
	var b []byte
	b = appendBytes(b, 1, mustHex(s.TraceID))
	b = appendBytes(b, 2, mustHex(s.SpanID))
	if s.ParentID != "" {
		b = appendBytes(b, 4, mustHex(s.ParentID))
	}
	b = appendBytes(b, 5, []byte(s.Name))
	b = appendTag(b, 6, 0)
	b = binary.AppendUvarint(b, 1) // internal
	b = appendFixed64(b, 7, s.Start)
	b = appendFixed64(b, 8, s.End)
	for _, kv := range s.Attrs {
		b = appendBytes(b, 9, encodeKV(kv))
	}
	for _, e := range s.Events {
		var eb []byte
		eb = appendFixed64(eb, 1, e.Time)
		eb = appendBytes(eb, 2, []byte(e.Name))
		for _, kv := range e.Attrs {
			eb = appendBytes(eb, 3, encodeKV(kv))
		}
		b = appendBytes(b, 11, eb)
	}
	if s.StatusCode != 0 || s.StatusMsg != "" {
		var sb []byte
		if s.StatusMsg != "" {
			sb = appendBytes(sb, 2, []byte(s.StatusMsg))
		}
		sb = appendTag(sb, 3, 0)
		sb = binary.AppendUvarint(sb, uint64(s.StatusCode))
		b = appendBytes(b, 15, sb)
	}
	return b
}

func encodeKV(kv KV) []byte {
	var v []byte
	switch t := kv.Value.(type) {
	case string:
		v = appendBytes(v, 1, []byte(t))
	case bool:
		v = appendTag(v, 2, 0)
		if t {
			v = binary.AppendUvarint(v, 1)
		} else {
			v = binary.AppendUvarint(v, 0)
		}
	case int:
		v = appendTag(v, 3, 0)
		v = binary.AppendUvarint(v, uint64(int64(t)))
	case int64:
		v = appendTag(v, 3, 0)
		v = binary.AppendUvarint(v, uint64(t))
	case float64:
		v = appendFixed64(v, 4, math.Float64bits(t))
	}
	var b []byte
	b = appendBytes(b, 1, []byte(kv.Key))
	b = appendBytes(b, 2, v)
	return b
}

func appendTag(b []byte, field, wt int) []byte {
	return binary.AppendUvarint(b, uint64(field<<3|wt))
}

func appendBytes(b []byte, field int, v []byte) []byte {
	b = appendTag(b, field, 2)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

func appendFixed64(b []byte, field int, v uint64) []byte {
	b = appendTag(b, field, 1)
	return binary.LittleEndian.AppendUint64(b, v)
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
