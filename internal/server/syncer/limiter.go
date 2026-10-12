package syncer

import (
	"context"
	"sync"
	"time"
)

// limiter caps blob traffic at rate bytes per second (shared by all transfer goroutines of a
// run). It is a simple "next free time" scheduler: each transfer reserves len/rate seconds
// after the previous reservation and sleeps until its slot starts, so the long-run average
// never exceeds the rate and bursts are bounded by one blob.
type limiter struct {
	mu   sync.Mutex
	rate float64 // bytes per second
	next time.Time
	now  func() time.Time
}

func newLimiter(bytesPerSecond int64) *limiter {
	return &limiter{rate: float64(bytesPerSecond), now: time.Now}
}

func (l *limiter) wait(ctx context.Context, n int) error {
	l.mu.Lock()
	now := l.now()
	start := l.next
	if start.Before(now) {
		start = now
	}
	l.next = start.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	delay := start.Sub(now)
	l.mu.Unlock()
	if delay <= 0 {
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
