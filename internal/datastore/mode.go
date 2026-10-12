package datastore

import (
	"errors"
	"sync/atomic"
	"time"
)

// Mode is the maintenance mode of a repository.
type Mode int32

// Modes. Online is normal operation. ReadOnly lets restores, browsing and verification-free reads continue
// while every write (new backups, retention, GC, verify state, quarantine, sync receive) is refused, so a
// storage migration or a snapshot of the datastore sees a quiet repository. Offline refuses every access
// to the datastore: it is for taking the storage away.
const (
	ModeOnline Mode = iota
	ModeReadOnly
	ModeOffline
)

// Maintenance-mode errors. Hosts see them as "temporarily unavailable" and retry later.
var (
	ErrReadOnly = errors.New("repository is in read-only maintenance mode")
	ErrOffline  = errors.New("repository is offline for maintenance")
)

// String is the name used in the API and the database.
func (m Mode) String() string {
	switch m {
	case ModeReadOnly:
		return "read-only"
	case ModeOffline:
		return "offline"
	}
	return "online"
}

// ParseMode converts the API/database name.
func ParseMode(s string) (Mode, bool) {
	switch s {
	case "online", "":
		return ModeOnline, true
	case "read-only":
		return ModeReadOnly, true
	case "offline":
		return ModeOffline, true
	}
	return ModeOnline, false
}

// ModeBackend wraps a Backend and enforces a maintenance mode that can change while the repository is in
// use. It sits below the whole object model, so no code path can write around it.
type ModeBackend struct {
	Backend
	mode atomic.Int32
}

// NewModeBackend wraps b in the given mode.
func NewModeBackend(b Backend, m Mode) *ModeBackend {
	mb := &ModeBackend{Backend: b}
	mb.mode.Store(int32(m))
	return mb
}

// SetMode changes the mode; it takes effect for the next backend call.
func (b *ModeBackend) SetMode(m Mode) { b.mode.Store(int32(m)) }

// Mode returns the current mode.
func (b *ModeBackend) Mode() Mode { return Mode(b.mode.Load()) }

func (b *ModeBackend) read() error {
	if b.Mode() == ModeOffline {
		return ErrOffline
	}
	return nil
}

func (b *ModeBackend) write() error {
	switch b.Mode() {
	case ModeOffline:
		return ErrOffline
	case ModeReadOnly:
		return ErrReadOnly
	}
	return nil
}

func (b *ModeBackend) Put(key string, data []byte, ifAbsent bool) error {
	if err := b.write(); err != nil {
		return err
	}
	return b.Backend.Put(key, data, ifAbsent)
}

func (b *ModeBackend) Get(key string) ([]byte, error) {
	if err := b.read(); err != nil {
		return nil, err
	}
	return b.Backend.Get(key)
}

func (b *ModeBackend) Stat(key string) (Info, error) {
	if err := b.read(); err != nil {
		return Info{}, err
	}
	return b.Backend.Stat(key)
}

func (b *ModeBackend) List(prefix string, fn func(string, Info) error) error {
	if err := b.read(); err != nil {
		return err
	}
	return b.Backend.List(prefix, fn)
}

func (b *ModeBackend) ListPrefixes(prefix string) ([]string, error) {
	if err := b.read(); err != nil {
		return nil, err
	}
	return b.Backend.ListPrefixes(prefix)
}

func (b *ModeBackend) Delete(key string) error {
	if err := b.write(); err != nil {
		return err
	}
	return b.Backend.Delete(key)
}

// CleanTmp removes crash leftovers of the wrapped backend (a write: refused unless online).
func (b *ModeBackend) CleanTmp(olderThan time.Duration) (int, error) {
	if err := b.write(); err != nil {
		return 0, err
	}
	if c, ok := b.Backend.(interface {
		CleanTmp(time.Duration) (int, error)
	}); ok {
		return c.CleanTmp(olderThan)
	}
	return 0, nil
}

// Unwrap returns the wrapped backend.
func (b *ModeBackend) Unwrap() Backend { return b.Backend }

// Unwrap peels wrappers (maintenance mode, caches) off a backend to reach the storage underneath.
func Unwrap(b Backend) Backend {
	for {
		u, ok := b.(interface{ Unwrap() Backend })
		if !ok {
			return b
		}
		b = u.Unwrap()
	}
}
