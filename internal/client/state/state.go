// Package state is the client's small local cache. It never decides
// correctness: the servers are the source of truth for what was committed, and
// Incus is the source of truth for which bitmaps exist. The cache only records
// scheduling information and diagnostics.
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ObjectState is what the client remembers about one storage object.
type ObjectState struct {
	LastRun    time.Time `json:"last_run"`
	LastStatus string    `json:"last_status"`
	LastError  string    `json:"last_error,omitempty"`
	LastSnap   string    `json:"last_snapshot,omitempty"`
}

// State is state.json.
type State struct {
	mu      sync.Mutex
	path    string
	Objects map[string]*ObjectState `json:"objects"` // key: project/kind/name
	Runs    map[string]time.Time    `json:"runs"`    // policy name -> last scheduled run start
	LastRun time.Time               `json:"last_run"`
	Verify  map[string]time.Time    `json:"verify"` // "verify:<server>" -> last deep verification
	Retry   map[string]time.Time    `json:"retry"`  // policy -> earliest retry after a failed run
	Held    map[string]string       `json:"held"`   // "object|target" -> last snapshot that target is known to hold
}

// Load reads path (missing file = empty state).
func Load(dir string) (*State, error) {
	s := &State{path: filepath.Join(dir, "state.json"), Objects: map[string]*ObjectState{}, Runs: map[string]time.Time{}, Verify: map[string]time.Time{}, Retry: map[string]time.Time{}, Held: map[string]string{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Objects == nil {
		s.Objects = map[string]*ObjectState{}
	}
	if s.Runs == nil {
		s.Runs = map[string]time.Time{}
	}
	if s.Verify == nil {
		s.Verify = map[string]time.Time{}
	}
	if s.Retry == nil {
		s.Retry = map[string]time.Time{}
	}
	if s.Held == nil {
		s.Held = map[string]string{}
	}
	return s, nil
}

// Update applies fn under the lock and persists atomically.
func (s *State) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Object returns a copy of an object's state.
func (s *State) Object(key string) ObjectState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.Objects[key]; ok {
		return *o
	}
	return ObjectState{}
}

// Last implements engine.Memory.
func (s *State) Last(object, target string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Held[object+"|"+target]
}

// SetLast implements engine.Memory (persisted lazily: a lost write only costs a
// slower bitmap clean-up, never correctness).
func (s *State) SetLast(object, target, snap string) {
	s.mu.Lock()
	cur := s.Held[object+"|"+target]
	s.mu.Unlock()
	if cur == snap {
		return
	}
	_ = s.Update(func(st *State) { st.Held[object+"|"+target] = snap })
}
