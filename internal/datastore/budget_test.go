package datastore

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// With a key buffer far smaller than one shard, the sharded listing still delivers exactly what
// one listing does, in order, on every backend.
func TestShardedListingWithATinyBufferMatchesOneListing(t *testing.T) {
	old := ListBufferMax
	ListBufferMax = 2
	defer func() { ListBufferMax = old }()
	each(t, func(t *testing.T, s *Store) {
		putChunks(t, s, testKeys(t), "buf", 300)
		var want, got []string
		if err := s.b.List("chunks/", func(key string, _ Info) error { want = append(want, key); return nil }); err != nil {
			t.Fatal(err)
		}
		for _, par := range []int{1, 4, 64} {
			s.SetRequestParallelism(par)
			got = got[:0]
			if err := s.listChunkObjects(func(key string, _ Info) error { got = append(got, key); return nil }); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("par=%d: %d keys vs %d", par, len(got), len(want))
			}
		}
	})
}

// The byte budget bounds what is admitted at once, settles to real sizes, and never deadlocks.
func TestByteBudgetBoundsAndProgresses(t *testing.T) {
	b := newByteBudget(100)
	var cur, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := b.reserve()
			n := b.settle(r, int64(10+i%3*20)) // 10, 30 or 50 bytes
			v := cur.Add(n)
			for {
				p := peak.Load()
				if v <= p || peak.CompareAndSwap(p, v) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			cur.Add(-n)
			b.release(n)
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock")
	}
	// once the estimate has grown to 50, at most 2 items fit; the first items may overshoot once
	if peak.Load() > 100+64*50 {
		t.Fatalf("peak %d", peak.Load())
	}
	if b.used != 0 {
		t.Fatalf("budget leaks %d bytes", b.used)
	}
}

// GC with a mark budget smaller than one index and a tiny listing buffer gives exactly the result
// of an unconstrained GC.
func TestGCUnderTightMemoryBoundsIsUnchanged(t *testing.T) {
	run := func(budget int64, buf, batch int) (*GCStats, []string) {
		oldB, oldL, oldS := MarkIndexBudget, ListBufferMax, gcSweepBatch
		MarkIndexBudget, ListBufferMax, gcSweepBatch = budget, buf, batch
		defer func() { MarkIndexBudget, ListBufferMax, gcSweepBatch = oldB, oldL, oldS }()
		s, _, _ := fabricate(t, 3000)
		s.now = func() time.Time { return time.Now().Add(72 * time.Hour) }
		st, err := s.GC(context.Background(), 24*time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CheckInvariants(); err != nil {
			t.Fatal(err)
		}
		var left []string
		_ = s.b.List("chunks/", func(key string, _ Info) error { left = append(left, key); return nil })
		return st, left
	}
	st0, left0 := run(256<<20, 1<<19, 4096)
	st1, left1 := run(1, 2, 7)
	if st0.Removed == 0 || st0.Removed != st1.Removed || st0.Live != st1.Live || !reflect.DeepEqual(left0, left1) {
		t.Fatalf("tight GC differs: %+v vs %+v (%d vs %d chunks left)", st0, st1, len(left0), len(left1))
	}
}
