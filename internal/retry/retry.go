// Package retry runs a function with exponential backoff, for the transient
// failures a call to Apple's own services routinely hits: a dropped
// connection, a 503 while a datacenter is unhappy, a 429 while it recovers.
package retry

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Config controls the backoff schedule.
type Config struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// Default is four attempts spanning at most a few seconds — enough to ride
// out a blip without holding an HTTP request open long enough that the
// caller assumes it hung.
func Default() Config {
	return Config{MaxAttempts: 4, BaseDelay: 300 * time.Millisecond, MaxDelay: 5 * time.Second}
}

// Afterer is implemented by errors that dictate their own wait, such as a 429
// carrying Apple's Retry-After header.
type Afterer interface {
	RetryAfter() (time.Duration, bool)
}

// Do calls fn until it succeeds, ctx ends, attempts run out, or retryable
// reports the error as not worth retrying.
func Do(ctx context.Context, cfg Config, retryable func(error) bool, fn func() error) error {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	delay := cfg.BaseDelay

	var err error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if ctx.Err() != nil || !retryable(err) || attempt == cfg.MaxAttempts {
			return err
		}

		wait := delay
		if a, ok := err.(Afterer); ok {
			if d, has := a.RetryAfter(); has {
				wait = d
			}
		}
		if wait > cfg.MaxDelay {
			wait = cfg.MaxDelay
		}
		jittered := time.Duration(float64(wait) * (0.5 + rand.Float64()))

		slog.Debug("retrying after a transient failure",
			"attempt", attempt, "wait", jittered, "error", err)

		t := time.NewTimer(jittered)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}

		delay *= 2
		if delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}
	}
	return err
}
