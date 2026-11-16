package pipeline

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/services/internal/model"
)

type memSink struct {
	mu    sync.Mutex
	n     int
	calls int
}

func (m *memSink) Write(_ context.Context, s []model.Step) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n += len(s)
	m.calls++
	return nil
}

func TestBatcherFlushesAndDrains(t *testing.T) {
	sink := &memSink{}
	b := NewBatcher(sink, 100, 10, time.Hour, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	if err := b.Submit(make([]model.Step, 25)); err != nil {
		t.Fatal(err)
	}
	cancel()
	b.Wait()
	if sink.n != 25 {
		t.Fatalf("want 25 written, got %d", sink.n)
	}
}

func TestSubmitIsAllOrNothing(t *testing.T) {
	b := NewBatcher(&memSink{}, 5, 10, time.Hour, slog.Default())
	if err := b.Submit(make([]model.Step, 6)); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("want backpressure, got %v", err)
	}
	if len(b.ch) != 0 {
		t.Fatalf("partial enqueue: %d", len(b.ch))
	}
}
