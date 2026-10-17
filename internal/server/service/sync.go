package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/format"
)

// This file holds the repository-level primitives used by server-to-server
// synchronisation. They operate on a Repo (not a Host), so a server can use them
// on its own repository directly, while the HTTP layer first checks the caller's
// sync scope.

// ListAllSnapshots lists every committed snapshot of the repository (oldest
// first per group) with operational metadata, optionally restricted to groups.
func (s *Service) ListAllSnapshots(ctx context.Context, repo *Repo, groups []string) ([]api.SnapshotRef, error) {
	store, err := s.Store(repo)
	if err != nil {
		return nil, err
	}
	all, err := store.ListGroups()
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, g := range groups {
		want[g] = true
	}
	// The PostgreSQL rows of every group come from one query (not one per group); the datastore
	// listing of each group stays authoritative for which snapshots exist.
	meta, err := s.snapshotMeta(ctx, repo, "")
	if err != nil {
		return nil, err
	}
	var out []api.SnapshotRef
	for _, g := range all {
		if len(want) > 0 && !want[g] {
			continue
		}
		infos, err := store.ListSnapshots(g)
		if err != nil {
			return nil, err
		}
		l, rowless := assembleSnapshotList(store, g, infos, meta[g])
		if rowless {
			// committed after the bulk read (or never recorded): read this group's rows again, after
			// its listing, exactly as a single-group listing does
			if l, err = s.snapshotList(ctx, repo, g); err != nil {
				return nil, err
			}
		}
		out = append(out, l.Snapshots...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].GroupID != out[j].GroupID {
			return out[i].GroupID < out[j].GroupID
		}
		return out[i].SnapshotID < out[j].SnapshotID
	})
	return out, nil
}

// SyncMissing returns the chunk ids the repository lacks. Chunks that are present are pinned
// for the duration of the sync activity.
func (s *Service) SyncMissing(ctx context.Context, repo *Repo, ids []string) ([]string, error) {
	parsed, err := parseIDs(ids)
	if err != nil {
		return nil, err
	}
	pin, err := s.syncPinFor(ctx, repo)
	if err != nil {
		return nil, err
	}
	miss, err := pin.Missing(parsed)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(miss))
	for i, m := range miss {
		out[i] = m.String()
	}
	return out, nil
}

// SyncPutChunk stores a chunk blob received by synchronisation.
func (s *Service) SyncPutChunk(ctx context.Context, repo *Repo, idHex string, blob []byte) error {
	id, err := format.ParseID(idHex)
	if err != nil {
		return bad("%v", err)
	}
	pin, err := s.syncPinFor(ctx, repo)
	if err != nil {
		return err
	}
	if _, err := pin.PutChunk(id, blob); err != nil {
		if errors.Is(err, datastore.ErrInvalid) {
			return bad("%v", err)
		}
		// maintenance mode, a storage fault or a closed pin is not the sender's mistake: it must stay
		// retryable (as in SessionPutChunk)
		return err
	}
	return nil
}

