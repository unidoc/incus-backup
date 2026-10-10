// Package datastore is the repository of incus-backup-server: a content-addressed
// store of encrypted chunks plus committed snapshots (index, manifest, commit
// marker), specified in docs/format.md section 8.
//
// The Store speaks only to a Backend with OBJECT-STORE semantics (whole-object
// puts, conditional create, listing, delete). There is no rename, no
// directory and no append anywhere above the backend, so the same repository
// logic runs on a local filesystem and on any S3-compatible service, and the
// repository object model is identical on both.
//
// It never sees a key. Integrity is checked with the SHA-256 trailers of the blobs,
// references are understood through the plaintext indexes, so garbage collection,
// verification and server-to-server sync work on opaque encrypted objects.
//
// Atomic visibility: a snapshot exists if and only if its commit marker
// (`groups/<g>/<s>/commit.json`) exists. The marker is written LAST with
// create-if-absent semantics, after the chunks, index and manifest are durable. A crash
// at any earlier point leaves invisible garbage that a retry overwrites.
package datastore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
)

var (
	// ErrExists is returned when a snapshot (or a conditional put) already exists.
	ErrExists = errors.New("snapshot already exists")
	// ErrNotFound is returned for unknown chunks/snapshots/objects.
	ErrNotFound = errors.New("not found")
	// ErrInvalid marks a malformed argument (bad group or snapshot id): a caller error, not a storage fault.
	ErrInvalid = errors.New("invalid argument")
	// ErrProtected is returned when removing a protected snapshot.
	ErrProtected = errors.New("snapshot is protected")
	// ErrMissingChunks is returned by Commit when the index references chunks the store lacks.
	ErrMissingChunks = errors.New("snapshot references missing chunks")
)

// MaxBlobSize bounds uploaded blobs (a 4 MiB chunk can grow slightly when incompressible).
const MaxBlobSize = 80 << 20

// Store is an open repository.
type Store struct {
	b Backend
	// gcMu serialises the final check+delete of GC against session claims and commits:
	// Missing/PutChunk/Commit hold the read side while they look at a chunk and record
	// their claim on it, GC holds the write side for each final check+delete, so a chunk a
	// session just relied on can never be swept.
	gcMu sync.RWMutex
	// pins: chunk ids that running sessions rely on (they said "present" or uploaded
	// them), and, while a GC runs, the ids of every session that closed since it started.
	pinMu     sync.Mutex
	open      map[*Session]struct{}
	gcActive  bool
	gcRunning int32
	gcShadow  map[uint64]struct{} // liveKey of pins of sessions that ended while a GC runs
	// fault, when set, is consulted at named crash points (tests only).
	fault func(point string) error
	now   func() time.Time
	// par overrides DefaultRequestParallelism (0 = default).
	par atomic.Int64
	// presence answers "definitely stored" for Missing on remote backends (presence.go).
	presence presenceSet
	// snapLocks serialise, per snapshot directory, the writes of a commit (index, manifest, marker)
	// against the orphan cleaner and DeleteSnapshot, which decide from an earlier look at the same
	// keys: without it a cleaner that listed the debris of a failed attempt deletes the index a new
	// commit of the same snapshot id has just written.
	snapLocks [64]sync.Mutex
}

func (s *Store) snapLock(group, snap string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(group + "/" + snap))
	return &s.snapLocks[h.Sum32()%uint32(len(s.snapLocks))]
}

// Meta is datastore.json.
type Meta struct {
	Format  int       `json:"format"`
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
}

const metaKey = "datastore.json"

// InitBackend creates a new repository on b (which must not hold one yet).
func InitBackend(b Backend) (*Meta, error) {
	var x [8]byte
	_, _ = randRead(x[:])
	m := &Meta{Format: 1, ID: fmt.Sprintf("%x", x), Created: time.Now().UTC()}
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := b.Put(metaKey, append(data, '\n'), true); err != nil {
		if errors.Is(err, ErrExists) {
			return nil, fmt.Errorf("%s already holds a repository", b.Describe())
		}
		return nil, err
	}
	return m, nil
}

