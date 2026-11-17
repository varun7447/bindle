package stream

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/payload"
)

// A record is either the gzipped batch itself or, for batches too big for
// a Kinesis record (1 MiB), a small JSON pointer to the batch in the spool
// store ("claim check").

const maxInline = 900 << 10

type batch struct {
	V     int          `json:"v"`
	Steps []model.Step `json:"steps"`
}

type pointer struct {
	V     int    `json:"v"`
	Spool string `json:"spool"`
}

func gz(v any) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Encode builds a record for steps, spooling to store if needed.
func Encode(ctx context.Context, steps []model.Step, spool payload.Store, now time.Time) ([]byte, error) {
	data, err := gz(batch{V: 1, Steps: steps})
	if err != nil {
		return nil, err
	}
	if len(data) <= maxInline {
		return data, nil
	}
	if spool == nil {
		return nil, errors.New("stream: batch exceeds a Kinesis record and no spool store is configured")
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	key := "spool/" + now.UTC().Format("2006/01/02") + "/" + hex.EncodeToString(b) + ".json.gz"
	if err := spool.Put(ctx, key, data); err != nil {
		return nil, fmt.Errorf("stream: spool: %w", err)
	}
	return json.Marshal(pointer{V: 1, Spool: key})
}

var spoolKeyRe = regexp.MustCompile(`^spool/\d{4}/\d{2}/\d{2}/[0-9a-f]{32}\.json\.gz$`)

const maxBatchBytes = 64 << 20

// Decode reads a record produced by Encode.
func Decode(ctx context.Context, data []byte, spool payload.Store) ([]model.Step, error) {
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		return gunzip(data)
	}
	var p pointer
	if err := json.Unmarshal(data, &p); err != nil || p.V != 1 {
		return nil, errors.New("stream: unknown record format")
	}
	if !spoolKeyRe.MatchString(p.Spool) {
		return nil, errors.New("stream: invalid spool key")
	}
	if spool == nil {
		return nil, errors.New("stream: spooled record but no spool store configured")
	}
	b, truncated, err := spool.Get(ctx, p.Spool, maxBatchBytes)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, errors.New("stream: spooled batch too large")
	}
	return gunzip(b)
}

func gunzip(data []byte) ([]model.Step, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var b batch
	if err := json.NewDecoder(io.LimitReader(r, maxBatchBytes)).Decode(&b); err != nil {
		return nil, err
	}
	if b.V != 1 {
		return nil, errors.New("stream: unknown batch version")
	}
	return b.Steps, nil
}
