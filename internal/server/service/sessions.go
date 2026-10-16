package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/format"
)

// DefaultLease is how long an upload session lives without any activity. Every API call on a
// session and every explicit heartbeat pushes the lease forward, so a legitimately long backup
// (a multi-hour first upload) never expires, while a dead client's session is reclaimed one
// lease after its last sign of life.
const DefaultLease = 15 * time.Minute

// MinGCGrace is the smallest garbage-collection grace period. Sessions pin the chunks they rely
// on, so the grace period no longer has to outlive a session; it still protects uploads whose
// session was lost (a server restart) until the client retries, and synchronisation.
const MinGCGrace = 12 * time.Hour

// DefaultGCGrace is the default minimum age of an unreferenced chunk.
const DefaultGCGrace = 24 * time.Hour

type session struct {
	id      string
	repo    *Repo
	host    *Host
	req     api.SessionRequest
	ds      *datastore.Session
	mu      sync.Mutex
	expires time.Time
	// persisted is when expires_at was last written to PostgreSQL (writes are rate-limited).
	persisted time.Time
	finished  bool
	// committing: a commit is in progress. The reaper leaves such a session alone: its pins are all
	// that keeps a concurrent GC from sweeping the chunks of the snapshot being committed.
	committing bool
	checked    int64
	newChunk   int64
	bytes      int64
}

func (s *Service) lease() time.Duration {
	if s.Lease > 0 {
		return s.Lease
	}
	return DefaultLease
}

// getSession returns the caller's live session and renews its lease. A session whose lease ran
// out is reaped right here (it may already have been reclaimed by someone else's backup).
func (s *Service) getSession(id string, h *Host) (*session, error) {
	s.mu.Lock()
	ss, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok || ss.host.ID != h.ID {
		return nil, fmt.Errorf("%w: session", ErrNotFound)
	}
	ss.mu.Lock()
	if ss.finished {
		ss.mu.Unlock()
		return nil, fmt.Errorf("%w: session already finished", ErrConflict)
	}
	now := s.Now()
	if now.After(ss.expires) {
		ss.mu.Unlock()
		s.ReapSessions(context.Background())
		return nil, fmt.Errorf("%w: session lease expired (no activity for %s)", ErrConflict, s.lease())
	}
	ss.expires = now.Add(s.lease())
	persist := now.Sub(ss.persisted) > time.Minute
	if persist {
		ss.persisted = now
	}
	exp := ss.expires
	ss.mu.Unlock()
	if persist {
		// the stored expiry runs one lease AHEAD of the in-memory one, so the database never lags the
		// truth and a live session's slot cannot be reclaimed by another writer
		_, _ = s.DB.Pool().Exec(context.Background(), `UPDATE sessions SET expires_at=$2, last_activity_at=$3 WHERE id=$1::uuid AND state='open'`, uuidFromHex(id), exp.Add(s.lease()), now)
	}
	return ss, nil
}

