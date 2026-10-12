package datastore

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// MemBackend is an in-memory Backend with strict object-store semantics (no
// directories, no rename, whole-object puts). It is the reference for what a
// remote object store offers: the repository's tests run against it as well as
// against the filesystem backend, which is what keeps the repository free of POSIX
// assumptions. Not for production.
type MemBackend struct {
	mu   sync.Mutex
	objs map[string]memObj
	now  func() time.Time
	// FailPut, if set, is consulted before every Put (fault injection).
	FailPut func(key string) error
}

type memObj struct {
	data []byte
	mod  time.Time
}

// NewMemBackend returns an empty backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{objs: map[string]memObj{}, now: time.Now}
}

// Describe implements Backend.
func (m *MemBackend) Describe() string { return "memory" }

// SetClock replaces the clock (tests).
func (m *MemBackend) SetClock(f func() time.Time) { m.now = f }

// Put implements Backend.
func (m *MemBackend) Put(key string, data []byte, ifAbsent bool) error {
	if err := validKey(key); err != nil {
		return err
	}
	if m.FailPut != nil {
		if err := m.FailPut(key); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[key]; ok && ifAbsent {
		return ErrExists
	}
	m.objs[key] = memObj{data: append([]byte(nil), data...), mod: m.now()}
	return nil
}

// Get implements Backend.
func (m *MemBackend) Get(key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), o.data...), nil
}

// Stat implements Backend.
func (m *MemBackend) Stat(key string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return Info{}, ErrNotFound
	}
	return Info{Size: int64(len(o.data)), ModTime: o.mod}, nil
}

// SetModTime backdates an object (tests of GC grace).
func (m *MemBackend) SetModTime(key string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.objs[key]; ok {
		o.mod = t
		m.objs[key] = o
	}
}

// Corrupt flips a byte of an object (tests).
func (m *MemBackend) Corrupt(key string, at int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.objs[key]; ok && at < len(o.data) {
		o.data = append([]byte(nil), o.data...)
		o.data[at] ^= 0xff
		m.objs[key] = o
	}
}

// List implements Backend.
func (m *MemBackend) List(prefix string, fn func(string, Info) error) error {
	m.mu.Lock()
	var keys []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	m.mu.Unlock()
	sort.Strings(keys)
	for _, k := range keys {
		info, err := m.Stat(k)
		if err != nil {
			continue
		}
		if err := fn(k, info); err != nil {
			return err
		}
	}
	return nil
}

// ListPrefixes implements Backend.
func (m *MemBackend) ListPrefixes(prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	for k := range m.objs {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		if i := strings.Index(rest, "/"); i > 0 {
			seen[rest[:i]] = true
		}
	}
	var out []string
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// Delete implements Backend.
func (m *MemBackend) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}