// SyncPutSnapshot commits a synchronised snapshot. It is idempotent: an
// identical snapshot that already exists is a success, a different one with the
// same id is a conflict. Nothing becomes visible unless every chunk is present.
func (s *Service) SyncPutSnapshot(ctx context.Context, repo *Repo, domain string, put api.SyncSnapshotPut) error {
	// Whatever the outcome, this transfer's pin has done its job for this snapshot: committed (the snapshot
	// protects its chunks), already present, or refused (the run's next Missing re-pins what it still needs).
	// Without this, every run that ends on a repair or a refusal holds one of MaxSyncPinsPerPeer for 2 h.
	defer s.releaseSyncPin(ctx, repo.ID)
	m := put.Meta
	if !datastore.ValidGroup(m.GroupID) || !format.ValidSnapshotID(m.SnapshotID) {
		return bad("invalid group or snapshot id")
	}
	// The id of a snapshot carries the time it was taken (UTC, to the second): a retention time more than
	// a day before it cannot be true (a backup commits after it starts), so it is refused. This keeps an
	// honest-but-buggy peer consistent; it does not stop a peer that forges the id as well (docs/design.md 3.3,
	// "What a sync-write peer is trusted with").
	if made, err := format.SnapshotIDTime(m.SnapshotID); err != nil {
		return bad("%v", err)
	} else if !m.Committed.IsZero() && m.Committed.Before(made.Add(-24*time.Hour)) {
		return bad("the commit time %s is more than a day before the snapshot id's own time %s", m.Committed.UTC().Format(time.RFC3339), made.Format(time.RFC3339))
	}
	if err := s.pinDomain(ctx, repo.ID, domain); err != nil {
		return err
	}
	if _, err := format.ParseIndex(put.Index); err != nil {
		return bad("%v", err)
	}
	store, err := s.Store(repo)
	if err != nil {
		return err
	}
	if cur, err := store.ReadIndex(m.GroupID, m.SnapshotID); err == nil {
		if format.IndexDigest(cur) == format.IndexDigest(put.Index) {
			s.healCorruptionRecords(ctx, repo) // a repair sync just re-uploaded quarantined chunks
			return nil
		}
		return fmt.Errorf("%w: a different snapshot with this id already exists", ErrConflict)
	}
	sid, err := s.acquireWriter(ctx, repo.ID, m.GroupID, m.SnapshotID, nil, "", s.Now().Add(2*time.Hour)) // a sync commit on S3 can be slow; the row is ended explicitly
	if err != nil {
		return err
	}
	finish := func(state string) {
		_, _ = s.DB.Pool().Exec(ctx, `UPDATE sessions SET state=$2 WHERE id=$1::uuid`, uuidFromHex(sid), state)
	}
	ds, err := store.BeginSession(m.GroupID, m.SnapshotID)
	if err != nil {
		finish("aborted")
		if errors.Is(err, datastore.ErrExists) {
			return nil
		}
		return err
	}
	if err := ds.PutIndex(put.Index); err != nil {
		ds.Abort()
		finish("aborted")
		return bad("%v", err)
	}
	if err := ds.PutManifest(put.Manifest); err != nil {
		ds.Abort()
		finish("aborted")
		return bad("%v", err)
	}
	// Retention buckets by the time the SOURCE committed (never by when we received it,
	// or a 30-day sync would collapse into one day), clamped so a lying source cannot
	// date snapshots into the future.
	at := m.Committed
	if at.IsZero() || at.After(s.Now()) {
		at = s.Now()
	}
	rc, err := ds.Commit("sync", at)
	if err != nil {
		ds.Abort()
		finish("aborted")
		if errors.Is(err, datastore.ErrMissingChunks) {
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return err
	}
	finish("committed")
	var oid string
	if err := s.DB.Pool().QueryRow(ctx, `INSERT INTO storage_objects (repository_id, host_name, project, name, kind, group_id, policy_name, retention_policy, first_seen_at, last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,$8,$8)
		ON CONFLICT (repository_id, group_id) DO UPDATE SET last_seen_at=EXCLUDED.last_seen_at RETURNING id`,
		repo.ID, m.Host, m.Project, m.Object, kindOrBlock(m.Kind), m.GroupID, m.Policy, s.Now()).Scan(&oid); err != nil {
		s.Log.Error("synced snapshot committed but object upsert failed; run reconcile", "err", err)
		return nil
	}
	_, err = s.DB.Pool().Exec(ctx, `INSERT INTO snapshots (repository_id, object_id, group_id, snapshot_id, created_at, committed_at, policy_name, source,
		logical_bytes, index_digest, chunk_count) VALUES ($1,$2,$3,$4,$5,$6,$7,'sync',$8,$9,$10) ON CONFLICT DO NOTHING`,
		repo.ID, oid, m.GroupID, m.SnapshotID, m.Created, rc.Committed, m.Policy, m.Logical, format.IndexDigest(put.Index), rc.ChunkCount)
	if err != nil {
		s.Log.Error("synced snapshot committed but PostgreSQL insert failed; run reconcile", "err", err)
	}
	return nil
}

func kindOrBlock(k string) string {
	switch k {
	case "block", "filesystem", "instance":
		return k
	}
	return "block"
}

// RepoIndex, RepoManifest and RepoChunk read repository objects for sync.

// RepoIndex reads a snapshot index.
func (s *Service) RepoIndex(ctx context.Context, repo *Repo, group, snap string) ([]byte, error) {
	st, err := s.Store(repo)
	if err != nil {
		return nil, err
	}
	b, err := st.ReadIndex(group, snap)
	if errors.Is(err, datastore.ErrNotFound) {
		return nil, fmt.Errorf("%w: snapshot", ErrNotFound)
	}
	return b, err
}

// RepoManifest reads a snapshot manifest.
func (s *Service) RepoManifest(ctx context.Context, repo *Repo, group, snap string) ([]byte, error) {
	st, err := s.Store(repo)
	if err != nil {
		return nil, err
	}
	b, err := st.ReadManifest(group, snap)
	if errors.Is(err, datastore.ErrNotFound) {
		return nil, fmt.Errorf("%w: snapshot", ErrNotFound)
	}
	return b, err
}

// RepoChunk reads a chunk blob.
func (s *Service) RepoChunk(ctx context.Context, repo *Repo, idHex string) ([]byte, error) {
	id, err := format.ParseID(idHex)
	if err != nil {
		return nil, bad("%v", err)
	}
	st, err := s.Store(repo)
	if err != nil {
		return nil, err
	}
	b, err := st.GetChunk(id)
	if errors.Is(err, datastore.ErrNotFound) {
		return nil, fmt.Errorf("%w: chunk", ErrNotFound)
	}
	return b, err
}

// SyncHostRepo authorises a sync caller and returns its repository.
func (s *Service) SyncHostRepo(ctx context.Context, h *Host, scopes ...string) (*Repo, error) {
	if err := requireScope(h, scopes...); err != nil {
		return nil, err
	}
	return s.RepositoryByID(ctx, h.RepoID)
}
