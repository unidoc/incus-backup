package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"syscall"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/progress"
	"github.com/unidoc/incus-backup/internal/retention"
)

func (s *Service) startRun(ctx context.Context, repoID, kind string) (string, error) {
	var id string
	var task *string // the task executing the run (see internal/server/tasks), if any
	if t := progress.TaskID(ctx); t != "" {
		task = &t
	}
	err := s.DB.Pool().QueryRow(ctx, `INSERT INTO maintenance_runs (repository_id, kind, started_at, task_id) VALUES ($1,$2,$3,$4) RETURNING id`, repoID, kind, s.Now(), task).Scan(&id)
	return id, err
}

func (s *Service) finishRun(ctx context.Context, id string, stats any, runErr error) {
	b, _ := json.Marshal(stats)
	status, msg := "ok", ""
	if runErr != nil {
		status, msg = "failed", runErr.Error()
		if errors.Is(runErr, context.Canceled) {
			status = "cancelled" // stopped on request: not a failure to alert about
		}
	}
	ctx = context.WithoutCancel(ctx) // a cancelled run still records how it ended
	_, _ = s.DB.Pool().Exec(ctx, `UPDATE maintenance_runs SET finished_at=$2, status=$3, stats=$4, error=$5 WHERE id=$1`, id, s.Now(), status, b, msg)
}

// PruneResult reports a retention run (or, with DryRun, previews one: exactly what would be
// kept and removed, and why).
type PruneResult struct {
	Groups    int              `json:"groups"`
	Kept      int              `json:"kept"`
	Removed   int              `json:"removed"`
	Protected int              `json:"protected"`
	Plan      []PrunePlanEntry `json:"plan"`
	DryRun    bool             `json:"dry_run"`
}

// PrunePlanEntry is one snapshot's decision.
type PrunePlanEntry struct {
	Group     string `json:"group"`
	Snapshot  string `json:"snapshot"`
	Reason    string `json:"reason"`
	Keep      bool   `json:"keep"`
	Protected bool   `json:"protected"`
	Object    string `json:"object"` // "project/name"
	Policy    string `json:"policy"`
	Timezone  string `json:"timezone"`
}

// Prune applies each group's retention policy (the policy named by its latest
// snapshot) using only snapshot times. It never needs a key. Groups whose
// policy is unknown or has no keep rules are left untouched. Protected snapshots, and
// snapshots a running backup builds on, are never removed. Admin only (local).
func (s *Service) Prune(ctx context.Context, repoName string, dryRun bool) (*PruneResult, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	if !dryRun {
		if err := s.requireWritable(ctx, repo.ID); err != nil {
			return nil, err
		}
	}
	store, err := s.Store(repo)
	if err != nil {
		return nil, err
	}
	res := &PruneResult{DryRun: dryRun}
	var rid string
	if !dryRun {
		release, gerr := s.guard(ctx)
		if gerr != nil {
			return nil, gerr
		}
		defer release()
		rid, _ = s.startRun(ctx, repo.ID, "retention")
	}
	pols, err := s.policiesByRepoID(ctx, repo.ID)
	if err == nil {
		err = s.prune(ctx, repo, store, pols, res)
	}
	if !dryRun {
		s.finishRun(ctx, rid, res, err)
	}
	return res, err
}

// PrunePreview is Prune(dry run): the exact plan, with nothing removed.
func (s *Service) PrunePreview(ctx context.Context, repoName string) (*PruneResult, error) {
	return s.Prune(ctx, repoName, true)
}

// ownerName maps a logical object name to the name users assign policies to:
// "instance/bosch/root" and "instance/bosch" -> "bosch", "volume/biggi" -> "biggi".
func ownerName(name string) string {
	switch {
	case strings.HasPrefix(name, "instance/"):
		n := strings.TrimPrefix(name, "instance/")
		return strings.TrimSuffix(n, "/root")
	case strings.HasPrefix(name, "volume/"):
		return strings.TrimPrefix(name, "volume/")
	}
	return name
}

type retentionSubject struct {
	hostID, project, name, pinned string
}

// retentionPolicyFor decides which policy governs an object: an admin assignment
// (object > project > host) wins; otherwise the policy pinned when the object was
// first seen. Nothing a backup-scope host sends later can change it.
func (s *Service) retentionPolicyFor(ctx context.Context, sub retentionSubject, cache map[string][]api.Assignment) string {
	if sub.hostID != "" {
		as, ok := cache[sub.hostID]
		if !ok {
			rows, err := s.DB.Pool().Query(ctx, `SELECT a.project, a.object, p.name FROM policy_assignments a JOIN policies p ON p.id=a.policy_id WHERE a.host_id=$1`, sub.hostID)
			if err == nil {
				for rows.Next() {
					var a api.Assignment
					if rows.Scan(&a.Project, &a.Object, &a.Policy) == nil {
						as = append(as, a)
					}
				}
				rows.Close()
			}
			cache[sub.hostID] = as
		}
		if p := ResolvePolicy(as, sub.project, ownerName(sub.name)); p != "" {
			return p
		}
	}
	return sub.pinned
}

