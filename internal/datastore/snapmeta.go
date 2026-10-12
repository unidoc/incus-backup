package datastore

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
)

// Per-snapshot metadata lives in the repository next to the snapshot, so rebuilding
// PostgreSQL from the datastore (reconcile) can never lose it:
//
//	groups/<g>/<s>/protected    empty-ish marker; retention and forget refuse to remove the snapshot
//	groups/<g>/<s>/notes        free text
//	groups/<g>/<s>/verify.json  last verification result and a short history
//	groups/<g>/notes            free text for the whole group
//
// These objects are plaintext and visible to the server by design (names of objects are
// control-plane data too); they hold no payload information.

// MaxNotes bounds a note.
const MaxNotes = 16 << 10

const maxVerifyHistory = 20

func (s *Store) requireCommitted(group, snap string) error {
	if err := checkIDs(group, snap); err != nil {
		return err
	}
	if _, err := s.b.Stat(snapPrefix(group, snap) + "commit.json"); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// SetProtected sets or clears the protection of a committed snapshot.
func (s *Store) SetProtected(group, snap string, protected bool) error {
	if err := s.requireCommitted(group, snap); err != nil {
		return err
	}
	key := snapPrefix(group, snap) + "protected"
	if protected {
		return s.b.Put(key, []byte("protected\n"), false)
	}
	return s.b.Delete(key)
}

// IsProtected reports whether the snapshot is protected.
func (s *Store) IsProtected(group, snap string) (bool, error) {
	if err := checkIDs(group, snap); err != nil {
		return false, err
	}
	_, err := s.b.Stat(snapPrefix(group, snap) + "protected")
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// SetNotes stores (or, with "", removes) the notes of a snapshot.
func (s *Store) SetNotes(group, snap, text string) error {
	if len(text) > MaxNotes {
		return errors.New("notes too long")
	}
	if err := s.requireCommitted(group, snap); err != nil {
		return err
	}
	key := snapPrefix(group, snap) + "notes"
	if text == "" {
		return s.b.Delete(key)
	}
	return s.b.Put(key, []byte(text), false)
}

// Notes returns the notes of a snapshot ("" when there are none).
func (s *Store) Notes(group, snap string) (string, error) {
	if err := checkIDs(group, snap); err != nil {
		return "", err
	}
	b, err := s.b.Get(snapPrefix(group, snap) + "notes")
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return string(b), err
}

// SetGroupNotes stores the notes of a group.
func (s *Store) SetGroupNotes(group, text string) error {
	if !ValidGroup(group) {
		return errors.New("invalid group")
	}
	if len(text) > MaxNotes {
		return errors.New("notes too long")
	}
	key := "groups/" + group + "/notes"
	if text == "" {
		return s.b.Delete(key)
	}
	return s.b.Put(key, []byte(text), false)
}

// GroupNotes returns the notes of a group.
func (s *Store) GroupNotes(group string) (string, error) {
	if !ValidGroup(group) {
		return "", errors.New("invalid group")
	}
	b, err := s.b.Get("groups/" + group + "/notes")
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return string(b), err
}

// Verification states of a snapshot.
const (
	VerifyOK     = "ok"
	VerifyFailed = "failed"
)

// VerifyEvent is one verification of a snapshot.
type VerifyEvent struct {
	At       time.Time `json:"at"`
	State    string    `json:"state"` // VerifyOK | VerifyFailed
	Chunks   int       `json:"chunks"`
	Problems []string  `json:"problems,omitempty"`
}

// VerifyRecord is verify.json: the latest state and a bounded history, newest last.
type VerifyRecord struct {
	State   string        `json:"state"`
	At      time.Time     `json:"at"`
	History []VerifyEvent `json:"history"`
}

// ReadVerify returns the verification record, or nil if the snapshot was never verified.
func (s *Store) ReadVerify(group, snap string) (*VerifyRecord, error) {
	if err := checkIDs(group, snap); err != nil {
		return nil, err
	}
	b, err := s.b.Get(snapPrefix(group, snap) + "verify.json")
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r VerifyRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, nil // an unreadable record is treated as "never verified"
	}
	return &r, nil
}

// RecordVerify appends an event to the snapshot's verification record. A snapshot that
// has been deleted meanwhile is ignored.
func (s *Store) RecordVerify(group, snap string, ev VerifyEvent) error {
	if err := s.requireCommitted(group, snap); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	r, err := s.ReadVerify(group, snap)
	if err != nil {
		return err
	}
	if r == nil {
		r = &VerifyRecord{}
	}
	r.State, r.At = ev.State, ev.At
	r.History = append(r.History, ev)
	if len(r.History) > maxVerifyHistory {
		r.History = r.History[len(r.History)-maxVerifyHistory:]
	}
	b, _ := json.Marshal(r)
	return s.b.Put(snapPrefix(group, snap)+"verify.json", b, false)
}

// ChunkIDsOf returns the chunk ids a committed snapshot references.
func (s *Store) ChunkIDsOf(group, snap string) ([]format.ID, error) {
	ib, err := s.ReadIndex(group, snap)
	if err != nil {
		return nil, err
	}
	idx, err := format.ParseIndex(ib)
	if err != nil {
		return nil, err
	}
	return idx.ChunkIDs(), nil
}
