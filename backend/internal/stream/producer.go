package stream

import (
	"context"
	"errors"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/payload"
	"github.com/bindle-ai/bindle/backend/internal/pipeline"
)

// Producer puts each ingest request's steps on the stream as one record,
// partitioned by tenant. It implements ingest.Submitter; Kinesis throttling
// surfaces as pipeline.ErrBackpressure, which ingest answers with 503 so
// exporters retry.
type Producer struct {
	Kinesis *Kinesis
	Stream  string
	Spool   payload.Store
	Timeout time.Duration // default 5s
}

// Submit implements ingest.Submitter.
func (p *Producer) Submit(steps []model.Step) error {
	if len(steps) == 0 {
		return nil
	}
	t := p.Timeout
	if t <= 0 {
		t = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()
	data, err := Encode(ctx, steps, p.Spool, time.Now())
	if err != nil {
		return err
	}
	err = p.Kinesis.PutRecord(ctx, p.Stream, steps[0].TenantID, data)
	if Throttled(err) || errors.Is(err, context.DeadlineExceeded) {
		return pipeline.ErrBackpressure
	}
	return err
}
