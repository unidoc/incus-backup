package datastore

import (
	"errors"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
)

// Corrupt chunks are QUARANTINED, not merely reported (cf. PBS renaming a bad chunk to
// `.bad`): the blob is moved to `quarantine/<id>` and removed from `chunks/`, so from that
// moment it no longer answers "present" to a dedup check, and the next backup or sync that
// holds the same plaintext uploads a fresh healthy copy. The bad blob is kept for
// inspection until the chunk is healthy again and PurgeQuarantine runs.

func quarantineKey(id format.ID) string { return "quarantine/" + id.String() }

// Quarantine moves a chunk blob out of the live chunk namespace. It is idempotent.
func (s *Store) Quarantine(id format.ID) error {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	key := chunkKey(id)
	blob, err := s.b.Get(key)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	// Re-check under the lock: a concurrent heal may have replaced it with a good blob.
	if format.VerifyBlob(blob) == nil {
		return nil
	}
	if err := s.b.Put(quarantineKey(id), blob, false); err != nil {
		return err
	}
	// Quarantine does not exclude concurrent sessions (it holds the read side only): forget the
	// chunk before the delete, and again after it, so the presence set can never keep it.
	s.presence.remove(id)
	err = s.b.Delete(key)
	s.presence.remove(id)
	return err
}

// QuarantinedChunk is an entry of the quarantine area.
type QuarantinedChunk struct {
	ID     format.ID
	Size   int64
	Since  time.Time
	Healed bool // a healthy copy is in chunks/ again
}

// ListQuarantine returns the quarantined chunks.
func (s *Store) ListQuarantine() ([]QuarantinedChunk, error) {
	var out []QuarantinedChunk
	err := s.b.List("quarantine/", func(key string, info Info) error {
		id, perr := format.ParseID(strings.TrimPrefix(key, "quarantine/"))
		if perr != nil {
			return nil
		}
		out = append(out, QuarantinedChunk{ID: id, Size: info.Size, Since: info.ModTime, Healed: s.ChunkHealthy(id)})
		return nil
	})
	return out, err
}

// PurgeQuarantine deletes quarantined blobs whose chunk is healthy again and that were
// quarantined more than olderThan ago. It returns how many it removed.
func (s *Store) PurgeQuarantine(olderThan time.Duration) (int, error) {
	q, err := s.ListQuarantine()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range q {
		if c.Healed && s.now().Sub(c.Since) > olderThan {
			if s.b.Delete(quarantineKey(c.ID)) == nil {
				n++
			}
		}
	}
	return n, nil
}

// HasChunk reports whether a healthy-named chunk object exists.
func (s *Store) HasChunk(id format.ID) bool {
	_, err := s.b.Stat(chunkKey(id))
	return err == nil
}

// ChunkHealthy reports whether the chunk object exists AND its blob passes the keyless integrity check.
func (s *Store) ChunkHealthy(id format.ID) bool {
	b, err := s.b.Get(chunkKey(id))
	return err == nil && format.VerifyBlob(b) == nil
}
