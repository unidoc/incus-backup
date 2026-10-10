package datastore

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
)

// The presence set: an in-memory answer to "is chunk X stored?" for repositories on a REMOTE
// backend, where every HEAD costs a network round trip and an incremental backup of a large disk
// asks about every one of its chunks.
//
// It is a repository-local optimisation that the engine never sees, and it holds POSITIVES only:
//
//   - It is loaded from a full LIST of chunks/ (and re-listed every PresenceRefresh), plus every
//     chunk this process has itself stored or seen with a HEAD since.
//   - This process removes an id BEFORE it deletes the object (GC sweep, quarantine), and any
//     answer from a request that was already in flight when a removal happened is dropped (the
//     generation check), so a deletion by this process can never leave a stale positive behind.
//     A deletion during a re-list is a tombstone applied to the new listing.
//   - An id that is not in the set is asked of the backend as before. A "not found" answer
//     evicts it.
//
// What it cannot know is a deletion by someone else (an operator, a bucket lifecycle rule, a
// listing that still shows an object deleted just before: the Backend contract bounds what a
// listing may MISS, not what it may still show). Therefore it is used ONLY by Missing, whose
// answer the client re-checks anyway, and NEVER by Commit: Commit asks the backend about every
// chunk after pinning them, so a stale positive costs one refused commit (409, missing chunks),
// whose ids are evicted here, and the client uploads them again; it can never make a snapshot
// with a missing chunk visible.
const (
	// PresenceRefresh is how often the set is rebuilt from a full listing (bounds how long a
	// deletion by someone else can make Missing answer wrongly before a commit corrects it).
	PresenceRefresh = 6 * time.Hour
	// PresenceMaxEntries caps the memory of the set: 80 bytes per id (measured, Go 1.26 map), so
	// about 160 MiB, twice that while a re-list replaces it. A repository with more chunks than this
	// (8 TiB of 4 MiB block chunks) does without it: every Missing id is a HEAD again.
	PresenceMaxEntries = 2 << 20
)

var errPresenceFull = errors.New("presence set: too many chunks")

type presenceSet struct {
	mu       sync.RWMutex
	on       bool // in use for this store
	ready    bool // a complete listing is loaded
	ids      map[format.ID]struct{}
	building bool
	added    map[format.ID]struct{} // stored while a listing runs
	tomb     map[format.ID]struct{} // removed while a listing runs
	gen      uint64                 // bumped by every removal ahead of a delete
	builtAt  time.Time
	max      int
	refresh  time.Duration
	now      func() time.Time
	builds   int // completed listings (tests)
	lastErr  error
}

func (p *presenceSet) has(id format.ID) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.ready {
		return false
	}
	_, ok := p.ids[id]
	return ok
}

// epoch is taken before a request whose positive answer may be recorded with observed.
func (p *presenceSet) epoch() uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.gen
}

// observed records that the chunk existed (a HEAD answered or a PUT succeeded) for a request
// that started at epoch ep. It is dropped if this process removed anything since.
func (p *presenceSet) observed(id format.ID, ep uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.on || p.gen != ep {
		return
	}
	if p.building {
		delete(p.tomb, id)
		p.added[id] = struct{}{}
	}
	if p.ready && len(p.ids) < p.max {
		p.ids[id] = struct{}{}
	}
}

// remove is called BEFORE this process deletes a chunk object (and again after it).
func (p *presenceSet) remove(id format.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gen++
	p.forget(id)
}

// evict forgets an id the backend just said is not there.
func (p *presenceSet) evict(id format.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.forget(id)
}

func (p *presenceSet) forget(id format.ID) {
	if !p.on {
		return
	}
	if p.ids != nil {
		delete(p.ids, id)
	}
	if p.building {
		delete(p.added, id)
		p.tomb[id] = struct{}{}
	}
}

// due reports whether a (re)build should start now, and marks it started.
func (p *presenceSet) start() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.on || p.building || (!p.builtAt.IsZero() && p.now().Sub(p.builtAt) < p.refresh) {
		return false
	}
	p.building = true
	p.added = map[format.ID]struct{}{}
	p.tomb = map[format.ID]struct{}{}
	return true
}

// SetPresenceCache switches the presence set on or off (it is on by default for S3 backends).
func (s *Store) SetPresenceCache(on bool) {
	s.presence.mu.Lock()
	defer s.presence.mu.Unlock()
	s.presence.on = on
	if !on {
		s.presence.ready, s.presence.ids, s.presence.builtAt = false, nil, time.Time{}
	}
}

// refreshPresence starts a background (re)build of the presence set when one is due. Missing
// keeps asking the backend until the listing is complete.
func (s *Store) refreshPresence() {
	if s.presence.start() {
		go s.buildPresence()
	}
}

// RebuildPresence lists chunks/ now and replaces the presence set (tests; normally it is done
// in the background by Missing). It is a no-op when the set is off.
func (s *Store) RebuildPresence() error {
	p := &s.presence
	p.mu.Lock()
	if !p.on {
		p.mu.Unlock()
		return nil
	}
	for p.building {
		p.mu.Unlock()
		time.Sleep(time.Millisecond)
		p.mu.Lock()
	}
	p.building = true
	p.added = map[format.ID]struct{}{}
	p.tomb = map[format.ID]struct{}{}
	p.mu.Unlock()
	s.buildPresence()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastErr
}

func (s *Store) buildPresence() {
	p := &s.presence
	fresh := map[format.ID]struct{}{}
	err := s.listChunkObjects(func(key string, _ Info) error {
		id, perr := format.ParseID(key[strings.LastIndex(key, "/")+1:])
		if perr != nil || key != chunkKey(id) {
			return nil // not a chunk at its canonical place: a HEAD of chunkKey(id) would not find it
		}
		fresh[id] = struct{}{}
		if len(fresh) > p.max {
			return errPresenceFull
		}
		return nil
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	p.building = false
	p.builtAt = p.now()
	p.lastErr = err
	if err != nil || !p.on {
		p.ready, p.ids, p.added, p.tomb = false, nil, nil, nil
		return
	}
	for id := range p.added {
		fresh[id] = struct{}{}
	}
	for id := range p.tomb {
		delete(fresh, id)
	}
	p.ids, p.ready, p.added, p.tomb = fresh, true, nil, nil
	p.builds++
}

// PresenceStats describes the presence set (tests, diagnostics).
type PresenceStats struct {
	On, Ready bool
	Entries   int
	Builds    int
	BuiltAt   time.Time
}

// PresenceStats returns the state of the presence set.
func (s *Store) PresenceStats() PresenceStats {
	p := &s.presence
	p.mu.RLock()
	defer p.mu.RUnlock()
	return PresenceStats{On: p.on, Ready: p.ready, Entries: len(p.ids), Builds: p.builds, BuiltAt: p.builtAt}
}