// OpenBackend opens an existing repository.
func OpenBackend(b Backend) (*Store, error) {
	data, err := b.Get(metaKey)
	if err != nil {
		return nil, fmt.Errorf("%s is not a repository: %w", b.Describe(), err)
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil || m.Format != 1 {
		return nil, fmt.Errorf("unsupported repository format in %s", b.Describe())
	}
	s := &Store{b: b, now: time.Now, open: map[*Session]struct{}{}}
	_, remote := Unwrap(b).(*S3Backend)
	s.presence = presenceSet{on: remote, max: PresenceMaxEntries, refresh: PresenceRefresh, now: time.Now}
	return s, nil
}

// Init creates a filesystem repository at root (which must be empty or absent).
func Init(root string) (*Meta, error) {
	if err := EnsureEmptyDir(root); err != nil {
		return nil, err
	}
	b, err := NewFSBackend(root)
	if err != nil {
		return nil, err
	}
	return InitBackend(b)
}

// Open opens a filesystem repository.
func Open(root string) (*Store, error) {
	b, err := NewFSBackend(root)
	if err != nil {
		return nil, err
	}
	return OpenBackend(b)
}

// Backend returns the storage the repository lives on.
func (s *Store) Backend() Backend { return s.b }

// Root returns the root directory of a filesystem repository ("" otherwise).
func (s *Store) Root() string {
	if fb, ok := Unwrap(s.b).(*FSBackend); ok {
		return fb.Root()
	}
	return ""
}

// Meta returns datastore.json.
func (s *Store) Meta() (*Meta, error) {
	data, err := s.b.Get(metaKey)
	if err != nil {
		return nil, err
	}
	var m Meta
	return &m, json.Unmarshal(data, &m)
}

// SetFault installs a crash-point hook (tests only).
func (s *Store) SetFault(f func(point string) error) { s.fault = f }

func (s *Store) at(point string) error {
	if s.fault == nil {
		return nil
	}
	return s.fault(point)
}

// ---- keys --------------------------------------------------------------------

// ValidGroup reports whether g is a well-formed group id (32 lowercase hex).
func ValidGroup(g string) bool {
	if len(g) != 32 {
		return false
	}
	for _, c := range []byte(g) {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func chunkKey(id format.ID) string {
	h := id.String()
	return "chunks/" + h[:2] + "/" + h[2:4] + "/" + h
}

func snapPrefix(group, snap string) string { return "groups/" + group + "/" + snap + "/" }

func checkIDs(group, snap string) error {
	if !ValidGroup(group) || !format.ValidSnapshotID(snap) {
		return fmt.Errorf("%w: invalid group or snapshot id", ErrInvalid)
	}
	return nil
}

// ---- chunks ------------------------------------------------------------------

// Missing returns the ids the store does not have, without downloading anything and
// without writing anything (no touch, no COPY). It claims nothing: only a Session's
// Missing pins the chunks it finds present.
func (s *Store) Missing(ids []format.ID) ([]format.ID, error) {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	return s.missingLocked(ids)
}

func (s *Store) missingLocked(ids []format.ID) ([]format.ID, error) {
	s.refreshPresence()
	uniq := uniqueIDs(ids)
	present, err := s.statChunks(uniq, true)
	if err != nil {
		return nil, err
	}
	have := make(map[format.ID]bool, len(uniq))
	for i, id := range uniq {
		have[id] = present[i]
	}
	var out []format.ID
	for _, id := range ids {
		if !id.IsZero() && !have[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// PutChunk stores a blob under id. It verifies the trailer (keyless). Putting an
// existing chunk is a no-op. created reports a new object. A chunk that was
// quarantined as corrupt does not exist any more, so putting it again heals it.
func (s *Store) PutChunk(id format.ID, blob []byte) (created bool, err error) {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	return s.putChunkLocked(id, blob)
}

func (s *Store) putChunkLocked(id format.ID, blob []byte) (created bool, err error) {
	if id.IsZero() {
		return false, fmt.Errorf("%w: zero chunk id has no blob", ErrInvalid)
	}
	if len(blob) > MaxBlobSize {
		return false, fmt.Errorf("%w: blob too large", ErrInvalid)
	}
	if err := format.VerifyBlob(blob); err != nil {
		return false, fmt.Errorf("%w: rejecting blob %s: %v", ErrInvalid, id, err)
	}
	key := chunkKey(id)
	ep := s.presence.epoch()
	// On a local filesystem a stat is cheap and saves writing and syncing a temp file for a chunk
	// that is already there. On an object store it is a round trip per uploaded chunk that the
	// conditional PUT below makes redundant (If-None-Match: * answers "exists" by itself).
	if statBeforePut(s.b) {
		if _, err := s.b.Stat(key); err == nil {
			return false, nil
		}
	}
	if err := s.at("chunk:before-write"); err != nil {
		return false, err
	}
	switch err := s.b.Put(key, blob, true); {
	case err == nil:
		s.presence.observed(id, ep)
		return true, nil
	case errors.Is(err, ErrExists): // a concurrent upload of the same chunk: identical content
		s.presence.observed(id, ep)
		return false, nil
	default:
		return false, err
	}
}

// GetChunk returns the stored blob.
func (s *Store) GetChunk(id format.ID) ([]byte, error) { return s.b.Get(chunkKey(id)) }

// ---- snapshots -----------------------------------------------------------------

// Receipt is commit.json, written by the server as the commit marker.
type Receipt struct {
	Committed      time.Time `json:"committed"`
	ChunkCount     int       `json:"chunk_count"`
	IndexBytes     int       `json:"index_bytes"`
	IndexDigest    string    `json:"index_digest"`
	ManifestDigest string    `json:"manifest_digest"`
	Source         string    `json:"source"` // "client" or "sync"
}

// Session is an in-progress snapshot upload. Nothing is written to the backend
// for the snapshot until Commit; chunks uploaded earlier are independent objects.
//
// A session PINS every chunk it relies on: the ones the backend said were present
// (Missing) and the ones it uploaded. Garbage collection never sweeps a pinned chunk,
// however old, until the session has ended AND the running GC (if any) has finished.
// This is what lets a legitimately long upload outlive the GC grace period.
type Session struct {
	s        *Store
	group    string
	snapshot string
	index    []byte
	manifest []byte
	mu       sync.Mutex
	// known holds the pins by liveKey (8 bytes of the id): a prefix shared with another chunk can
	// only pin that one too (kept longer), never unpin anything.
	known  map[uint64]struct{}
	closed bool
}

// BeginSession starts building snapshot for group.
func (s *Store) BeginSession(group, snapshot string) (*Session, error) {
	if err := checkIDs(group, snapshot); err != nil {
		return nil, err
	}
	if _, err := s.b.Stat(snapPrefix(group, snapshot) + "commit.json"); err == nil {
		return nil, ErrExists
	}
	x := &Session{s: s, group: group, snapshot: snapshot, known: map[uint64]struct{}{}}
	s.pinMu.Lock()
	s.open[x] = struct{}{}
	s.pinMu.Unlock()
	return x, nil
}

// BeginPinSession returns a session that only pins chunks (Missing and PutChunk): used by
// server-to-server synchronisation to protect chunks received before their snapshot commits.
// It cannot Commit.
func (s *Store) BeginPinSession() *Session {
	x := &Session{s: s, known: map[uint64]struct{}{}}
	s.pinMu.Lock()
	s.open[x] = struct{}{}
	s.pinMu.Unlock()
	return x
}

// Close ends the session (after Commit or instead of it). Idempotent. Its pins move to the
// shadow set while a GC runs, so a GC that is in the middle of its sweep keeps honouring them.
func (x *Session) Close() {
	x.s.pinMu.Lock()
	defer x.s.pinMu.Unlock()
	x.mu.Lock()
	if x.closed {
		x.mu.Unlock()
		return
	}
	x.closed = true
	if x.s.gcActive {
		for k := range x.known {
			x.s.gcShadow[k] = struct{}{}
		}
	}
	x.mu.Unlock()
	delete(x.s.open, x)
}

// ErrSessionClosed is returned by a session used after Close.
var ErrSessionClosed = errors.New("session is closed")

// Missing is Store.Missing, pinning the ids that are present.
func (x *Session) Missing(ids []format.ID) ([]format.ID, error) {
	x.s.gcMu.RLock()
	defer x.s.gcMu.RUnlock()
	miss, err := x.s.missingLocked(ids)
	if err != nil {
		return nil, err
	}
	gone := map[format.ID]bool{}
	for _, m := range miss {
		gone[m] = true
	}
	x.mu.Lock()
	if x.closed {
		x.mu.Unlock()
		return nil, ErrSessionClosed
	}
	for _, id := range ids {
		if !id.IsZero() && !gone[id] {
			x.known[liveKey(id)] = struct{}{}
		}
	}
	x.mu.Unlock()
	return miss, nil
}

// PutChunk is Store.PutChunk, pinning the chunk.
func (x *Session) PutChunk(id format.ID, blob []byte) (bool, error) {
	x.s.gcMu.RLock()
	defer x.s.gcMu.RUnlock()
	created, err := x.s.putChunkLocked(id, blob)
	if err == nil {
		x.mu.Lock()
		if x.closed {
			x.mu.Unlock()
			return created, ErrSessionClosed
		}
		x.known[liveKey(id)] = struct{}{}
		x.mu.Unlock()
	}
	return created, err
}

// PutIndex checks and keeps the index.
func (x *Session) PutIndex(b []byte) error {
	if _, err := format.ParseIndex(b); err != nil {
		return err
	}
	x.index = b
	return nil
}

// PutManifest checks the encrypted manifest's trailer and keeps it.
func (x *Session) PutManifest(b []byte) error {
	if len(b) < 64 || len(b) > 16<<20 || string(b[:4]) != "IBMF" {
		return errors.New("not a manifest")
	}
	if err := verifyTrailer(b); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	x.manifest = b
	return nil
}

// Abort discards the session (nothing was written for the snapshot).
func (x *Session) Abort() { x.Close() }

// Commit makes the snapshot visible. Every referenced chunk must exist. Order: index,
// manifest, then the commit marker, last and create-if-absent. at is the commit time
// recorded in the receipt (zero = now): for a synchronised snapshot it is the SOURCE's
// commit time. The session is closed after a successful commit (or ErrExists).
//
// Every chunk of the index is pinned for the rest of a running GC: chunks the session
// did not claim itself were only stat'ed here, and a GC that marked before this commit
// would otherwise not know the new snapshot references them.
func (x *Session) Commit(source string, at time.Time) (*Receipt, error) {
	rc, err := x.commit(source, at)
	// A commit refused for missing chunks leaves the session open: the caller uploads them and retries.
	if err == nil || rc != nil || errors.Is(err, ErrExists) {
		x.Close()
	}
	return rc, err
}

func (x *Session) commit(source string, at time.Time) (*Receipt, error) {
	if x.group == "" {
		return nil, errors.New("a pin-only session cannot commit")
	}
	if x.index == nil || x.manifest == nil {
		return nil, fmt.Errorf("%w: index and manifest are required", ErrInvalid)
	}
	idx, err := format.ParseIndex(x.index)
	if err != nil {
		return nil, err
	}
	ids := idx.ChunkIDs()
	// 1. PIN every chunk of the index before looking at any of them. From here until the session
	// closes, no GC can sweep them (a GC that already checked is caught by step 2: the chunk is
	// gone and the commit is refused). This replaces holding the GC lock across the slow checks.
	x.s.gcMu.RLock()
	x.mu.Lock()
	if x.closed {
		x.mu.Unlock()
		x.s.gcMu.RUnlock()
		return nil, ErrSessionClosed
	}
	for _, id := range ids {
		x.known[liveKey(id)] = struct{}{}
	}
	x.mu.Unlock()
	x.s.gcMu.RUnlock()
	// 2. every chunk must exist. This asks the BACKEND about every distinct chunk, in parallel and
	// without holding a lock; it never trusts the presence set (an object deleted behind this
	// process's back must be found here, not made part of a visible snapshot). Ids found missing
	// are evicted from the presence set, so the client's retry of Missing asks about them again.
	uniq := uniqueIDs(ids)
	present, err := x.s.statChunks(uniq, false)
	if err != nil {
		return nil, err
	}
	var missing []format.ID
	for i, id := range uniq {
		if !present[i] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %d (first %s)", ErrMissingChunks, len(missing), missing[0])
	}
	prefix := snapPrefix(x.group, x.snapshot)
	l := x.s.snapLock(x.group, x.snapshot)
	l.Lock()
	defer l.Unlock()
	if _, err := x.s.b.Stat(prefix + "commit.json"); err == nil {
		return nil, ErrExists
	}
	if err := x.s.b.Put(prefix+"index", x.index, false); err != nil {
		return nil, err
	}
	if err := x.s.b.Put(prefix+"manifest", x.manifest, false); err != nil {
		return nil, err
	}
	if err := x.s.at("commit:before-marker"); err != nil {
		return nil, err
	}
	if at.IsZero() {
		at = x.s.now()
	}
	rc := &Receipt{Committed: at.UTC(), ChunkCount: len(ids), IndexBytes: len(x.index), IndexDigest: format.IndexDigest(x.index),
		ManifestDigest: format.IndexDigest(x.manifest), Source: source}
	rb, _ := json.Marshal(rc)
	if err := x.s.b.Put(prefix+"commit.json", rb, true); err != nil {
		if errors.Is(err, ErrExists) {
			// A retried PUT whose first attempt succeeded (the response was lost) answers "exists":
			// if the marker is byte for byte ours, the commit succeeded.
			if cur, gerr := x.s.b.Get(prefix + "commit.json"); gerr == nil && bytes.Equal(cur, rb) {
				return rc, nil
			}
			return nil, ErrExists
		}
		return nil, err
	}
	if err := x.s.at("commit:after-marker"); err != nil {
		return rc, err
	}
	return rc, nil
}

func verifyTrailer(b []byte) error {
	if len(b) < 32 {
		return errors.New("too short")
	}
	return trailerOK(b)
}

// SnapshotInfo describes a committed snapshot.
type SnapshotInfo struct {
	Group     string
	Snapshot  string
	Receipt   Receipt
	Protected bool // a `protected` marker exists: retention and forget refuse to remove it
	HasNotes  bool
	HasVerify bool // a verify.json exists
}

// ListGroups returns every group id.
func (s *Store) ListGroups() ([]string, error) {
	names, err := s.b.ListPrefixes("groups/")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		if ValidGroup(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

type snapFlags struct{ marker, protected, notes, verify bool }

// scanGroup lists the objects of a group once and returns what each snapshot directory holds.
func (s *Store) scanGroup(group string) (map[string]*snapFlags, error) {
	if !ValidGroup(group) {
		return nil, errors.New("invalid group")
	}
	seen := map[string]*snapFlags{}
	err := s.b.List("groups/"+group+"/", func(key string, _ Info) error {
		rest := strings.TrimPrefix(key, "groups/"+group+"/")
		i := strings.Index(rest, "/")
		if i <= 0 || !format.ValidSnapshotID(rest[:i]) {
			return nil
		}
		f := seen[rest[:i]]
		if f == nil {
			f = &snapFlags{}
			seen[rest[:i]] = f
		}
		switch rest[i+1:] {
		case "commit.json":
			f.marker = true
		case "protected":
			f.protected = true
		case "notes":
			f.notes = true
		case "verify.json":
			f.verify = true
		}
		return nil
	})
	return seen, err
}

// committedSnapshots returns the ids of the snapshots of a group that have a commit marker,
// sorted, from one listing (no marker is read).
func (s *Store) committedSnapshots(group string) ([]string, error) {
	seen, err := s.scanGroup(group)
	if err != nil {
		return nil, err
	}
	var markers []string
	for sn, f := range seen {
		if f.marker {
			markers = append(markers, sn)
		}
	}
	sort.Strings(markers)
	return markers, nil
}

// ListSnapshots returns the COMMITTED snapshots of a group, oldest first. Uploads
// whose commit marker is missing are not snapshots and are not listed. The markers are
// read in parallel.
func (s *Store) ListSnapshots(group string) ([]SnapshotInfo, error) {
	seen, err := s.scanGroup(group)
	if err != nil {
		return nil, err
	}
	var markers []string
	for sn, f := range seen {
		if f.marker {
			markers = append(markers, sn)
		}
	}
	sort.Strings(markers)
	infos := make([]SnapshotInfo, len(markers))
	gone := make([]bool, len(markers))
	_ = forEach(s.parallelism(), len(markers), func(i int) error {
		sn := markers[i]
		f := seen[sn]
		si := SnapshotInfo{Group: group, Snapshot: sn, Protected: f.protected, HasNotes: f.notes, HasVerify: f.verify}
		if b, err := s.b.Get(snapPrefix(group, sn) + "commit.json"); err == nil {
			_ = json.Unmarshal(b, &si.Receipt)
		} else if errors.Is(err, ErrNotFound) {
			gone[i] = true // deleted while listing
		}
		infos[i] = si
		return nil
	})
	out := make([]SnapshotInfo, 0, len(markers))
	for i := range infos {
		if !gone[i] {
			out = append(out, infos[i])
		}
	}
	return out, nil
}

// ReadIndex returns the raw index file of a committed snapshot.
func (s *Store) ReadIndex(group, snap string) ([]byte, error) {
	return s.readSnap(group, snap, "index")
}

// ReadManifest returns the raw manifest blob of a committed snapshot.
func (s *Store) ReadManifest(group, snap string) ([]byte, error) {
	return s.readSnap(group, snap, "manifest")
}

func (s *Store) readSnap(group, snap, file string) ([]byte, error) {
	if err := checkIDs(group, snap); err != nil {
		return nil, err
	}
	// a snapshot that is not committed does not exist, whatever else is lying around
	if _, err := s.b.Stat(snapPrefix(group, snap) + "commit.json"); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.b.Get(snapPrefix(group, snap) + file)
}

// DeleteSnapshot removes a committed snapshot (its chunks are left for GC). The
// commit marker goes first, so the snapshot disappears atomically; a crash after
// that leaves only garbage. A protected snapshot is refused (ErrProtected): unprotect
// it first.
func (s *Store) DeleteSnapshot(group, snap string) error {
	if err := checkIDs(group, snap); err != nil {
		return err
	}
	prefix := snapPrefix(group, snap)
	l := s.snapLock(group, snap)
	l.Lock()
	defer l.Unlock()
	marker, err := s.b.Get(prefix + "commit.json")
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if _, err := s.b.Stat(prefix + "protected"); err == nil {
		return ErrProtected
	}
	if err := s.b.Delete(prefix + "commit.json"); err != nil {
		return err
	}
	// Protection set between the check and the delete wins: put the marker back.
	if _, err := s.b.Stat(prefix + "protected"); err == nil {
		if perr := s.b.Put(prefix+"commit.json", marker, true); perr != nil && !errors.Is(perr, ErrExists) {
			return perr
		}
		return ErrProtected
	}
	for _, f := range []string{"index", "manifest", "notes", "verify.json", "protected"} {
		if err := s.b.Delete(prefix + f); err != nil {
			return err
		}
	}
	return nil
}

// SnapshotExists reports whether a committed snapshot is in the repository.
func (s *Store) SnapshotExists(group, snap string) bool {
	if checkIDs(group, snap) != nil {
		return false
	}
	_, err := s.b.Stat(snapPrefix(group, snap) + "commit.json")
	return err == nil
}

// CleanTmp removes backend crash leftovers older than olderThan (filesystem
// temp files; a no-op for object stores).
func (s *Store) CleanTmp(olderThan time.Duration) (int, error) {
	if c, ok := s.b.(interface {
		CleanTmp(time.Duration) (int, error)
	}); ok {
		return c.CleanTmp(olderThan)
	}
	return 0, nil
}

// CleanOrphans removes the objects of uploads that never got their commit marker (index, manifest,
// and stray metadata of a snapshot that was removed half-way) once they are older than olderThan.
// Only keys of the form groups/<group>/<snapshot id>/<file> are considered: group notes and other
// group-level objects are never touched.
func (s *Store) CleanOrphans(olderThan time.Duration) (int, error) {
	cutoff := s.now().Add(-olderThan)
	committed := map[string]bool{}
	type obj struct {
		dir, key string
		mod      time.Time
	}
	var all []obj
	err := s.b.List("groups/", func(key string, info Info) error {
		parts := strings.Split(key, "/")
		if len(parts) != 4 || !ValidGroup(parts[1]) || !format.ValidSnapshotID(parts[2]) {
			return nil
		}
		dir := strings.Join(parts[:3], "/") + "/"
		if parts[3] == "commit.json" {
			committed[dir] = true
		} else {
			all = append(all, obj{dir, key, info.ModTime})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	// The listing is old by the time a delete runs: each directory is re-checked (marker, then the
	// object's age) under its snapshot lock, which a commit of the same snapshot holds while it writes.
	byDir := map[string][]obj{}
	var dirs []string
	for _, o := range all {
		if committed[o.dir] || o.mod.After(cutoff) {
			continue
		}
		if byDir[o.dir] == nil {
			dirs = append(dirs, o.dir)
		}
		byDir[o.dir] = append(byDir[o.dir], o)
	}
	n := 0
	for _, dir := range dirs {
		parts := strings.Split(dir, "/")
		l := s.snapLock(parts[1], parts[2])
		l.Lock()
		if _, err := s.b.Stat(dir + "commit.json"); err == nil || !errors.Is(err, ErrNotFound) {
			l.Unlock()
			continue // committed meanwhile, or unknown: keep everything
		}
		for _, o := range byDir[dir] {
			if info, err := s.b.Stat(o.key); err != nil || info.ModTime.After(cutoff) {
				continue // gone, rewritten since the listing, or unknown
			}
			if s.b.Delete(o.key) == nil {
				n++
			}
		}
		l.Unlock()
	}
	return n, nil
}
