package datastore

import "sync"

// MarkIndexBudget bounds the index bytes (raw and parsed) GC's parallel mark phase holds at once.
// Small indexes are read with the full request parallelism; a repository of very large indexes
// (a 4 TiB disk has a 40 MB index) is read only a few at a time.
var MarkIndexBudget int64 = 256 << 20

// byteBudget admits work items whose size is only known once they are read: an item reserves the
// largest size seen so far, then settles to its real size (which may overshoot once: the data is
// already in memory). It never deadlocks: a reservation waits only while others hold some, and
// holders never wait for more.
type byteBudget struct {
	mu          sync.Mutex
	cond        *sync.Cond
	limit, used int64
	est         int64
}

func newByteBudget(limit int64) *byteBudget {
	b := &byteBudget{limit: limit, est: 1 << 20}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *byteBudget) reserve() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.used > 0 && b.used+b.est > b.limit {
		b.cond.Wait()
	}
	b.used += b.est
	return b.est
}

func (b *byteBudget) settle(reserved, actual int64) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used += actual - reserved
	if actual > b.est {
		b.est = actual
	}
	if actual < reserved {
		b.cond.Broadcast()
	}
	return actual
}

func (b *byteBudget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
	b.cond.Broadcast()
}
