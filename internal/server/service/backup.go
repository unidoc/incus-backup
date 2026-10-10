package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/format"
)

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func requireScope(h *Host, scopes ...string) error {
	for _, sc := range scopes {
		if h.Scope == sc {
			return nil
		}
	}
	return fmt.Errorf("%w: scope %q cannot do this", ErrForbidden, h.Scope)
}

func uuidFromHex(h string) string {
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func parseIDs(in []string) ([]format.ID, error) {
	out := make([]format.ID, 0, len(in))
	for _, s := range in {
		id, err := format.ParseID(s)
		if err != nil {
			return nil, bad("%v", err)
		}
		out = append(out, id)
	}
	return out, nil
}

// SessionMissing reports which chunk ids the repository lacks. Chunks that are present
// are pinned for the session, so garbage collection cannot remove them before commit.
func (s *Service) SessionMissing(ctx context.Context, h *Host, sid string, ids []string) ([]string, error) {
	ss, err := s.getSession(sid, h)
	if err != nil {
		return nil, err
	}
	parsed, err := parseIDs(ids)
	if err != nil {
		return nil, err
	}
	miss, err := ss.ds.Missing(parsed)
	if err != nil {
		return nil, err
	}
	ss.mu.Lock()
	ss.checked += int64(len(parsed))
	ss.mu.Unlock()
	out := make([]string, len(miss))
	for i, m := range miss {
		out[i] = m.String()
	}
	return out, nil
}

// SessionPutChunk stores one chunk blob.
func (s *Service) SessionPutChunk(ctx context.Context, h *Host, sid, idHex string, blob []byte) error {
	ss, err := s.getSession(sid, h)
	if err != nil {
		return err
	}
	id, err := format.ParseID(idHex)
	if err != nil {
		return bad("%v", err)
	}
	created, err := ss.ds.PutChunk(id, blob)
	if errors.Is(err, datastore.ErrInvalid) {
		return bad("%v", err)
	}
	if err != nil {
		return err // a storage fault or maintenance mode is not the client's mistake: it must stay retryable (5xx)
	}
	ss.mu.Lock()
	if created {
		ss.newChunk++
		ss.bytes += int64(len(blob))
	}
	ss.mu.Unlock()
	return nil
}

// SessionPutIndex stores the index of the snapshot being uploaded.
func (s *Service) SessionPutIndex(ctx context.Context, h *Host, sid string, b []byte) error {
	ss, err := s.getSession(sid, h)
	if err != nil {
		return err
	}
	if err := ss.ds.PutIndex(b); err != nil {
		return bad("%v", err)
	}
	return nil
}

// SessionPutManifest stores the encrypted manifest.
func (s *Service) SessionPutManifest(ctx context.Context, h *Host, sid string, b []byte) error {
	ss, err := s.getSession(sid, h)
	if err != nil {
		return err
	}
	if err := ss.ds.PutManifest(b); err != nil {
		return bad("%v", err)
	}
	return nil
}

func (s *Service) indexDigest(repo *Repo, group, snap string) (string, error) {
	st, err := s.Store(repo)
	if err != nil {
		return "", err
	}
	b, err := st.ReadIndex(group, snap)
	if err != nil {
		return "", err
	}
	return format.IndexDigest(b), nil
}

// Snapshots lists the committed snapshots of a group, oldest first. The
// datastore is authoritative; PostgreSQL adds operational metadata.
func (s *Service) Snapshots(ctx context.Context, h *Host, group string) (*api.SnapshotList, error) {
	if err := requireScope(h, api.ScopeBackup, api.ScopeSyncRead); err != nil {
		return nil, err
	}
	if !datastore.ValidGroup(group) {
		return nil, bad("invalid group id")
	}
	repo, err := s.RepositoryByID(ctx, h.RepoID)
	if err != nil {
		return nil, err
	}
	return s.snapshotList(ctx, repo, group)
}

func (s *Service) snapshotList(ctx context.Context, repo *Repo, group string) (*api.SnapshotList, error) {
	store, err := s.Store(repo)
	if err != nil {
		return nil, err
	}
	infos, err := store.ListSnapshots(group)
	if err != nil {
		return nil, err
	}
	meta, err := s.snapshotMeta(ctx, repo, group)
	if err != nil {
		return nil, err
	}
	l, _ := assembleSnapshotList(store, group, infos, meta[group])
	return l, nil
}

// snapshotMeta reads the PostgreSQL rows of one group's snapshots, or (group "") of every group of the
// repository in ONE query, keyed by group and snapshot id.
func (s *Service) snapshotMeta(ctx context.Context, repo *Repo, group string) (map[string]map[string]api.SnapshotRef, error) {
	q := `SELECT sn.group_id, sn.snapshot_id, sn.created_at, sn.committed_at, sn.index_digest, sn.chunk_count, sn.policy_name,
		COALESCE(o.project,''), COALESCE(o.name,''), COALESCE(o.kind,''), COALESCE(o.host_name,''), sn.logical_bytes, sn.verify_state, sn.notes
		FROM snapshots sn LEFT JOIN storage_objects o ON o.id=sn.object_id WHERE sn.repository_id=$1`
	args := []any{repo.ID}
	if group != "" {
		q += ` AND sn.group_id=$2`
		args = append(args, group)
	}
	rows, err := s.DB.Pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	meta := map[string]map[string]api.SnapshotRef{}
	for rows.Next() {
		var g string
		var r api.SnapshotRef
		if err := rows.Scan(&g, &r.SnapshotID, &r.Created, &r.Committed, &r.IndexDigest, &r.ChunkCount, &r.Policy, &r.Project, &r.Object, &r.Kind, &r.Host, &r.Logical, &r.VerifyState, &r.Notes); err != nil {
			return nil, err
		}
		if meta[g] == nil {
			meta[g] = map[string]api.SnapshotRef{}
		}
		meta[g][r.SnapshotID] = r
	}
	return meta, rows.Err()
}

// assembleSnapshotList merges the datastore's committed snapshots of a group (authoritative for
// existence and protection) with their PostgreSQL rows. rowless reports a snapshot without a row.
func assembleSnapshotList(store *datastore.Store, group string, infos []datastore.SnapshotInfo, meta map[string]api.SnapshotRef) (l *api.SnapshotList, rowless bool) {
	out := &api.SnapshotList{Snapshots: []api.SnapshotRef{}}
	for _, in := range infos {
		r, ok := meta[in.Snapshot]
		if !ok {
			rowless = true
			r = api.SnapshotRef{SnapshotID: in.Snapshot, Created: in.Receipt.Committed, Committed: in.Receipt.Committed, ChunkCount: in.Receipt.ChunkCount}
		}
		r.GroupID = group
		r.Protected = in.Protected // the datastore is authoritative for protection
		if !ok && in.HasVerify {   // a row missing from PostgreSQL (rebuild in progress): the repository knows
			if rec, err := store.ReadVerify(group, in.Snapshot); err == nil && rec != nil {
				r.VerifyState = rec.State
			}
		}
		out.Snapshots = append(out.Snapshots, r)
	}
	return out, rowless
}

// ReadIndex returns a snapshot's index file.
func (s *Service) ReadIndex(ctx context.Context, h *Host, group, snap string) ([]byte, error) {
	return s.readSnapFile(ctx, h, group, snap, true)
}

// ReadManifest returns a snapshot's encrypted manifest.
func (s *Service) ReadManifest(ctx context.Context, h *Host, group, snap string) ([]byte, error) {
	return s.readSnapFile(ctx, h, group, snap, false)
}

func (s *Service) readSnapFile(ctx context.Context, h *Host, group, snap string, index bool) ([]byte, error) {
	if err := requireScope(h, api.ScopeBackup, api.ScopeSyncRead); err != nil {
		return nil, err
	}
	repo, err := s.RepositoryByID(ctx, h.RepoID)
	if err != nil {
		return nil, err
	}
	st, err := s.Store(repo)
	if err != nil {
		return nil, err
	}
	var b []byte
	if index {
		b, err = st.ReadIndex(group, snap)
	} else {
		b, err = st.ReadManifest(group, snap)
	}
	if errors.Is(err, datastore.ErrNotFound) {
		return nil, fmt.Errorf("%w: snapshot", ErrNotFound)
	}
	return b, err
}

// ReadChunk returns a stored chunk blob.
func (s *Service) ReadChunk(ctx context.Context, h *Host, idHex string) ([]byte, error) {
	if err := requireScope(h, api.ScopeBackup, api.ScopeSyncRead); err != nil {
		return nil, err
	}
	id, err := format.ParseID(idHex)
	if err != nil {
		return nil, bad("%v", err)
	}
	repo, err := s.RepositoryByID(ctx, h.RepoID)
	if err != nil {
		return nil, err
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

// StartRun records the start of a client backup run.
func (s *Service) StartRun(ctx context.Context, h *Host, r api.RunStart) (string, error) {
	if err := requireScope(h, api.ScopeBackup); err != nil {
		return "", err
	}
	var id string
	err := s.DB.Pool().QueryRow(ctx, `INSERT INTO backup_runs (repository_id, host_id, started_at, objects_total) VALUES ($1,$2,$3,$4) RETURNING id`,
		h.RepoID, h.ID, r.Started, r.ObjectsTotal).Scan(&id)
	return id, err
}

// FinishRun records the result of a run.
func (s *Service) FinishRun(ctx context.Context, h *Host, id string, f api.RunFinish) error {
	if err := requireScope(h, api.ScopeBackup); err != nil {
		return err
	}
	switch f.Status {
	case "ok", "partial", "failed", "deferred":
	default:
		return bad("invalid status %q", f.Status)
	}
	ct, err := s.DB.Pool().Exec(ctx, `UPDATE backup_runs SET finished_at=$3, status=$4, objects_ok=$5, objects_failed=$6, logical_bytes=$7, uploaded_bytes=$8, error=$9, objects_deferred=$10
		WHERE id=$1::uuid AND host_id=$2`, id, h.ID, s.Now(), f.Status, f.ObjectsOK, f.ObjectsFailed, f.LogicalBytes, f.UploadedBytes, f.Error, max(f.ObjectsDeferred, 0))
	if err != nil {
		var pe interface{ SQLState() string }
		if errors.As(err, &pe) && pe.SQLState() == "22P02" {
			return bad("invalid run id")
		}
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: run", ErrNotFound)
	}
	return nil
}

var _ = pgx.ErrNoRows

// Objects lists the storage objects of the caller's repository with their latest
// snapshot. A freshly enrolled host uses it to find what a repository holds
// (disaster recovery) because group ids are opaque.
func (s *Service) Objects(ctx context.Context, h *Host) (*api.ObjectList, error) {
	if err := requireScope(h, api.ScopeBackup); err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool().Query(ctx, `SELECT o.group_id, o.project, o.name, o.kind, o.host_name, o.policy_name, o.shared_by,
		(SELECT count(*) FROM snapshots sn WHERE sn.object_id=o.id)
		FROM storage_objects o WHERE o.repository_id=$1 ORDER BY o.project, o.name`, h.RepoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &api.ObjectList{Objects: []api.RepoObject{}}
	for rows.Next() {
		var o api.RepoObject
		var shared []byte
		if err := rows.Scan(&o.GroupID, &o.Project, &o.Name, &o.Kind, &o.Host, &o.Policy, &shared, &o.Snapshots); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(shared, &o.SharedBy)
		out.Objects = append(out.Objects, o)
	}
	return out, rows.Err()
}