// SessionHeartbeat renews a session's lease without doing anything else (the client sends it
// while it is busy reading and hashing a disk and has nothing to upload).
func (s *Service) SessionHeartbeat(ctx context.Context, h *Host, sid string) (time.Time, error) {
	ss, err := s.getSession(sid, h)
	if err != nil {
		return time.Time{}, err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.expires, nil
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// acquireWriter takes the single-writer slot of a group: the database refuses a second open
// session for the same (repository, group), so two writers can never race on one object even
// across processes. Expired holders are reclaimed first.
func (s *Service) acquireWriter(ctx context.Context, repoID, group, snap string, hostID *string, base string, expires time.Time) (string, error) {
	id := newID()
	insert := func() error {
		_, err := s.DB.Pool().Exec(ctx, `INSERT INTO sessions (id, repository_id, host_id, group_id, snapshot_id, expires_at, base_snapshot_id)
			VALUES ($1::uuid,$2,$3,$4,$5,$6,$7)`, uuidFromHex(id), repoID, hostID, group, snap, expires, base)
		return err
	}
	err := insert()
	if err != nil && isUniqueViolation(err) {
		// maybe the holder's lease ran out: reclaim and retry once
		s.ReapSessions(ctx)
		_, _ = s.DB.Pool().Exec(ctx, `UPDATE sessions SET state='expired' WHERE repository_id=$1 AND group_id=$2 AND state='open' AND expires_at < $3`, repoID, group, s.Now())
		err = insert()
	}
	if err != nil {
		if isUniqueViolation(err) {
			var holder string
			var since time.Time
			_ = s.DB.Pool().QueryRow(ctx, `SELECT COALESCE(h.name,''), se.created_at FROM sessions se LEFT JOIN hosts h ON h.id=se.host_id
				WHERE se.repository_id=$1 AND se.group_id=$2 AND se.state='open'`, repoID, group).Scan(&holder, &since)
			if holder == "" {
				holder = "the server (a delete or a sync pin)"
			}
			return "", fmt.Errorf("%w: another backup of this object is already running (started by %s at %s); one writer per object at a time",
				ErrConflict, holder, since.Format(time.RFC3339))
		}
		return "", err
	}
	return id, nil
}

// BeginSession opens an upload session for one snapshot of one object.
func (s *Service) BeginSession(ctx context.Context, h *Host, domain string, req api.SessionRequest) (*api.SessionResponse, error) {
	if err := requireScope(h, api.ScopeBackup); err != nil {
		return nil, err
	}
	if !datastore.ValidGroup(req.GroupID) || !format.ValidSnapshotID(req.SnapshotID) {
		return nil, bad("invalid group or snapshot id")
	}
	if req.Created.After(s.Now().Add(s.MaxClockSkew)) {
		return nil, bad("the snapshot is dated %s, more than %s in the future (fix the host's clock)", req.Created.Format(time.RFC3339), s.MaxClockSkew)
	}
	switch req.Kind {
	case "block", "filesystem", "instance":
	default:
		return nil, bad("invalid object kind %q", req.Kind)
	}
	if req.PreviousID != "" && !format.ValidSnapshotID(req.PreviousID) {
		return nil, bad("invalid previous snapshot id")
	}
	repo, err := s.RepositoryByID(ctx, h.RepoID)
	if err != nil {
		return nil, err
	}
	// fail fast in maintenance: before the host reads (and dedups) the whole object, not at commit
	if err := s.requireWritable(ctx, repo.ID); err != nil {
		return nil, fmt.Errorf("repository %s: %w (the host retries at its next run)", repo.Name, err)
	}
	if err := s.pinDomain(ctx, repo.ID, domain); err != nil {
		return nil, err
	}
	store, err := s.openStore(ctx, repo)
	if err != nil {
		return nil, err
	}
	// the object's identity and retention are pinned when first seen (see claimObject)
	if err := claimObject(ctx, s.DB.Pool(), h, req.GroupID, req.Project, req.Object, req.Kind, req.Policy, []string{}, s.Now()); err != nil {
		return nil, err
	}
	expires := s.Now().Add(s.lease())
	hid := h.ID
	sid, err := s.acquireWriter(ctx, repo.ID, req.GroupID, req.SnapshotID, &hid, req.PreviousID, expires.Add(s.lease()))
	if err != nil {
		return nil, err
	}
	ds, err := store.BeginSession(req.GroupID, req.SnapshotID)
	if err != nil {
		_, _ = s.DB.Pool().Exec(ctx, `UPDATE sessions SET state='aborted' WHERE id=$1::uuid`, uuidFromHex(sid))
		if errors.Is(err, datastore.ErrExists) {
			return nil, fmt.Errorf("%w: snapshot already exists", ErrConflict)
		}
		return nil, err
	}
	ss := &session{id: sid, repo: repo, host: h, req: req, ds: ds, expires: expires, persisted: s.Now()}
	s.mu.Lock()
	s.sessions[sid] = ss
	s.mu.Unlock()
	return &api.SessionResponse{Session: sid, Expires: expires, Lease: s.lease()}, nil
}

// SessionCommit makes the snapshot visible. The datastore commit is atomic; the
// PostgreSQL record is written afterwards and Reconcile repairs it if a crash
// happens in between. A commit refused because chunks are missing leaves the session
// open: the client uploads the missing chunks and commits again.
func (s *Service) SessionCommit(ctx context.Context, h *Host, sid string, cr api.CommitRequest) (*api.CommitResponse, error) {
	ss, err := s.getSession(sid, h)
	if err != nil {
		return nil, err
	}
	ss.mu.Lock()
	ss.committing = true
	ss.mu.Unlock()
	rc, err := ss.ds.Commit("client", s.Now())
	if err != nil {
		ss.mu.Lock()
		ss.committing = false
		ss.mu.Unlock()
		if errors.Is(err, datastore.ErrMissingChunks) {
			return nil, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		if errors.Is(err, datastore.ErrExists) {
			s.endSession(ctx, ss, "aborted")
			return nil, fmt.Errorf("%w: snapshot already exists", ErrConflict)
		}
		return nil, storeErr(err)
	}
	// The snapshot is durable. Everything after this must survive the client going away: a cancelled
	// request context must not leave the session open, the base pinned or the snapshot unrecorded.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	ss.mu.Lock()
	checked, newc, bytes := ss.checked, ss.newChunk, ss.bytes
	ss.mu.Unlock()
	s.endSession(ctx, ss, "committed")

	idx, _ := s.indexDigest(ss.repo, ss.req.GroupID, ss.req.SnapshotID)
	var objectID *string
	var oid string
	if err := s.DB.Pool().QueryRow(ctx, `SELECT id FROM storage_objects WHERE repository_id=$1 AND group_id=$2`, ss.repo.ID, ss.req.GroupID).Scan(&oid); err == nil {
		objectID = &oid
	}
	_, err = s.DB.Pool().Exec(ctx, `INSERT INTO snapshots (repository_id, object_id, group_id, snapshot_id, created_at, committed_at, policy_name, source,
		logical_bytes, dirty_bytes, read_bytes, uploaded_bytes, full_read, duration_ms, index_digest, previous_snapshot_id, chunk_count)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'client',$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT DO NOTHING`,
		ss.repo.ID, objectID, ss.req.GroupID, ss.req.SnapshotID, ss.req.Created, rc.Committed, ss.req.Policy,
		cr.LogicalBytes, cr.DirtyBytes, cr.ReadBytes, cr.UploadedBytes, cr.FullRead, cr.DurationMs, idx, ss.req.PreviousID, rc.ChunkCount)
	if err != nil {
		// The snapshot is durable in the datastore; Reconcile will record it.
		s.Log.Error("snapshot committed but PostgreSQL insert failed; run reconcile", "group", ss.req.GroupID, "snapshot", ss.req.SnapshotID, "err", err)
	}
	_, _ = s.DB.Pool().Exec(ctx, `UPDATE sessions SET chunks_checked=$2, chunks_new=$3, bytes_uploaded=$4 WHERE id=$1::uuid`,
		uuidFromHex(sid), checked, newc, bytes)
	s.healCorruptionRecords(ctx, ss.repo)
	return &api.CommitResponse{SnapshotID: ss.req.SnapshotID, Committed: rc.Committed, ChunkCount: rc.ChunkCount}, nil
}

// endSession finishes a session in memory, in the datastore (drops its pins) and in PostgreSQL
// (frees the group's writer slot).
func (s *Service) endSession(ctx context.Context, ss *session, state string) {
	ss.mu.Lock()
	ss.finished = true
	ss.mu.Unlock()
	ss.ds.Close()
	s.mu.Lock()
	delete(s.sessions, ss.id)
	s.mu.Unlock()
	_, _ = s.DB.Pool().Exec(ctx, `UPDATE sessions SET state=$2 WHERE id=$1::uuid`, uuidFromHex(ss.id), state)
}

// SessionAbort discards an upload session.
func (s *Service) SessionAbort(ctx context.Context, h *Host, sid string) error {
	ss, err := s.getSession(sid, h)
	if err != nil {
		return err
	}
	// While SessionCommit runs, an abort (the client gave up waiting) must not drop the session's pins
	// under it, as ReapSessions does not either: the commit finishes and ends the session itself.
	ss.mu.Lock()
	committing := ss.committing
	if !committing {
		ss.finished = true // closes the window to a commit that starts now (it finds the session finished)
	}
	ss.mu.Unlock()
	if committing {
		return fmt.Errorf("%w: the snapshot is being committed", ErrConflict)
	}
	s.endSession(ctx, ss, "aborted")
	return nil
}

// ReapSessions ends sessions whose lease ran out and returns how many it reclaimed.
func (s *Service) ReapSessions(ctx context.Context) int {
	now := s.Now()
	var dead []*session
	s.mu.Lock()
	for _, ss := range s.sessions {
		ss.mu.Lock()
		expired := now.After(ss.expires) && !ss.finished && !ss.committing
		ss.mu.Unlock()
		if expired {
			dead = append(dead, ss)
		}
	}
	s.mu.Unlock()
	for _, ss := range dead {
		s.endSession(ctx, ss, "expired")
	}
	s.reapSyncPins(now)
	return len(dead)
}

// ExpireStaleSessions is called at startup: sessions of a previous process are gone, so their
// rows are closed. Their chunks stay (idempotent) and the client simply retries.
func (s *Service) ExpireStaleSessions(ctx context.Context) error {
	_, err := s.DB.Pool().Exec(ctx, `UPDATE sessions SET state='expired' WHERE state='open'`)
	return err
}

// pinnedBases returns the snapshots that open sessions build on, per group. Retention and
// forget never remove them while the session is open (this is the "gc pin").
func (s *Service) pinnedBases(ctx context.Context, repoID string) (map[string]map[string]bool, error) {
	rows, err := s.DB.Pool().Query(ctx, `SELECT group_id, base_snapshot_id FROM sessions WHERE repository_id=$1 AND state='open' AND base_snapshot_id <> '' AND expires_at >= $2`, repoID, s.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var g, b string
		if err := rows.Scan(&g, &b); err != nil {
			return nil, err
		}
		if out[g] == nil {
			out[g] = map[string]bool{}
		}
		out[g][b] = true
	}
	return out, rows.Err()
}

// groupBusy reports whether a writer holds the group right now.
func (s *Service) groupBusy(ctx context.Context, repoID, group string) (bool, error) {
	var n int
	err := s.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM sessions WHERE repository_id=$1 AND group_id=$2 AND state='open' AND expires_at >= $3`, repoID, group, s.Now()).Scan(&n)
	return n > 0, err
}

var _ = pgx.ErrNoRows

// ---- synchronisation pins ------------------------------------------------------

// syncPin is one long-lived pin-only datastore session per repository and transfer: chunks received by
// server-to-server synchronisation are pinned while the transfer is active, so a sync that
// runs for many hours cannot lose chunks to a concurrent garbage collection before the
// snapshots that reference them are committed.
type syncPin struct {
	ds      *datastore.Session
	expires time.Time
	peer    string // the calling host ("" for local transfers, which are not capped)
	repoID  string
}

// MaxSyncPinsPerPeer bounds the open sync pins one sync-write host may hold per repository. A pin is
// released by a commit or after two hours of silence; without a bound a peer could open one per request.
const MaxSyncPinsPerPeer = 8

type syncPeerKey struct{}

// WithSyncPeer names the remote host a sync call comes from, so that its open pins are counted against
// MaxSyncPinsPerPeer. Local transfers (the sync manager) carry no peer and are not capped.
func WithSyncPeer(ctx context.Context, hostID string) context.Context {
	return context.WithValue(ctx, syncPeerKey{}, hostID)
}

type syncTransferKey struct{}

// WithSyncTransfer names the synchronisation transfer (one job run of one peer) the calls made with the
// returned context belong to. Each transfer has its own chunk pin; calls without one share the pin "".
func WithSyncTransfer(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, syncTransferKey{}, id)
}

func syncPinKey(ctx context.Context, repoID string) string {
	id, _ := ctx.Value(syncTransferKey{}).(string)
	return repoID + "\x00" + id
}

func (s *Service) syncPinFor(ctx context.Context, repo *Repo) (*datastore.Session, error) {
	store, err := s.openStore(context.Background(), repo)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := syncPinKey(ctx, repo.ID)
	p := s.syncPins[key]
	if p == nil {
		peer, _ := ctx.Value(syncPeerKey{}).(string)
		if peer != "" {
			open := 0
			for _, o := range s.syncPins {
				if o.peer == peer && o.repoID == repo.ID {
					open++
				}
			}
			// Refuse rather than release the oldest pin: that one may belong to a transfer still sending,
			// and releasing it would unprotect its chunks. The peer retries once an older transfer commits
			// or its pin runs out (two hours of silence).
			if open >= MaxSyncPinsPerPeer {
				return nil, fmt.Errorf("%w: this host already has %d open sync transfers into this repository; wait for one to commit (an abandoned transfer is released after two hours)", ErrTooManyRequests, open)
			}
		}
		p = &syncPin{ds: store.BeginPinSession(), peer: peer, repoID: repo.ID}
		s.syncPins[key] = p
	}
	// a sync moves one snapshot at a time and may be rate limited: keep the pin for two hours of silence
	p.expires = s.Now().Add(2 * time.Hour)
	return p.ds, nil
}

// releaseSyncPin ends the sync pin of the transfer ctx belongs to (called when a synchronised snapshot has
// committed: from then on its chunks are protected by the snapshot itself, and the pin set must not grow
// for ever). Other transfers into the same repository keep their pins.
func (s *Service) releaseSyncPin(ctx context.Context, repoID string) {
	key := syncPinKey(ctx, repoID)
	s.mu.Lock()
	p := s.syncPins[key]
	delete(s.syncPins, key)
	s.mu.Unlock()
	if p != nil {
		p.ds.Close()
	}
}

func (s *Service) reapSyncPins(now time.Time) {
	s.mu.Lock()
	var dead []*syncPin
	for id, p := range s.syncPins {
		if now.After(p.expires) {
			dead = append(dead, p)
			delete(s.syncPins, id)
		}
	}
	s.mu.Unlock()
	for _, p := range dead {
		p.ds.Close()
	}
}
