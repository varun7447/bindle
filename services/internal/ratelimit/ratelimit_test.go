package ratelimit

import (
	"testing"
	"time"
)

func newAt(rate, burst float64, now *time.Time) *Limiter {
	l := New(rate, burst)
	l.now = func() time.Time { return *now }
	return l
}

func TestBurstThenRefill(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newAt(10, 20, &now)
	if !l.Allow("a", 20) {
		t.Fatal("full burst should pass")
	}
	if l.Allow("a", 1) {
		t.Fatal("bucket should be empty")
	}
	now = now.Add(500 * time.Millisecond) // +5 tokens
	if !l.Allow("a", 5) || l.Allow("a", 1) {
		t.Fatal("refill should give exactly 5 tokens")
	}
	now = now.Add(time.Hour) // capped at burst
	if !l.Allow("a", 20) || l.Allow("a", 1) {
		t.Fatal("refill must cap at burst")
	}
}

func TestTenantsIsolated(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newAt(1, 1, &now)
	if !l.Allow("a", 1) || l.Allow("a", 1) {
		t.Fatal("tenant a should be exhausted")
	}
	if !l.Allow("b", 1) {
		t.Fatal("tenant b must not be affected by tenant a")
	}
}

func TestOversizedBatchRejectedWithoutDraining(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newAt(10, 10, &now)
	if l.Allow("a", 11) {
		t.Fatal("batch larger than burst must be rejected")
	}
	if !l.Allow("a", 10) {
		t.Fatal("rejected batch must not consume tokens")
	}
}

func TestDisabledAndNil(t *testing.T) {
	if !New(0, 0).Allow("a", 1<<30) {
		t.Fatal("rate 0 disables limiting")
	}
	var l *Limiter
	if !l.Allow("a", 1) {
		t.Fatal("nil limiter allows")
	}
}

func TestBurstAtLeastRate(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newAt(10, 1, &now)
	if !l.Allow("a", 10) {
		t.Fatal("burst should be raised to rate")
	}
}
