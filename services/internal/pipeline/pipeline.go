// Package pipeline batches normalized steps and writes them to a sink.
//
// v0 runs in-process (ingest -> batcher -> OpenSearch). In v0.2 the ingest
// edge publishes to Kinesis and a separate processor service runs this same
// batcher on the consumer side; the Sink interface stays the same.
package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/bindle-ai/bindle/services/internal/model"
)

// ErrBackpressure means the buffer is full; the edge returns 503 so OTLP
// exporters retry later instead of the service running out of memory.
var ErrBackpressure = errors.New("pipeline: buffer full")

// Sink stores a batch of steps.
type Sink interface {
	Write(ctx context.Context, steps []model.Step) error
}

// Batcher buffers steps and flushes by size or time.
type Batcher struct {
	ch         chan model.Step
	sink       Sink
	maxBatch   int
	flushEvery time.Duration
	log        *slog.Logger
	done       chan struct{}
}

// NewBatcher creates a batcher with a bounded buffer.
func NewBatcher(sink Sink, buffer, maxBatch int, flushEvery time.Duration, log *slog.Logger) *Batcher {
	return &Batcher{
		ch:         make(chan model.Step, buffer),
		sink:       sink,
		maxBatch:   maxBatch,
		flushEvery: flushEvery,
		log:        log,
		done:       make(chan struct{}),
	}
}

// Submit enqueues all steps or none (so a request is never half-accepted).
func (b *Batcher) Submit(steps []model.Step) error {
	if cap(b.ch)-len(b.ch) < len(steps) {
		return ErrBackpressure
	}
	for _, s := range steps {
		select {
		case b.ch <- s:
		default:
			return ErrBackpressure
		}
	}
	return nil
}

// Run flushes until ctx is cancelled, then drains what is buffered.
func (b *Batcher) Run(ctx context.Context) {
	defer close(b.done)
	t := time.NewTicker(b.flushEvery)
	defer t.Stop()
	batch := make([]model.Step, 0, b.maxBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		b.write(batch)
		batch = make([]model.Step, 0, b.maxBatch)
	}
	for {
		select {
		case s := <-b.ch:
			batch = append(batch, s)
			if len(batch) >= b.maxBatch {
				flush()
			}
		case <-t.C:
			flush()
		case <-ctx.Done():
			for {
				select {
				case s := <-b.ch:
					batch = append(batch, s)
				default:
					flush()
					return
				}
			}
		}
	}
}

// Wait blocks until Run has drained and returned.
func (b *Batcher) Wait() { <-b.done }

func (b *Batcher) write(batch []model.Step) {
	backoff := 200 * time.Millisecond
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := b.sink.Write(ctx, batch)
		cancel()
		if err == nil {
			return
		}
		b.log.Warn("sink write failed", "attempt", attempt, "steps", len(batch), "err", err)
		time.Sleep(backoff)
		backoff *= 2
	}
	// v0: drop after retries. v0.2: Kinesis retention makes this replayable.
	b.log.Error("dropping batch after retries", "steps", len(batch))
}
