package otlp

import (
	"encoding/binary"
	"errors"
)

// A minimal protobuf wire-format reader. OTLP's trace schema is small and
// stable, so decoding it by hand keeps the ingest binary dependency-free.

var (
	errTruncated = errors.New("otlp: truncated message")
	errOverflow  = errors.New("otlp: varint overflow")
	errWireType  = errors.New("otlp: unexpected wire type")
	errTooDeep   = errors.New("otlp: value nesting too deep")
)

const (
	wtVarint  = 0
	wtFixed64 = 1
	wtBytes   = 2
	wtFixed32 = 5
)

type reader struct {
	b []byte
	i int
}

func (r *reader) done() bool { return r.i >= len(r.b) }

func (r *reader) varint() (uint64, error) {
	var x uint64
	var s uint
	for n := 0; n < 10; n++ {
		if r.i >= len(r.b) {
			return 0, errTruncated
		}
		c := r.b[r.i]
		r.i++
		x |= uint64(c&0x7f) << s
		if c < 0x80 {
			return x, nil
		}
		s += 7
	}
	return 0, errOverflow
}

func (r *reader) key() (field int, wt int, err error) {
	k, err := r.varint()
	if err != nil {
		return 0, 0, err
	}
	return int(k >> 3), int(k & 7), nil
}

func (r *reader) bytes() ([]byte, error) {
	n, err := r.varint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(r.b)-r.i) {
		return nil, errTruncated
	}
	v := r.b[r.i : r.i+int(n)]
	r.i += int(n)
	return v, nil
}

func (r *reader) fixed64() (uint64, error) {
	if len(r.b)-r.i < 8 {
		return 0, errTruncated
	}
	v := binary.LittleEndian.Uint64(r.b[r.i:])
	r.i += 8
	return v, nil
}

func (r *reader) fixed32() (uint32, error) {
	if len(r.b)-r.i < 4 {
		return 0, errTruncated
	}
	v := binary.LittleEndian.Uint32(r.b[r.i:])
	r.i += 4
	return v, nil
}

func (r *reader) skip(wt int) error {
	var err error
	switch wt {
	case wtVarint:
		_, err = r.varint()
	case wtFixed64:
		_, err = r.fixed64()
	case wtBytes:
		_, err = r.bytes()
	case wtFixed32:
		_, err = r.fixed32()
	default:
		err = errWireType
	}
	return err
}

func expect(got, want int) error {
	if got != want {
		return errWireType
	}
	return nil
}
