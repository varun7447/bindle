package otlp

import (
	"encoding/hex"
	"math"

	"github.com/bindle-ai/bindle/backend/internal/model"
)

// Field numbers follow opentelemetry/proto/trace/v1/trace.proto and
// opentelemetry/proto/common/v1/common.proto.

const maxValueDepth = 8

// DecodeProto decodes an OTLP ExportTraceServiceRequest (protobuf encoding).
func DecodeProto(b []byte) ([]model.Span, error) {
	var out []model.Span
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return nil, err
		}
		if f == 1 {
			if err := expect(wt, wtBytes); err != nil {
				return nil, err
			}
			rs, err := r.bytes()
			if err != nil {
				return nil, err
			}
			if err := decodeResourceSpans(rs, &out); err != nil {
				return nil, err
			}
			continue
		}
		if err := r.skip(wt); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func decodeResourceSpans(b []byte, out *[]model.Span) error {
	var res map[string]any
	var scopes [][]byte
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return err
		}
		switch f {
		case 1: // resource
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
		case 2: // scope_spans
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
		if err := decodeScopeSpans(sb, res, out); err != nil {
			return err
		}
	}
	return nil
}

func decodeScopeSpans(b []byte, res map[string]any, out *[]model.Span) error {
	var scopeName, scopeVersion string
	var spans [][]byte
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return err
		}
		switch f {
		case 1: // scope
			if err := expect(wt, wtBytes); err != nil {
				return err
			}
			sb, err := r.bytes()
			if err != nil {
				return err
			}
			if scopeName, scopeVersion, err = decodeScope(sb); err != nil {
				return err
			}
		case 2: // spans
			if err := expect(wt, wtBytes); err != nil {
				return err
			}
			sp, err := r.bytes()
			if err != nil {
				return err
			}
			spans = append(spans, sp)
		default:
			if err := r.skip(wt); err != nil {
				return err
			}
		}
	}
	for _, sp := range spans {
		s, err := decodeSpan(sp)
		if err != nil {
			return err
		}
		s.ResourceAttrs = res
		s.ScopeName = scopeName
		s.ScopeVersion = scopeVersion
		*out = append(*out, s)
	}
	return nil
}

func decodeScope(b []byte) (name, version string, err error) {
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return "", "", err
		}
		switch f {
		case 1, 2:
			if err := expect(wt, wtBytes); err != nil {
				return "", "", err
			}
			v, err := r.bytes()
			if err != nil {
				return "", "", err
			}
			if f == 1 {
				name = string(v)
			} else {
				version = string(v)
			}
		default:
			if err := r.skip(wt); err != nil {
				return "", "", err
			}
		}
	}
	return name, version, nil
}

func decodeSpan(b []byte) (model.Span, error) {
	s := model.Span{Attributes: map[string]any{}}
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return s, err
		}
		switch f {
		case 1, 2, 4, 5: // trace_id, span_id, parent_span_id, name
			if err := expect(wt, wtBytes); err != nil {
				return s, err
			}
			v, err := r.bytes()
			if err != nil {
				return s, err
			}
			switch f {
			case 1:
				s.TraceID = hex.EncodeToString(v)
			case 2:
				s.SpanID = hex.EncodeToString(v)
			case 4:
				s.ParentSpanID = hex.EncodeToString(v)
			case 5:
				s.Name = string(v)
			}
		case 6: // kind
			if err := expect(wt, wtVarint); err != nil {
				return s, err
			}
			v, err := r.varint()
			if err != nil {
				return s, err
			}
			s.Kind = int32(v)
		case 7, 8: // start / end time
			if err := expect(wt, wtFixed64); err != nil {
				return s, err
			}
			v, err := r.fixed64()
			if err != nil {
				return s, err
			}
			if f == 7 {
				s.StartUnixNano = v
			} else {
				s.EndUnixNano = v
			}
		case 9: // attributes
			if err := expect(wt, wtBytes); err != nil {
				return s, err
			}
			kb, err := r.bytes()
			if err != nil {
				return s, err
			}
			k, v, err := decodeKeyValue(kb, 0)
			if err != nil {
				return s, err
			}
			s.Attributes[k] = v
		case 11: // events
			if err := expect(wt, wtBytes); err != nil {
				return s, err
			}
			eb, err := r.bytes()
			if err != nil {
				return s, err
			}
			e, err := decodeEvent(eb)
			if err != nil {
				return s, err
			}
			s.Events = append(s.Events, e)
		case 13: // links
			if err := expect(wt, wtBytes); err != nil {
				return s, err
			}
			lb, err := r.bytes()
			if err != nil {
				return s, err
			}
			l, err := decodeLink(lb)
			if err != nil {
				return s, err
			}
			s.Links = append(s.Links, l)
		case 15: // status
			if err := expect(wt, wtBytes); err != nil {
				return s, err
			}
			sb, err := r.bytes()
			if err != nil {
				return s, err
			}
			if s.StatusCode, s.StatusMessage, err = decodeStatus(sb); err != nil {
				return s, err
			}
		default:
			if err := r.skip(wt); err != nil {
				return s, err
			}
		}
	}
	return s, nil
}

