// Package ratelimit is a per-tenant token bucket so one runaway agent loop
// cannot starve other tenants.
package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter allows `rate` spans per second per tenant with a burst of `burst`.
type Limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
}

// New creates a limiter. rate <= 0 disables limiting.
func New(rate, burst float64) *Limiter {
	if burst < rate {
		burst = rate
	}
	return &Limiter{rate: rate, burst: burst, buckets: map[string]*bucket{}, now: time.Now}
}

// Allow reports whether n spans may be accepted for tenant now.
func (l *Limiter) Allow(tenant string, n int) bool {
	if l == nil || l.rate <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[tenant]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[tenant] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}
