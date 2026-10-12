package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/progress"
)

// ---- repository detach / destroy ------------------------------------------------------------
//
// Retiring a repository needs it OFFLINE first (no backup, restore or maintenance touches it), no
// open upload session, and no enrolled identity unless the caller lists every one of them explicitly
// (they lose their enrolment: the hosts rows go with the repository).
//
//   - detach forgets the repository in PostgreSQL and leaves the datastore untouched (it can be attached
//     again, also on another server; ClearOwner releases owner.json so that needs no --adopt);
//   - destroy deletes every object of the datastore (S3: every key under the prefix; filesystem: the
//     directory's contents, the directory itself stays) and then forgets it. It is meant to run as a
//     task (progress, cancel); if it fails or is cancelled the repository stays registered and offline,
//     with the reason, so nothing half-deleted looks healthy.

// RemoveRequest are the safety inputs of detach and destroy.
type RemoveRequest struct {
	// Confirm must equal the repository name (typed confirmation; destroy only).
	Confirm string `json:"confirm"`
	// ForceHosts must list every enrolled identity (host and sync peer names) when there are any.
	ForceHosts []string `json:"force_hosts"`
	// ClearOwner releases owner.json on detach (when this server owns it).
	ClearOwner bool `json:"clear_owner"`
}

// RemovePreflight checks that a repository may be detached or destroyed now.
func (s *Service) RemovePreflight(ctx context.Context, name string, req RemoveRequest, destroy bool) (*Repo, error) {
	repo, err := s.Repository(ctx, name)
	if err != nil {
		return nil, err
	}
	if destroy && req.Confirm != repo.Name {
		return nil, bad("type the repository name (%s) to confirm that every backup in it is deleted", repo.Name)
	}
	if s.persistedMode(ctx, repo.ID) != datastore.ModeOffline {
		return nil, fmt.Errorf("%w: set repository %s offline first (no backup, restore or maintenance may use it while it is removed)", ErrConflict, repo.Name)
	}
	var open int
	if err := s.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM sessions WHERE repository_id=$1 AND state='open' AND expires_at >= $2`, repo.ID, s.Now()).Scan(&open); err != nil {
		return nil, err
	}
	if open > 0 {
		return nil, fmt.Errorf("%w: %d upload session(s) are still open on %s; wait until they end", ErrConflict, open, repo.Name)
	}
	names, err := s.identityNames(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	if len(names) > 0 {
		want := map[string]bool{}
		for _, n := range req.ForceHosts {
			want[n] = true
		}
		var missing []string
		for _, n := range names {
			if !want[n] {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 || len(want) != len(names) {
			return nil, fmt.Errorf("%w: %d identit(ies) are enrolled in %s (%s); they lose their enrolment: list every one of them in force_hosts to go ahead",
				ErrConflict, len(names), repo.Name, strings.Join(names, ", "))
		}
	}
	return repo, nil
}

func (s *Service) identityNames(ctx context.Context, repoID string) ([]string, error) {
	rows, err := s.DB.Pool().Query(ctx, `SELECT DISTINCT name FROM hosts WHERE repository_id=$1 ORDER BY name`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// rawBackend opens the repository's storage WITHOUT the maintenance-mode wrapper (the repository is
// offline while it is removed) and checks it is the datastore PostgreSQL knows.
func (s *Service) rawBackend(ctx context.Context, repo *Repo) (datastore.Backend, error) {
	var b datastore.Backend
	var err error
	if repo.Backend == BackendS3 {
		b, err = s.S3Backend(ctx, repo)
	} else {
		b, err = datastore.NewFSBackend(repo.Path)
	}
	if err != nil {
		return nil, err
	}
	st, err := datastore.OpenBackend(b)
	if err != nil {
		return nil, fmt.Errorf("repository %s: %w", repo.Name, err)
	}
	m, err := st.Meta()
	if err != nil {
		return nil, err
	}
	if repo.DatastoreID != "" && m.ID != repo.DatastoreID {
		return nil, fmt.Errorf("repository %s: the datastore at %s is a different repository (id %s, expected %s); refusing to touch it",
			repo.Name, b.Describe(), m.ID, repo.DatastoreID)
	}
	return b, nil
}

// forget removes the repository's rows (cascading to hosts, snapshots, objects, policies, runs...) and
// drops its open store.
func (s *Service) forget(ctx context.Context, repo *Repo) error {
	if _, err := s.DB.Pool().Exec(ctx, `DELETE FROM repositories WHERE id=$1`, repo.ID); err != nil {
		return fmt.Errorf("forgetting repository %s: %w", repo.Name, err)
	}
	s.mu.Lock()
	delete(s.stores, repo.ID)
	delete(s.modes, repo.ID)
	s.mu.Unlock()
	return nil
}

// DetachResult reports a detach.
type DetachResult struct {
	Repository   string `json:"repository"`
	Location     string `json:"location"`
	OwnerCleared bool   `json:"owner_cleared"`
}

// DetachRepository forgets a repository and leaves its datastore as it is.
func (s *Service) DetachRepository(ctx context.Context, name string, req RemoveRequest) (*DetachResult, error) {
	release, err := s.guard(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	repo, err := s.RemovePreflight(ctx, name, req, false)
	if err != nil {
		return nil, err
	}
	res := &DetachResult{Repository: repo.Name, Location: repo.Path}
	if req.ClearOwner {
		b, err := s.rawBackend(ctx, repo)
		if err != nil {
			return nil, err
		}
		id, err := s.ServerID(ctx)
		if err != nil {
			return nil, err
		}
		cur, err := datastore.ReadOwner(b)
		if err != nil {
			return nil, err
		}
		if cur != nil && cur.ServerID == id {
			if err := b.Delete("owner.json"); err != nil {
				return nil, fmt.Errorf("releasing owner.json: %w", err)
			}
			res.OwnerCleared = true
		}
	}
	if err := s.forget(ctx, repo); err != nil {
		return nil, err
	}
	s.Log.Warn("repository detached (datastore left in place)", "repository", repo.Name, "location", repo.Path, "owner_cleared", res.OwnerCleared)
	return res, nil
}

// DestroyResult reports a destroy.
type DestroyResult struct {
	Repository     string `json:"repository"`
	Location       string `json:"location"`
	ObjectsDeleted int64  `json:"objects_deleted"`
	BytesDeleted   int64  `json:"bytes_deleted"`
}

// DestroyRepository deletes a repository's datastore content and forgets it. Run RemovePreflight first
// (synchronously) and this in a task; it checks again.
func (s *Service) DestroyRepository(ctx context.Context, name string, req RemoveRequest) (res *DestroyResult, err error) {
	release, err := s.guard(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	repo, err := s.RemovePreflight(ctx, name, req, true)
	if err != nil {
		return nil, err
	}
	res = &DestroyResult{Repository: repo.Name, Location: repo.Path}
	defer func() {
		if err != nil { // registered, offline, and saying why: a half-deleted repository must not look healthy
			reason := "destroy did not finish: " + err.Error()
			if len(reason) > 500 {
				reason = reason[:500]
			}
			_, _ = s.DB.Pool().Exec(context.WithoutCancel(ctx), `UPDATE repositories SET mode_reason=$2 WHERE id=$1`, repo.ID, reason)
		}
	}()
	b, err := s.rawBackend(ctx, repo)
	if err != nil {
		return res, err
	}
	progress.Set(ctx, 0, "listing every object of %s", b.Describe())
	type obj struct {
		key  string
		size int64
	}
	var keys []obj
	if err := b.List("", func(k string, in datastore.Info) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		keys = append(keys, obj{k, in.Size})
		return nil
	}); err != nil {
		return res, fmt.Errorf("listing the datastore: %w", err)
	}
	// the repository marker goes last: until then the datastore is still recognisably this repository
	sort.SliceStable(keys, func(i, j int) bool { return !isMarker(keys[i].key) && isMarker(keys[j].key) })
	progress.Logf(ctx, progress.Info, "deleting %d objects from %s", len(keys), b.Describe())
	var done, bytes atomic.Int64
	var firstErr error
	var mu sync.Mutex
	work := make(chan obj)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for o := range work {
				if err := b.Delete(o.key); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("deleting %s: %w", o.key, err)
					}
					mu.Unlock()
					continue
				}
				n := done.Add(1)
				bytes.Add(o.size)
				if n%500 == 0 {
					progress.Set(ctx, progressPct(n, len(keys)), "%d of %d objects deleted", n, len(keys))
				}
			}
		}()
	}
	var markers []obj
	for _, o := range keys {
		if isMarker(o.key) {
			markers = append(markers, o)
			continue
		}
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop || ctx.Err() != nil {
			break
		}
		work <- o
	}
	close(work)
	wg.Wait()
	res.ObjectsDeleted, res.BytesDeleted = done.Load(), bytes.Load()
	if firstErr != nil {
		return res, firstErr
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	for _, o := range markers {
		if err := b.Delete(o.key); err != nil {
			return res, fmt.Errorf("deleting %s: %w", o.key, err)
		}
		res.ObjectsDeleted++
		res.BytesDeleted += o.size
	}
	if repo.Backend != BackendS3 {
		// temp files and the now empty directories (the directory itself stays: it may be a mount point)
		ents, err := os.ReadDir(repo.Path)
		if err != nil {
			return res, err
		}
		for _, e := range ents {
			if err := os.RemoveAll(filepath.Join(repo.Path, e.Name())); err != nil {
				return res, fmt.Errorf("cleaning %s: %w", repo.Path, err)
			}
		}
	}
	progress.Set(ctx, 99, "datastore empty; forgetting the repository")
	if err := s.forget(context.WithoutCancel(ctx), repo); err != nil {
		return res, err
	}
	s.Log.Warn("repository destroyed", "repository", repo.Name, "location", repo.Path, "objects", res.ObjectsDeleted, "bytes", res.BytesDeleted)
	return res, nil
}

func isMarker(k string) bool { return !strings.Contains(k, "/") }

func progressPct(done int64, total int) int {
	return progress.Pct(done, int64(total), 1, 98)
}

// ---- whole backup group ----------------------------------------------------------------------

// GroupDeleteResult reports a group delete.
type GroupDeleteResult struct {
	Group              string `json:"group"`
	Object             string `json:"object"`
	SnapshotsDeleted   int    `json:"snapshots_deleted"`
	ProtectedOverruled int    `json:"protected_overruled"`
}

// DeleteGroupPreflight checks a group delete: the repository is online, the group exists, no backup
// runs into it, and protected snapshots are only removed with forceProtected.
func (s *Service) DeleteGroupPreflight(ctx context.Context, repoName, group string, forceProtected bool) (*Repo, error) {
	if !datastore.ValidGroup(group) {
		return nil, bad("invalid group id")
	}
	repo, st, err := s.repoStore(ctx, repoName)
	if err != nil {
		return nil, err
	}
	if err := s.requireWritable(ctx, repo.ID); err != nil {
		return nil, err
	}
	infos, err := st.ListSnapshots(group)
	if err != nil {
		return nil, err
	}
	var known int
	_ = s.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM storage_objects WHERE repository_id=$1 AND group_id=$2`, repo.ID, group).Scan(&known)
	if len(infos) == 0 && known == 0 {
		return nil, fmt.Errorf("%w: object %s", ErrNotFound, group)
	}
	if busy, err := s.groupBusy(ctx, repo.ID, group); err != nil {
		return nil, err
	} else if busy {
		return nil, fmt.Errorf("%w: a backup of this object is running; wait until it ends", ErrConflict)
	}
	prot := 0
	for _, in := range infos {
		if in.Protected {
			prot++
		}
	}
	if prot > 0 && !forceProtected {
		return nil, fmt.Errorf("%w: %d snapshot(s) of this object are protected; unprotect them or pass force_protected", ErrConflict, prot)
	}
	return repo, nil
}