func decodeStatus(b []byte) (code int32, msg string, err error) {
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return 0, "", err
		}
		switch f {
		case 2:
			if err := expect(wt, wtBytes); err != nil {
				return 0, "", err
			}
			v, err := r.bytes()
			if err != nil {
				return 0, "", err
			}
			msg = string(v)
		case 3:
			if err := expect(wt, wtVarint); err != nil {
				return 0, "", err
			}
			v, err := r.varint()
			if err != nil {
				return 0, "", err
			}
			code = int32(v)
		default:
			if err := r.skip(wt); err != nil {
				return 0, "", err
			}
		}
	}
	return code, msg, nil
}

func decodeEvent(b []byte) (model.Event, error) {
	e := model.Event{Attributes: map[string]any{}}
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return e, err
		}
		switch f {
		case 1:
			if err := expect(wt, wtFixed64); err != nil {
				return e, err
			}
			if e.TimeUnixNano, err = r.fixed64(); err != nil {
				return e, err
			}
		case 2:
			if err := expect(wt, wtBytes); err != nil {
				return e, err
			}
			v, err := r.bytes()
			if err != nil {
				return e, err
			}
			e.Name = string(v)
		case 3:
			if err := expect(wt, wtBytes); err != nil {
				return e, err
			}
			kb, err := r.bytes()
			if err != nil {
				return e, err
			}
			k, v, err := decodeKeyValue(kb, 0)
			if err != nil {
				return e, err
			}
			e.Attributes[k] = v
		default:
			if err := r.skip(wt); err != nil {
				return e, err
			}
		}
	}
	return e, nil
}

func decodeLink(b []byte) (model.Link, error) {
	l := model.Link{Attributes: map[string]any{}}
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return l, err
		}
		switch f {
		case 1, 2:
			if err := expect(wt, wtBytes); err != nil {
				return l, err
			}
			v, err := r.bytes()
			if err != nil {
				return l, err
			}
			if f == 1 {
				l.TraceID = hex.EncodeToString(v)
			} else {
				l.SpanID = hex.EncodeToString(v)
			}
		case 4:
			if err := expect(wt, wtBytes); err != nil {
				return l, err
			}
			kb, err := r.bytes()
			if err != nil {
				return l, err
			}
			k, v, err := decodeKeyValue(kb, 0)
			if err != nil {
				return l, err
			}
			l.Attributes[k] = v
		default:
			if err := r.skip(wt); err != nil {
				return l, err
			}
		}
	}
	return l, nil
}

// decodeAttrsMessage decodes any message whose repeated KeyValue lives at field.
func decodeAttrsMessage(b []byte, field int) (map[string]any, error) {
	attrs := map[string]any{}
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return nil, err
		}
		if f == field && wt == wtBytes {
			kb, err := r.bytes()
			if err != nil {
				return nil, err
			}
			k, v, err := decodeKeyValue(kb, 0)
			if err != nil {
				return nil, err
			}
			attrs[k] = v
			continue
		}
		if err := r.skip(wt); err != nil {
			return nil, err
		}
	}
	return attrs, nil
}

