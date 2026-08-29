// Package ratelimit is a minimal token-bucket limiter.
//
// It is self-contained (no third-party dependency) because it is used in two
// very different spots: an HTTP middleware that must reject instantly, and an
// outbound client that should pace itself instead of bursting at Apple.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Limiter is a token bucket: it holds up to burst tokens and refills at rate
// tokens per second.
type Limiter struct {
	mu       sync.Mutex
	rate     float64
	burst    float64
	tokens   float64
	lastFill time.Time
}

// New returns a Limiter. A non-positive rate disables limiting: Allow and
// Wait then always succeed immediately.
func New(rate float64, burst int) *Limiter {
	if burst < 1 {
		burst = 1
	}
	return &Limiter{
		rate:     rate,
		burst:    float64(burst),
		tokens:   float64(burst),
		lastFill: time.Now(),
	}
}

// refill must be called with mu held.
func (l *Limiter) refill() {
	now := time.Now()
	elapsed := now.Sub(l.lastFill).Seconds()
	l.lastFill = now
	l.tokens += elapsed * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
}

// Allow reports whether a token is available right now, consuming it if so.
// It never blocks, which is what an HTTP middleware needs.
func (l *Limiter) Allow() bool {
	if l.rate <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refill()
	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}

// Wait blocks until a token is available or ctx ends. It is meant for
// outbound calls, which should be paced rather than rejected.
func (l *Limiter) Wait(ctx context.Context) error {
	if l.rate <= 0 {
		return nil
	}
	for {
		l.mu.Lock()
		l.refill()
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		need := 1 - l.tokens
		wait := time.Duration(need / l.rate * float64(time.Second))
		l.mu.Unlock()

		if wait <= 0 {
			wait = time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
