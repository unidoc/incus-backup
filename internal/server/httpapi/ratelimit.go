package httpapi

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// limiter is a small per-IP token bucket. Enrolment is the only endpoint
// reachable without a client certificate, so it is the only one limited: a
// guessing attack on one-time tokens (256 bits, so hopeless) still must not be
// able to hammer the database.
type limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(perMinute, burst int) *limiter {
	return &limiter{rate: float64(perMinute) / 60, burst: float64(burst), buckets: map[string]*bucket{}, now: time.Now}
}

func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[ip]
	if b == nil {
		if len(l.buckets) > 10000 { // bound memory under address spraying, but never forget a limited address
			for k, v := range l.buckets {
				if v.tokens >= l.burst || now.Sub(v.last) > 10*time.Minute {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) > 20000 {
				return false // under a flood from many addresses, enrolment simply waits
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func clientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}
