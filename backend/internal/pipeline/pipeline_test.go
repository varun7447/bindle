package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
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

func TestByteBudgetBackpressure(t *testing.T) {
	sink := &blockingSink{release: make(chan struct{})}
	b := NewBatcher(sink, 1000, 10, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.MaxBytes = 3 << 20
	big := model.Step{Input: strings.Repeat("x", 1<<20)}
	if err := b.Submit([]model.Step{big, big}); err != nil {
		t.Fatal(err)
	}
	if err := b.Submit([]model.Step{big, big}); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("over budget: %v", err)
	}
	if got := b.InflightBytes(); got < 2<<20 || got > 3<<20 {
		t.Fatalf("rejected request still counted: %d", got)
	}
	if err := b.Submit([]model.Step{{Input: "small"}}); err != nil {
		t.Fatalf("small step within budget: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	close(sink.release)
	cancel()
	b.Wait()
	if got := b.InflightBytes(); got != 0 {
		t.Fatalf("budget not released after write: %d", got)
	}
}

type blockingSink struct{ release chan struct{} }

func (s *blockingSink) Write(ctx context.Context, _ []model.Step) error {
	<-s.release
	return nil
}