// DeleteGroup deletes every snapshot of one object and forgets the object (its chunks are freed by the
// next GC). Snapshots a running backup builds on stop it with a conflict.
func (s *Service) DeleteGroup(ctx context.Context, repoName, group string, forceProtected bool) (*GroupDeleteResult, error) {
	release, err := s.guard(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	repo, err := s.DeleteGroupPreflight(ctx, repoName, group, forceProtected)
	if err != nil {
		return nil, err
	}
	_, st, err := s.repoStore(ctx, repoName)
	if err != nil {
		return nil, err
	}
	// Hold the group's writer slot for the whole delete: a host cannot start a session into the group
	// meanwhile (it would be told 200 for a commit the final sweep then removes). guard() is only the
	// daemon lock.
	sid, err := s.acquireWriter(ctx, repo.ID, group, "", nil, "", s.Now().Add(6*time.Hour))
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = s.DB.Pool().Exec(context.WithoutCancel(ctx), `UPDATE sessions SET state='committed' WHERE id=$1::uuid`, uuidFromHex(sid))
	}()
	res := &GroupDeleteResult{Group: group}
	var project, name string
	_ = s.DB.Pool().QueryRow(ctx, `SELECT project, name FROM storage_objects WHERE repository_id=$1 AND group_id=$2`, repo.ID, group).Scan(&project, &name)
	if name != "" {
		res.Object = project + "/" + name
	}
	pins, err := s.pinnedBases(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	if len(pins[group]) > 0 {
		return nil, fmt.Errorf("%w: a running backup builds on a snapshot of this object", ErrConflict)
	}
	infos, err := st.ListSnapshots(group)
	if err != nil {
		return nil, err
	}
	for i, in := range infos {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		progress.Set(ctx, progress.Pct(int64(i), int64(len(infos)), 0, 95), "%d of %d snapshots deleted", i, len(infos))
		if in.Protected {
			if !forceProtected {
				return res, fmt.Errorf("%w: snapshot %s was protected meanwhile", ErrConflict, in.Snapshot)
			}
			if err := st.SetProtected(group, in.Snapshot, false); err != nil {
				return res, mapStoreErr(err, "snapshot")
			}
			res.ProtectedOverruled++
			progress.Logf(ctx, progress.Warn, "snapshot %s was protected: protection removed (force_protected)", in.Snapshot)
		}
		if err := st.DeleteSnapshot(group, in.Snapshot); err != nil && !errors.Is(err, datastore.ErrNotFound) {
			return res, mapStoreErr(err, "snapshot "+in.Snapshot)
		}
		if _, err := s.DB.Pool().Exec(ctx, `DELETE FROM snapshots WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`, repo.ID, group, in.Snapshot); err != nil {
			return res, err
		}
		res.SnapshotsDeleted++
		progress.Logf(ctx, progress.Info, "deleted snapshot %s", in.Snapshot)
	}
	// group-level files (notes) and anything left under the group
	if err := st.PurgeGroup(group); err != nil {
		return res, err
	}
	p := s.DB.Pool()
	_, _ = p.Exec(ctx, `DELETE FROM snapshots WHERE repository_id=$1 AND group_id=$2`, repo.ID, group)
	_, _ = p.Exec(ctx, `DELETE FROM snapshot_verifications WHERE repository_id=$1 AND group_id=$2`, repo.ID, group)
	if _, err := p.Exec(ctx, `DELETE FROM storage_objects WHERE repository_id=$1 AND group_id=$2`, repo.ID, group); err != nil {
		return res, err
	}
	s.Log.Warn("backup group deleted", "repository", repo.Name, "group", group, "object", res.Object, "snapshots", res.SnapshotsDeleted, "protected_overruled", res.ProtectedOverruled)
	return res, nil
}