func (s *Service) prune(ctx context.Context, repo *Repo, store *datastore.Store, pols []api.Policy, res *PruneResult) error {
	byName := map[string]api.Policy{}
	for _, p := range pols {
		byName[p.Name] = p
	}
	groups, err := store.ListGroups()
	if err != nil {
		return err
	}
	pins, err := s.pinnedBases(ctx, repo.ID)
	if err != nil {
		return err
	}
	cache := map[string][]api.Assignment{}
	for gi, g := range groups {
		if err := ctx.Err(); err != nil {
			return err
		}
		progress.Set(ctx, progress.Pct(int64(gi), int64(len(groups)), 0, 99), "object %d of %d: %d kept, %d removed", gi+1, len(groups), res.Kept, res.Removed)
		infos, err := store.ListSnapshots(g)
		if err != nil {
			return err
		}
		if len(infos) == 0 {
			continue
		}
		res.Groups++
		var sub retentionSubject
		var hid *string
		_ = s.DB.Pool().QueryRow(ctx, `SELECT host_id, project, name, retention_policy FROM storage_objects WHERE repository_id=$1 AND group_id=$2`, repo.ID, g).
			Scan(&hid, &sub.project, &sub.name, &sub.pinned)
		if hid != nil {
			sub.hostID = *hid
		}
		polName := s.retentionPolicyFor(ctx, sub, cache)
		pol, ok := byName[polName]
		rules := retention.Rules{Last: pol.KeepLast, Hourly: pol.KeepHourly, Daily: pol.KeepDaily, Weekly: pol.KeepWeekly, Monthly: pol.KeepMonthly, Yearly: pol.KeepYearly}
		tzName := pol.Timezone
		if tzName == "" {
			tzName = "UTC"
		}
		loc, lerr := time.LoadLocation(tzName)
		if lerr != nil {
			s.Log.Error("retention: unknown time zone, keeping the whole group", "policy", polName, "tz", tzName)
			ok = false
		}
		var snaps []retention.Snapshot
		pinned := map[string]bool{}
		for _, in := range infos {
			p := in.Protected
			if pins[g][in.Snapshot] {
				p = true
				pinned[in.Snapshot] = true
			}
			// server-controlled time, never the host-supplied one
			snaps = append(snaps, retention.Snapshot{ID: in.Snapshot, Time: in.Receipt.Committed, Protected: p})
		}
		var plan []retention.Decision
		if ok {
			plan = retention.Plan(snaps, rules, loc)
		} else {
			plan = retention.Plan(snaps, retention.Rules{}, nil) // unknown policy: keep everything
		}
		obj := sub.project + "/" + sub.name
		for _, d := range plan {
			reason := d.Reason
			if pinned[d.ID] {
				reason = "pinned by a running backup"
			}
			res.Plan = append(res.Plan, PrunePlanEntry{Group: g, Snapshot: d.ID, Reason: reason, Keep: d.Keep, Protected: d.Protected,
				Object: obj, Policy: polName, Timezone: tzName})
			if d.Protected {
				res.Protected++
			}
			if d.Keep {
				res.Kept++
				continue
			}
			res.Removed++
			if res.DryRun {
				continue
			}
			if err := store.DeleteSnapshot(g, d.ID); err != nil {
				if errors.Is(err, datastore.ErrProtected) { // protected between the listing and now
					res.Removed--
					res.Kept++
					continue
				}
				if errors.Is(err, datastore.ErrNotFound) { // forgotten by someone else meanwhile: fine
					continue
				}
				return err
			}
			progress.Logf(ctx, progress.Info, "removed %s %s (%s, policy %s)", obj, d.ID, reason, polName)
			if _, err := s.DB.Pool().Exec(ctx, `DELETE FROM snapshots WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`, repo.ID, g, d.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetObjectRetention pins the retention policy of an object (admin only: the CLI). The object is
// addressed by project and name; an ambiguous pair (two objects with the same project and name, e.g.
// a VM root and a container root of one name) is refused: use SetObjectRetentionByGroup.
func (s *Service) SetObjectRetention(ctx context.Context, repoName, project, name, policy string) error {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	rows, err := s.DB.Pool().Query(ctx, `SELECT group_id FROM storage_objects WHERE repository_id=$1 AND project=$2 AND name=$3`, repo.ID, project, name)
	if err != nil {
		return err
	}
	var groups []string
	for rows.Next() {
		var g string
		if rows.Scan(&g) == nil {
			groups = append(groups, g)
		}
	}
	rows.Close()
	switch len(groups) {
	case 0:
		return fmt.Errorf("%w: object %s/%s", ErrNotFound, project, name)
	case 1:
		return s.SetObjectRetentionByGroup(ctx, repoName, groups[0], policy)
	}
	return fmt.Errorf("%w: %d objects are called %s/%s (different kinds): address one by group id", ErrConflict, len(groups), project, name)
}

// SetObjectRetentionByGroup pins the retention policy of exactly one object.
func (s *Service) SetObjectRetentionByGroup(ctx context.Context, repoName, group, policy string) error {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	if policy != "" {
		var n int
		if err := s.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM policies WHERE repository_id=$1 AND name=$2`, repo.ID, policy).Scan(&n); err != nil || n == 0 {
			return fmt.Errorf("%w: policy %q", ErrNotFound, policy)
		}
	}
	ct, err := s.DB.Pool().Exec(ctx, `UPDATE storage_objects SET retention_policy=$3 WHERE repository_id=$1 AND group_id=$2`, repo.ID, group, policy)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: object", ErrNotFound)
	}
	return nil
}

// GC runs chunk garbage collection.
func (s *Service) GC(ctx context.Context, repoName string, grace time.Duration, dryRun bool) (*datastore.GCStats, error) {
	if grace < MinGCGrace {
		return nil, bad("gc grace must be at least %s (it protects uploads whose session was lost)", MinGCGrace)
	}
	if !dryRun {
		release, gerr := s.guard(ctx)
		if gerr != nil {
			return nil, gerr
		}
		defer release()
	}
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	if !dryRun {
		if err := s.requireWritable(ctx, repo.ID); err != nil {
			return nil, err
		}
	}
	store, err := s.Store(repo)
	if err != nil {
		return nil, err
	}
	rid, _ := s.startRun(ctx, repo.ID, "gc")
	st, err := store.GC(ctx, grace, dryRun)
	s.finishRun(ctx, rid, st, err)
	return st, err
}

// VerifyRequest selects a verification job.
type VerifyRequest struct {
	// Mode: "all" (default: every snapshot and a full chunk scrub), "new" (only snapshots never
	// verified) or "stale" (never verified, or last verified more than StaleDays ago).
	Mode      string
	StaleDays int
	// NoQuarantine only reports corrupt chunks instead of moving them aside.
	NoQuarantine bool
	// Only restricts the job to these "group/snapshot" ids (used to re-verify repaired snapshots).
	Only []string
}

// Verify checks the whole repository without any key: every chunk, every snapshot.
// Corrupt chunks are quarantined and the verification state of every snapshot is recorded.
func (s *Service) Verify(ctx context.Context, repoName string) (*datastore.VerifyStats, error) {
	return s.VerifyJob(ctx, repoName, VerifyRequest{})
}

// VerifyJob runs a verification job (see VerifyRequest) and records its result in the
// repository (verify.json per snapshot) and PostgreSQL (state, history, corrupt chunks).
// Within the job each chunk is read at most once.
func (s *Service) VerifyJob(ctx context.Context, repoName string, req VerifyRequest) (*datastore.VerifyStats, error) {
	if !req.NoQuarantine { // quarantining deletes chunks: only the process that owns the pins may do it
		release, gerr := s.guard(ctx)
		if gerr != nil {
			return nil, gerr
		}
		defer release()
	}
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	if err := s.requireWritable(ctx, repo.ID); err != nil { // verification records its result in the repository
		return nil, err
	}
	store, err := s.Store(repo)
	if err != nil {
		return nil, err
	}
	opts := datastore.VerifyOpts{Quarantine: !req.NoQuarantine, Record: true, Now: s.Now}
	if len(req.Only) > 0 {
		only := map[string]bool{}
		for _, o := range req.Only {
			only[o] = true
		}
		opts.Select = func(si datastore.SnapshotInfo, _ *datastore.VerifyRecord) bool { return only[si.Group+"/"+si.Snapshot] }
		req.Mode = "only"
	}
	switch req.Mode {
	case "only":
	case "", "all":
		opts.AllChunks = true
	case "new":
		opts.Select = func(_ datastore.SnapshotInfo, rec *datastore.VerifyRecord) bool { return rec == nil }
	case "stale":
		// StaleDays > 0: one limit for everything. StaleDays == 0: each object's policy decides
		// (reverify_days; 0 there means "only ever verify new snapshots").
		perGroup, err := s.reverifyDaysByGroup(ctx, repo.ID)
		if err != nil {
			return nil, err
		}
		opts.Select = func(si datastore.SnapshotInfo, rec *datastore.VerifyRecord) bool {
			if rec == nil {
				return true
			}
			days := req.StaleDays
			if days == 0 {
				d, ok := perGroup[si.Group]
				if !ok {
					m, _ := s.maintenanceByID(ctx, repo.ID)
					d = m.DefaultReverifyDays
				}
				days = d
			}
			if days == 0 {
				return false
			}
			return s.Now().Sub(rec.At) > time.Duration(days)*24*time.Hour
		}
	default:
		return nil, bad("unknown verify mode %q", req.Mode)
	}
	kind := "verify"
	if opts.AllChunks {
		kind = "scrub"
	}
	rid, _ := s.startRun(ctx, repo.ID, kind)
	vs, err := store.VerifyWith(ctx, opts)
	if vs != nil {
		s.recordVerification(ctx, repo, vs, !req.NoQuarantine)
	}
	s.finishRun(ctx, rid, vs, err)
	if err == nil && len(vs.Problems) > 0 {
		return vs, fmt.Errorf("verify found %d problem(s)", len(vs.Problems))
	}
	return vs, err
}

// recordVerification mirrors a verify job into PostgreSQL.
func (s *Service) recordVerification(ctx context.Context, repo *Repo, vs *datastore.VerifyStats, quarantined bool) {
	now := s.Now()
	affected := map[string][]string{} // chunk id -> "group/snapshot"
	for _, r := range vs.Results {
		_, _ = s.DB.Pool().Exec(ctx, `UPDATE snapshots SET verify_state=$4, verified_at=$5 WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`,
			repo.ID, r.Group, r.Snapshot, r.State, now)
		pj, _ := json.Marshal(r.Problems)
		_, _ = s.DB.Pool().Exec(ctx, `INSERT INTO snapshot_verifications (repository_id, group_id, snapshot_id, verified_at, state, chunks, problems) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			repo.ID, r.Group, r.Snapshot, now, r.State, r.Chunks, pj)
		for _, p := range r.Problems {
			if i := strings.LastIndex(p, ": "); i > 0 && (strings.HasPrefix(p, "chunk-corrupt") || strings.HasPrefix(p, "chunk-missing")) {
				id := p[i+2:]
				affected[id] = append(affected[id], r.Group+"/"+r.Snapshot)
			}
		}
	}
	for _, p := range vs.Problems {
		if p.Kind != "chunk-corrupt" && p.Kind != "chunk-missing" {
			continue
		}
		id := p.Object
		if p.Kind == "chunk-missing" {
			id = p.Detail
		}
		if len(id) != 64 {
			continue
		}
		aj, _ := json.Marshal(affected[id])
		state := "quarantined"
		if !quarantined {
			state = "reported" // found but left in place (verify --no-quarantine): nothing is healed by that
		}
		_, _ = s.DB.Pool().Exec(ctx, `INSERT INTO corrupt_chunks (repository_id, chunk_id, detected_at, state, detail, affected) VALUES ($1,$2,$3,$6,$4,$5)
			ON CONFLICT (repository_id, chunk_id) DO UPDATE SET state=EXCLUDED.state, healed_at=NULL, detail=EXCLUDED.detail, affected=EXCLUDED.affected, detected_at=EXCLUDED.detected_at`,
			repo.ID, id, now, p.Kind+": "+p.Detail, aj, state)
	}
	s.healCorruptionRecords(ctx, repo)
}

// healCorruptionRecords marks quarantined chunks as healed once a healthy copy exists again
// (a later backup or sync uploaded the same content).
func (s *Service) healCorruptionRecords(ctx context.Context, repo *Repo) {
	rows, err := s.DB.Pool().Query(ctx, `SELECT chunk_id FROM corrupt_chunks WHERE repository_id=$1 AND state IN ('quarantined','reported')`, repo.ID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	store, err := s.openStore(ctx, repo)
	if err != nil {
		return
	}
	for _, id := range ids {
		cid, err := format.ParseID(id)
		if err != nil || !store.ChunkHealthy(cid) {
			continue
		}
		_, _ = s.DB.Pool().Exec(ctx, `UPDATE corrupt_chunks SET state='healed', healed_at=$3 WHERE repository_id=$1 AND chunk_id=$2`, repo.ID, id, s.Now())
	}
}

// CorruptChunk is a recorded corruption.
type CorruptChunk struct {
	ChunkID    string     `json:"chunk_id"`
	DetectedAt time.Time  `json:"detected_at"`
	State      string     `json:"state"`
	HealedAt   *time.Time `json:"healed_at"`
	Detail     string     `json:"detail"`
	Affected   []string   `json:"affected"`
}

// CorruptChunks lists the recorded corrupt chunks of a repository (newest first).
func (s *Service) CorruptChunks(ctx context.Context, repoName string, includeHealed bool) ([]CorruptChunk, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	s.healCorruptionRecords(ctx, repo)
	rows, err := s.DB.Pool().Query(ctx, `SELECT chunk_id, detected_at, state, healed_at, detail, affected FROM corrupt_chunks
		WHERE repository_id=$1 AND ($2 OR state='quarantined') ORDER BY detected_at DESC`, repo.ID, includeHealed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CorruptChunk
	for rows.Next() {
		var c CorruptChunk
		var aj []byte
		if err := rows.Scan(&c.ChunkID, &c.DetectedAt, &c.State, &c.HealedAt, &c.Detail, &aj); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(aj, &c.Affected)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReconcileResult reports what Reconcile repaired.
type ReconcileResult struct {
	Inserted int `json:"inserted"`
	Removed  int `json:"removed"`
}

// Reconcile makes PostgreSQL agree with the datastore, which is authoritative:
// committed snapshots missing from the database are recorded (this is also the
// rebuild path after losing the database) and rows without a snapshot removed.
func (s *Service) Reconcile(ctx context.Context, repoName string) (*ReconcileResult, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	store, err := s.Store(repo)
	if err != nil {
		return nil, err
	}
	rid, _ := s.startRun(ctx, repo.ID, "reconcile")
	res := &ReconcileResult{}
	err = s.reconcile(ctx, repo, store, res)
	s.finishRun(ctx, rid, res, err)
	return res, err
}

// snapRow is the reconcile-relevant part of a snapshots row.
type snapRow struct {
	protected   bool
	notes       string
	verifyState string
	verifiedAt  *time.Time
	hasHistory  bool // snapshot_verifications holds at least one row
}

// reconcileRows reads, in three queries, what PostgreSQL holds for the repository: the snapshot rows
// (keyed "group/snapshot") and the notes of every storage object (keyed by group).
func (s *Service) reconcileRows(ctx context.Context, repoID string) (map[string]*snapRow, map[string]string, error) {
	p := s.DB.Pool()
	snaps := map[string]*snapRow{}
	rows, err := p.Query(ctx, `SELECT group_id, snapshot_id, protected, notes, verify_state, verified_at FROM snapshots WHERE repository_id=$1`, repoID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var g, sn string
		r := &snapRow{}
		if err := rows.Scan(&g, &sn, &r.protected, &r.notes, &r.verifyState, &r.verifiedAt); err != nil {
			rows.Close()
			return nil, nil, err
		}
		snaps[g+"/"+sn] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows, err = p.Query(ctx, `SELECT DISTINCT group_id, snapshot_id FROM snapshot_verifications WHERE repository_id=$1`, repoID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var g, sn string
		if err := rows.Scan(&g, &sn); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if r := snaps[g+"/"+sn]; r != nil {
			r.hasHistory = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	notes := map[string]string{}
	rows, err = p.Query(ctx, `SELECT group_id, notes FROM storage_objects WHERE repository_id=$1`, repoID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g, n string
		if err := rows.Scan(&g, &n); err != nil {
			return nil, nil, err
		}
		notes[g] = n
	}
	return snaps, notes, rows.Err()
}

func (s *Service) reconcile(ctx context.Context, repo *Repo, store *datastore.Store, res *ReconcileResult) error {
	started := s.Now()
	present := map[string]bool{}
	// What PostgreSQL holds, read once instead of one query per snapshot. A row that appears after
	// this read (a concurrent commit) is simply not known here: the INSERT below is ON CONFLICT DO
	// NOTHING, and its metadata is then written unconditionally, as before.
	known, objNotes, err := s.reconcileRows(ctx, repo.ID)
	if err != nil {
		return err
	}
	groups, err := store.ListGroups()
	if err != nil {
		return err
	}
	for gi, g := range groups {
		if err := ctx.Err(); err != nil {
			return err
		}
		progress.Set(ctx, progress.Pct(int64(gi), int64(len(groups)), 0, 90), "object %d of %d: %d records added", gi+1, len(groups), res.Inserted)
		infos, err := store.ListSnapshots(g)
		if err != nil {
			return err
		}
		for _, in := range infos {
			present[g+"/"+in.Snapshot] = true
			cur, exists := known[g+"/"+in.Snapshot]
			s.syncSnapshotMetadata(ctx, repo, store, in, cur)
			if exists {
				continue
			}
			ib, err := store.ReadIndex(g, in.Snapshot)
			if err != nil {
				continue
			}
			idx, err := format.ParseIndex(ib)
			if err != nil {
				continue
			}
			var objectID *string
			var oid string
			if s.DB.Pool().QueryRow(ctx, `SELECT id FROM storage_objects WHERE repository_id=$1 AND group_id=$2`, repo.ID, g).Scan(&oid) == nil {
				objectID = &oid
			}
			if _, err := s.DB.Pool().Exec(ctx, `INSERT INTO snapshots (repository_id, object_id, group_id, snapshot_id, created_at, committed_at, source, logical_bytes, index_digest, chunk_count)
				VALUES ($1,$2,$3,$4,$5,$5,'reconcile',$6,$7,$8) ON CONFLICT DO NOTHING`,
				repo.ID, objectID, g, in.Snapshot, in.Receipt.Committed, int64(idx.Size), format.IndexDigest(ib), len(idx.ChunkIDs())); err != nil {
				return err
			}
			res.Inserted++
			s.syncSnapshotMetadata(ctx, repo, store, in, nil)
		}
		if n, err := store.GroupNotes(g); err == nil {
			if cur, ok := objNotes[g]; ok && cur == n {
				continue
			}
			_, _ = s.DB.Pool().Exec(ctx, `UPDATE storage_objects SET notes=$3 WHERE repository_id=$1 AND group_id=$2`, repo.ID, g, n)
		}
	}
	rows, err := s.DB.Pool().Query(ctx, `SELECT id, group_id, snapshot_id FROM snapshots WHERE repository_id=$1`, repo.ID)
	if err != nil {
		return err
	}
	type staleRow struct{ id, group, snap string }
	var stale []staleRow
	for rows.Next() {
		var id, g, sn string
		if err := rows.Scan(&id, &g, &sn); err != nil {
			rows.Close()
			return err
		}
		if !present[g+"/"+sn] {
			stale = append(stale, staleRow{id, g, sn})
		}
	}
	rows.Close()
	for _, st := range stale {
		// the listing was taken a while ago: a snapshot committed since then is NOT stale
		if store.SnapshotExists(st.group, st.snap) {
			continue
		}
		if _, err := s.DB.Pool().Exec(ctx, `DELETE FROM snapshots WHERE id=$1 AND committed_at < $2`, st.id, started); err != nil {
			return err
		}
		res.Removed++
	}
	return nil
}

// ---- capacity ----------------------------------------------------------------

// SampleCapacity records physical usage and filesystem space.
func (s *Service) SampleCapacity(ctx context.Context, repoName string) error {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	store, err := s.Store(repo)
	if err != nil {
		return err
	}
	bytes, chunks, err := store.Usage()
	if err != nil {
		return err
	}
	var snaps, logical int64
	_ = s.DB.Pool().QueryRow(ctx, `SELECT count(*), COALESCE(sum(logical_bytes),0) FROM snapshots WHERE repository_id=$1`, repo.ID).Scan(&snaps, &logical)
	var st syscall.Statfs_t
	var total, free int64
	// Only a filesystem reports free space. An object store does not expose enough
	// to be honest about it, so total/free stay 0 and the forecast says "unavailable".
	if repo.Backend != BackendS3 && syscall.Statfs(repo.Path, &st) == nil {
		total = int64(st.Blocks) * int64(st.Bsize)
		free = int64(st.Bavail) * int64(st.Bsize)
	}
	_, err = s.DB.Pool().Exec(ctx, `INSERT INTO capacity_samples (repository_id, taken_at, physical_bytes, logical_bytes, chunk_count, snapshot_count, fs_total_bytes, fs_free_bytes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, repo.ID, s.Now(), bytes, logical, chunks, snaps, total, free)
	return err
}

// Capacity is the storage report and forecast.
type Capacity struct {
	PhysicalBytes int64           `json:"physical_bytes"`
	LogicalBytes  int64           `json:"logical_bytes"`
	Chunks        int64           `json:"chunks"`
	Snapshots     int64           `json:"snapshots"`
	FSTotal       int64           `json:"fs_total"`
	FSFree        int64           `json:"fs_free"`
	DedupRatio    float64         `json:"dedup_ratio"`
	GrowthPerDay  map[int]float64 `json:"growth_per_day"` // window days -> bytes/day (7, 30, 90); NaN when unknown
	Projected80   *time.Time      `json:"projected80"`
	ProjectedFull *time.Time      `json:"projected_full"`
}

// Capacity reports current usage and projects when the filesystem reaches 80 %
// and 100 % by linear regression over the last 30 days of samples (needs at
// least two samples a day apart).
func (s *Service) Capacity(ctx context.Context, repoName string) (*Capacity, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	c := &Capacity{GrowthPerDay: map[int]float64{}}
	err = s.DB.Pool().QueryRow(ctx, `SELECT physical_bytes, logical_bytes, chunk_count, snapshot_count, fs_total_bytes, fs_free_bytes
		FROM capacity_samples WHERE repository_id=$1 ORDER BY taken_at DESC LIMIT 1`, repo.ID).
		Scan(&c.PhysicalBytes, &c.LogicalBytes, &c.Chunks, &c.Snapshots, &c.FSTotal, &c.FSFree)
	if err != nil {
		return nil, fmt.Errorf("%w: no capacity samples yet", ErrNotFound)
	}
	if c.PhysicalBytes > 0 {
		c.DedupRatio = float64(c.LogicalBytes) / float64(c.PhysicalBytes)
	}
	now := s.Now()
	for _, w := range []int{7, 30, 90} {
		c.GrowthPerDay[w] = s.growth(ctx, repo.ID, now.AddDate(0, 0, -w))
	}
	g := c.GrowthPerDay[30]
	if math.IsNaN(g) {
		g = c.GrowthPerDay[7]
	}
	if !math.IsNaN(g) && g > 0 && c.FSTotal > 0 {
		used := float64(c.FSTotal - c.FSFree)
		if t80 := 0.8*float64(c.FSTotal) - used; t80 > 0 {
			t := now.Add(time.Duration(t80 / g * 24 * float64(time.Hour)))
			c.Projected80 = &t
		}
		if full := float64(c.FSFree); full > 0 {
			t := now.Add(time.Duration(full / g * 24 * float64(time.Hour)))
			c.ProjectedFull = &t
		}
	}
	return c, nil
}

// growth is a least-squares slope of physical bytes (bytes/day) since `since`.
func (s *Service) growth(ctx context.Context, repoID string, since time.Time) float64 {
	rows, err := s.DB.Pool().Query(ctx, `SELECT taken_at, physical_bytes FROM capacity_samples WHERE repository_id=$1 AND taken_at >= $2 ORDER BY taken_at`, repoID, since)
	if err != nil {
		return math.NaN()
	}
	defer rows.Close()
	var xs, ys []float64
	for rows.Next() {
		var t time.Time
		var b int64
		if rows.Scan(&t, &b) != nil {
			return math.NaN()
		}
		xs = append(xs, float64(t.Unix())/86400)
		ys = append(ys, float64(b))
	}
	return slope(xs, ys)
}

func slope(xs, ys []float64) float64 {
	n := float64(len(xs))
	if n < 2 || xs[len(xs)-1]-xs[0] < 0.5 {
		return math.NaN()
	}
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return math.NaN()
	}
	return (n*sxy - sx*sy) / den
}

// ---- status --------------------------------------------------------------------

// HostStatus summarises one backup host.
type HostStatus struct {
	Name        string     `json:"name"`
	LastSeen    *time.Time `json:"last_seen"`
	Objects     int        `json:"objects"`
	Protected   int        `json:"protected"`
	LastRunAt   *time.Time `json:"last_run_at"`
	LastRunOK   string     `json:"last_run_ok"`
	Unprotected []string   `json:"unprotected"`
}

// Status is the control-plane health overview of a repository.
type Status struct {
	Repository   string       `json:"repository"`
	Snapshots    int          `json:"snapshots"`
	Objects      int          `json:"objects"`
	Hosts        []HostStatus `json:"hosts"`
	RunsOK24h    int          `json:"runs_ok24h"`
	RunsFail24h  int          `json:"runs_fail24h"`
	LastVerify   *time.Time   `json:"last_verify"`
	LastGC       *time.Time   `json:"last_gc"`
	OpenSessions int          `json:"open_sessions"`
}

// Status builds the overview.
func (s *Service) Status(ctx context.Context, repoName string) (*Status, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	p := s.DB.Pool()
	st := &Status{Repository: repo.Name}
	_ = p.QueryRow(ctx, `SELECT count(*) FROM snapshots WHERE repository_id=$1`, repo.ID).Scan(&st.Snapshots)
	_ = p.QueryRow(ctx, `SELECT count(*) FROM storage_objects WHERE repository_id=$1`, repo.ID).Scan(&st.Objects)
	_ = p.QueryRow(ctx, `SELECT count(*) FROM backup_runs WHERE repository_id=$1 AND status='ok' AND started_at > $2`, repo.ID, s.Now().Add(-24*time.Hour)).Scan(&st.RunsOK24h)
	_ = p.QueryRow(ctx, `SELECT count(*) FROM backup_runs WHERE repository_id=$1 AND status IN ('failed','partial') AND started_at > $2`, repo.ID, s.Now().Add(-24*time.Hour)).Scan(&st.RunsFail24h)
	_ = p.QueryRow(ctx, `SELECT max(finished_at) FROM maintenance_runs WHERE repository_id=$1 AND kind='verify' AND status='ok'`, repo.ID).Scan(&st.LastVerify)
	_ = p.QueryRow(ctx, `SELECT max(finished_at) FROM maintenance_runs WHERE repository_id=$1 AND kind='gc' AND status='ok'`, repo.ID).Scan(&st.LastGC)
	s.mu.Lock()
	st.OpenSessions = len(s.sessions)
	s.mu.Unlock()
	rows, err := p.Query(ctx, `SELECT id, name, last_seen_at FROM hosts WHERE repository_id=$1 AND scope='backup' ORDER BY name`, repo.ID)
	if err != nil {
		return nil, err
	}
	type hrow struct {
		id string
		hs HostStatus
	}
	var hs []hrow
	for rows.Next() {
		var h hrow
		if err := rows.Scan(&h.id, &h.hs.Name, &h.hs.LastSeen); err != nil {
			rows.Close()
			return nil, err
		}
		hs = append(hs, h)
	}
	rows.Close()
	for _, h := range hs {
		orows, err := p.Query(ctx, `SELECT o.project, o.name, EXISTS (SELECT 1 FROM snapshots sn WHERE sn.object_id=o.id) FROM storage_objects o WHERE o.host_id=$1`, h.id)
		if err != nil {
			return nil, err
		}
		for orows.Next() {
			var pr, nm string
			var has bool
			if err := orows.Scan(&pr, &nm, &has); err != nil {
				orows.Close()
				return nil, err
			}
			h.hs.Objects++
			if has {
				h.hs.Protected++
			} else {
				h.hs.Unprotected = append(h.hs.Unprotected, pr+"/"+nm)
			}
		}
		orows.Close()
		_ = p.QueryRow(ctx, `SELECT finished_at, status FROM backup_runs WHERE host_id=$1 AND finished_at IS NOT NULL ORDER BY started_at DESC LIMIT 1`, h.id).Scan(&h.hs.LastRunAt, &h.hs.LastRunOK)
		st.Hosts = append(st.Hosts, h.hs)
	}
	return st, nil
}

// syncSnapshotMetadata copies the repository's per-snapshot metadata (protection, notes,
// verification state and history) into PostgreSQL. The datastore is authoritative, so this is
// what makes a rebuilt database come back with protected snapshots and verify state intact.
//
// cur, when not nil, is the row as PostgreSQL held it when the reconcile run started: values that
// already agree are not written again (a steady-state run then issues no write per snapshot). Any
// difference, or no knowledge (nil), writes as before.
func (s *Service) syncSnapshotMetadata(ctx context.Context, repo *Repo, store *datastore.Store, in datastore.SnapshotInfo, cur *snapRow) {
	notes := ""
	if in.HasNotes {
		notes, _ = store.Notes(in.Group, in.Snapshot)
	}
	if cur == nil || cur.protected != in.Protected || cur.notes != notes {
		_, _ = s.DB.Pool().Exec(ctx, `UPDATE snapshots SET protected=$4, notes=$5 WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`,
			repo.ID, in.Group, in.Snapshot, in.Protected, notes)
	}
	if !in.HasVerify {
		return
	}
	rec, err := store.ReadVerify(in.Group, in.Snapshot)
	if err != nil || rec == nil {
		return
	}
	// PostgreSQL keeps microseconds: compare at that precision
	if cur == nil || cur.verifyState != rec.State || cur.verifiedAt == nil || !cur.verifiedAt.Equal(rec.At.Truncate(time.Microsecond)) {
		_, _ = s.DB.Pool().Exec(ctx, `UPDATE snapshots SET verify_state=$4, verified_at=$5 WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`,
			repo.ID, in.Group, in.Snapshot, rec.State, rec.At)
	}
	n := 1
	if cur == nil || !cur.hasHistory {
		_ = s.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM snapshot_verifications WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`, repo.ID, in.Group, in.Snapshot).Scan(&n)
	}
	if n == 0 {
		for _, ev := range rec.History {
			pj, _ := json.Marshal(ev.Problems)
			_, _ = s.DB.Pool().Exec(ctx, `INSERT INTO snapshot_verifications (repository_id, group_id, snapshot_id, verified_at, state, chunks, problems) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				repo.ID, in.Group, in.Snapshot, ev.At, ev.State, ev.Chunks, pj)
		}
	}
}

// reverifyDaysByGroup maps each object's group id to its governing policy's reverify_days.
func (s *Service) reverifyDaysByGroup(ctx context.Context, repoID string) (map[string]int, error) {
	rows, err := s.DB.Pool().Query(ctx, `SELECT o.group_id, p.reverify_days FROM storage_objects o JOIN policies p ON p.repository_id=o.repository_id AND p.name=o.retention_policy WHERE o.repository_id=$1`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var g string
		var d int
		if err := rows.Scan(&g, &d); err != nil {
			return nil, err
		}
		out[g] = d
	}
	return out, rows.Err()
}