func decodeKeyValue(b []byte, depth int) (string, any, error) {
	var key string
	var val any
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return "", nil, err
		}
		switch f {
		case 1:
			if err := expect(wt, wtBytes); err != nil {
				return "", nil, err
			}
			v, err := r.bytes()
			if err != nil {
				return "", nil, err
			}
			key = string(v)
		case 2:
			if err := expect(wt, wtBytes); err != nil {
				return "", nil, err
			}
			vb, err := r.bytes()
			if err != nil {
				return "", nil, err
			}
			if val, err = decodeAnyValue(vb, depth+1); err != nil {
				return "", nil, err
			}
		default:
			if err := r.skip(wt); err != nil {
				return "", nil, err
			}
		}
	}
	return key, val, nil
}

func decodeAnyValue(b []byte, depth int) (any, error) {
	if depth > maxValueDepth {
		return nil, errTooDeep
	}
	var val any
	r := reader{b: b}
	for !r.done() {
		f, wt, err := r.key()
		if err != nil {
			return nil, err
		}
		switch f {
		case 1: // string_value
			if err := expect(wt, wtBytes); err != nil {
				return nil, err
			}
			v, err := r.bytes()
			if err != nil {
				return nil, err
			}
			val = string(v)
		case 2: // bool_value
			if err := expect(wt, wtVarint); err != nil {
				return nil, err
			}
			v, err := r.varint()
			if err != nil {
				return nil, err
			}
			val = v != 0
		case 3: // int_value
			if err := expect(wt, wtVarint); err != nil {
				return nil, err
			}
			v, err := r.varint()
			if err != nil {
				return nil, err
			}
			val = int64(v)
		case 4: // double_value
			if err := expect(wt, wtFixed64); err != nil {
				return nil, err
			}
			v, err := r.fixed64()
			if err != nil {
				return nil, err
			}
			val = math.Float64frombits(v)
		case 5: // array_value
			if err := expect(wt, wtBytes); err != nil {
				return nil, err
			}
			ab, err := r.bytes()
			if err != nil {
				return nil, err
			}
			arr := []any{}
			ar := reader{b: ab}
			for !ar.done() {
				af, awt, err := ar.key()
				if err != nil {
					return nil, err
				}
				if af == 1 && awt == wtBytes {
					eb, err := ar.bytes()
					if err != nil {
						return nil, err
					}
					ev, err := decodeAnyValue(eb, depth+1)
					if err != nil {
						return nil, err
					}
					arr = append(arr, ev)
					continue
				}
				if err := ar.skip(awt); err != nil {
					return nil, err
				}
			}
			val = arr
		case 6: // kvlist_value
			if err := expect(wt, wtBytes); err != nil {
				return nil, err
			}
			kb, err := r.bytes()
			if err != nil {
				return nil, err
			}
			m := map[string]any{}
			kr := reader{b: kb}
			for !kr.done() {
				kf, kwt, err := kr.key()
				if err != nil {
					return nil, err
				}
				if kf == 1 && kwt == wtBytes {
					eb, err := kr.bytes()
					if err != nil {
						return nil, err
					}
					k, v, err := decodeKeyValue(eb, depth+1)
					if err != nil {
						return nil, err
					}
					m[k] = v
					continue
				}
				if err := kr.skip(kwt); err != nil {
					return nil, err
				}
			}
			val = m
		case 7: // bytes_value
			if err := expect(wt, wtBytes); err != nil {
				return nil, err
			}
			v, err := r.bytes()
			if err != nil {
				return nil, err
			}
			val = hex.EncodeToString(v)
		default:
			if err := r.skip(wt); err != nil {
				return nil, err
			}
		}
	}
	return val, nil
}
