package demo

import (
	"sync"
	"time"
)

// Clock is the one clock the server service, the auth store and every simulated host read. While the
// seed runs it is a simulated time that only the seeder advances; afterwards it follows the real clock.
// Handler goroutines of the agent API read it concurrently, hence the mutex.
type Clock struct {
	mu   sync.Mutex
	t    time.Time
	real bool
}

// NewSimClock starts a simulated clock at t.
func NewSimClock(t time.Time) *Clock { return &Clock{t: t} }

// NewRealClock returns a clock that follows time.Now.
func NewRealClock() *Clock { return &Clock{real: true} }

// Now returns the current (simulated or real) time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.real {
		return time.Now()
	}
	return c.t
}

// Set moves a simulated clock to t (the seed only ever moves it forward).
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	c.t, c.real = t, false
	c.mu.Unlock()
}

// Real switches to the real clock for good.
func (c *Clock) Real() {
	c.mu.Lock()
	c.real = true
	c.mu.Unlock()
}
