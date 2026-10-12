package datastore

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/unidoc/incus-backup/internal/format"
)

// Request parallelism. Every chunk question (Missing, the existence check of Commit, the index
// reads of the GC mark phase, the chunk reads of Verify, the GC sweep) is an independent request:
// on an object store each one costs a network round trip, so they are issued in parallel, bounded
// so that one repository cannot flood its service. See docs/performance.md for the measurements
// behind the defaults.
const (
	// DefaultRequestParallelism bounds the concurrent metadata requests (HEAD, small GET, DELETE,
	// LIST of one prefix) of one operation. Measured on MinIO behind a delay line (11,000 ids):
	// 32 -> 64 halves the time at 5-80 ms RTT and costs nothing at 0 ms; 64 requests per 20 ms
	// is ~3,200/s, below the per-prefix request rates S3 services document.
	DefaultRequestParallelism = 64
	// DefaultVerifyParallelism bounds the chunk blobs one verification holds in memory at a time
	// (each is at most MaxBlobSize; real chunks are at most 8 MiB plus a few bytes).
	DefaultVerifyParallelism = 16
	// verifyBatch is how many chunk ids a verification reads ahead before it records their
	// results in order (bounds the memory of a scrub of a huge repository).
	verifyBatch = 4096
)

// SetRequestParallelism overrides DefaultRequestParallelism (1 = strictly sequential).
func (s *Store) SetRequestParallelism(n int) { s.par.Store(int64(n)) }

func (s *Store) parallelism() int {
	if n := int(s.par.Load()); n > 0 {
		return n
	}
	return DefaultRequestParallelism
}

// forEach runs fn(0..n-1) on at most par goroutines. It stops handing out work after the first
// error and returns the error of the lowest index that failed. fn must not take s.gcMu: callers
// hold it around forEach when they need it (a recursive RLock deadlocks once a writer waits).
func forEach(par, n int, fn func(i int) error) error {
	if par > n {
		par = n
	}
	if par <= 1 {
		for i := 0; i < n; i++ {
			if err := fn(i); err != nil {
				return err
			}
		}
		return nil
	}
	var (
		next     atomic.Int64
		stop     atomic.Bool
		mu       sync.Mutex
		firstIdx = n
		firstErr error
		wg       sync.WaitGroup
	)
	for w := 0; w < par; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				if err := fn(i); err != nil {
					mu.Lock()
					if i < firstIdx {
						firstIdx, firstErr = i, err
					}
					mu.Unlock()
					stop.Store(true)
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// uniqueIDs returns the distinct non-zero ids of ids in order of first appearance.
func uniqueIDs(ids []format.ID) []format.ID {
	seen := make(map[format.ID]struct{}, len(ids))
	out := make([]format.ID, 0, len(ids))
	for _, id := range ids {
		if id.IsZero() {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// statChunks reports for every id of uniq whether its chunk object exists, asking the backend
// in parallel. With usePresence, ids the presence set knows to be present are not asked (see
// presence.go: only Missing may do that, never Commit). A backend error other than "not found"
// aborts the whole call.
func (s *Store) statChunks(uniq []format.ID, usePresence bool) ([]bool, error) {
	present := make([]bool, len(uniq))
	err := forEach(s.parallelism(), len(uniq), func(i int) error {
		id := uniq[i]
		if usePresence && s.presence.has(id) {
			present[i] = true
			return nil
		}
		ep := s.presence.epoch()
		_, err := s.b.Stat(chunkKey(id))
		switch {
		case err == nil:
			present[i] = true
			s.presence.observed(id, ep)
		case errors.Is(err, ErrNotFound):
			s.presence.evict(id)
		default:
			return err
		}
		return nil
	})
	return present, err
}

// statBeforePut: whether PutChunk first asks if the chunk exists (see putChunkLocked).
func statBeforePut(b Backend) bool {
	// Unwrap: production stores sit behind a ModeBackend, which must not hide the S3 backend.
	_, remote := Unwrap(b).(*S3Backend)
	return !remote
}
