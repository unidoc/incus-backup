package service

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/schedule"
)

// Read models for the infrastructure pages of the management UI (hosts, objects, capacity):
// backup run history, pending enrolment tokens and per-object freshness/size statistics. All of
// them are plain aggregate queries over the control plane; none reads the datastore.

// ---- freshness ------------------------------------------------------------------------------

// Freshness states of a storage object.
const (
	FreshOK          = "ok"          // has a snapshot that is not overdue
	FreshStale       = "stale"       // the last snapshot is older than its schedule allows
	FreshNever       = "never"       // no snapshot yet
	FreshUnscheduled = "unscheduled" // no (parseable, non-manual) schedule: freshness cannot be judged
	FreshReference   = "reference"   // an instance record; its disks carry the data and are judged instead
)

// Freshness judges one object against its policy schedule. It is THE rule the overview, the object
// list and the host pages share: `sched`/`known` is the schedule of the governing policy (retention
// policy, else the declared one), `last` the creation time of the newest snapshot. It returns the
// state, when the next snapshot is due and when the object counts as overdue (zero times when not
// scheduled).
func Freshness(sched string, known bool, last *time.Time, now time.Time) (state string, due, overdueAt time.Time) {
	if last == nil {
		return FreshNever, time.Time{}, time.Time{}
	}
	spec, err := schedule.Parse(sched)
	if !known || err != nil || spec.Manual() {
		return FreshUnscheduled, time.Time{}, time.Time{}
	}
	next := spec.Next(*last)
	if next.IsZero() {
		return FreshOK, time.Time{}, time.Time{}
	}
	grace := 2 * time.Hour
	if iv := spec.Next(next).Sub(next); iv/4 > grace {
		grace = iv / 4
	}
	if now.After(next.Add(grace)) {
		return FreshStale, next, next.Add(grace)
	}
	return FreshOK, next, next.Add(grace)
}

// ---- backup runs ----------------------------------------------------------------------------

// BackupRunRow is one backup run a host reported (backup_runs).
type BackupRunRow struct {
	ID            string     `json:"id"`
	Host          string     `json:"host"` // "" when the host identity was deleted
	Started       time.Time  `json:"started"`
	Finished      *time.Time `json:"finished"`
	Status        string     `json:"status"` // running | ok | partial | failed
	ObjectsTotal  int        `json:"objects_total"`
	ObjectsOK     int        `json:"objects_ok"`
	ObjectsFailed int        `json:"objects_failed"`
	LogicalBytes  int64      `json:"logical_bytes"`
	UploadedBytes int64      `json:"uploaded_bytes"`
	Error         string     `json:"error"`
}

// BackupRunFilter selects backup runs; zero values mean "any".
type BackupRunFilter struct {
	Host   string
	Status string
	Before time.Time // runs that started strictly before (paging)
	Limit  int       // 1..1000, default 100
}

// BackupRuns lists the backup runs of a repository, newest first.
func (s *Service) BackupRuns(ctx context.Context, repoName string, f BackupRunFilter) ([]BackupRunRow, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	q := `SELECT b.id::text, COALESCE(h.name,''), b.started_at, b.finished_at, b.status, b.objects_total, b.objects_ok, b.objects_failed,
		b.logical_bytes, b.uploaded_bytes, b.error
		FROM backup_runs b LEFT JOIN hosts h ON h.id=b.host_id WHERE b.repository_id=$1`
	args := []any{repo.ID}
	if f.Host != "" {
		args = append(args, f.Host)
		q += fmt.Sprintf(" AND h.name=$%d", len(args))
	}
	if f.Status != "" {
		args = append(args, f.Status)
		q += fmt.Sprintf(" AND b.status=$%d", len(args))
	}
	if !f.Before.IsZero() {
		args = append(args, f.Before)
		q += fmt.Sprintf(" AND b.started_at < $%d", len(args))
	}
	args = append(args, f.Limit)
	q += fmt.Sprintf(" ORDER BY b.started_at DESC, b.id LIMIT $%d", len(args))
	rows, err := s.DB.Pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BackupRunRow{}
	for rows.Next() {
		var r BackupRunRow
		if err := rows.Scan(&r.ID, &r.Host, &r.Started, &r.Finished, &r.Status, &r.ObjectsTotal, &r.ObjectsOK, &r.ObjectsFailed,
			&r.LogicalBytes, &r.UploadedBytes, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- pending enrolment tokens ---------------------------------------------------------------

// PendingEnrolment is an enrolment token that was issued and not used yet (never the token itself:
// only its hash is stored, and not even that leaves the server).
type PendingEnrolment struct {
	ID      string    `json:"id"`
	Host    string    `json:"host"`
	Scope   string    `json:"scope"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	// Replaces: an identity with this name and scope is already enrolled; using the token revokes its
	// earlier certificates (re-enrolment).
	Replaces bool `json:"replaces"`
}

// PendingEnrolments lists the unused, unexpired enrolment tokens of a repository, newest first.
func (s *Service) PendingEnrolments(ctx context.Context, repoName string) ([]PendingEnrolment, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool().Query(ctx, `SELECT t.id::text, t.host_name, t.scope, t.created_at, t.expires_at,
			EXISTS (SELECT 1 FROM hosts h WHERE h.repository_id=t.repository_id AND h.name=t.host_name AND h.scope=t.scope)
		FROM enrollment_tokens t WHERE t.repository_id=$1 AND t.used_at IS NULL AND t.expires_at > $2
		ORDER BY t.created_at DESC, t.id`, repo.ID, s.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingEnrolment{}
	for rows.Next() {
		var p PendingEnrolment
		if err := rows.Scan(&p.ID, &p.Host, &p.Scope, &p.Created, &p.Expires, &p.Replaces); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// RevokeEnrolment deletes an unused enrolment token so it can no longer be used.
func (s *Service) RevokeEnrolment(ctx context.Context, repoName, id string) error {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	id = strings.ToLower(id)
	if !uuidRE.MatchString(id) {
		return fmt.Errorf("%w: no pending enrolment token %q", ErrNotFound, id)
	}
	ct, err := s.DB.Pool().Exec(ctx, `DELETE FROM enrollment_tokens WHERE id=$1::uuid AND repository_id=$2 AND used_at IS NULL`, id, repo.ID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: no pending enrolment token %q (already used or revoked)", ErrNotFound, id)
	}
	return nil
}

// ---- per-object statistics ------------------------------------------------------------------

// ObjectStat is one storage object with its freshness and size history, for the object list, the
// host page and the capacity page.
type ObjectStat struct {
	GroupID         string     `json:"group_id"`
	Host            string     `json:"host"`
	Project         string     `json:"project"`
	Name            string     `json:"name"`
	Kind            string     `json:"kind"`
	DeclaredPolicy  string     `json:"declared_policy"`
	RetentionPolicy string     `json:"retention_policy"`
	Policy          string     `json:"policy"`   // the policy whose schedule judged freshness ("" = none known)
	Schedule        string     `json:"schedule"` // that policy's schedule ("" = none known)
	Notes           string     `json:"notes"`
	FirstSeen       time.Time  `json:"first_seen"`
	Snapshots       int        `json:"snapshots"`
	Protected       int        `json:"protected"`     // protected snapshots
	FailedVerify    int        `json:"failed_verify"` // snapshots whose last verification failed
	Latest          *SnapStat  `json:"latest"`        // nil: no snapshot
	Freshness       string     `json:"freshness"`     // ok | stale | never | unscheduled | reference
	Due             *time.Time `json:"due"`           // when the next snapshot is due by the schedule
	OverdueAt       *time.Time `json:"overdue_at"`    // from when it counts as stale
	// UploadedBytes sums what the host reported as newly uploaded for the snapshots that still exist:
	// an approximation of the new data this object brought into the repository (not its stored size,
	// which is undefined under deduplication).
	UploadedBytes int64 `json:"uploaded_bytes"`
	// History holds the logical sizes of the newest snapshots (at most 30), oldest first.
	History []SnapPoint `json:"history"`
}

// SnapStat is the newest snapshot of an object.
type SnapStat struct {
	ID           string    `json:"id"`
	Created      time.Time `json:"created"`
	LogicalBytes int64     `json:"logical_bytes"`
	DurationMs   int64     `json:"duration_ms"`
	VerifyState  string    `json:"verify_state"` // "" | ok | failed
}

// SnapPoint is one point of an object's size history.
type SnapPoint struct {
	At           time.Time `json:"at"`
	LogicalBytes int64     `json:"logical_bytes"`
}

// ObjectStats returns every storage object of a repository (optionally of one host) with its freshness.
func (s *Service) ObjectStats(ctx context.Context, repoName, host string) ([]ObjectStat, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	p := s.DB.Pool()
	sched := map[string]string{}
	prows, err := p.Query(ctx, `SELECT name, schedule FROM policies WHERE repository_id=$1`, repo.ID)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var n, sc string
		if err := prows.Scan(&n, &sc); err != nil {
			prows.Close()
			return nil, err
		}
		sched[n] = sc
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, err
	}

	rows, err := p.Query(ctx, `SELECT o.id::text, o.group_id, o.host_name, o.project, o.name, o.kind, o.policy_name, o.retention_policy, o.notes, o.first_seen_at,
			count(sn.id), count(sn.id) FILTER (WHERE sn.protected), count(sn.id) FILTER (WHERE sn.verify_state='failed'),
			max(sn.created_at), COALESCE(sum(sn.uploaded_bytes),0)
		FROM storage_objects o LEFT JOIN snapshots sn ON sn.object_id=o.id
		WHERE o.repository_id=$1 AND ($2='' OR o.host_name=$2)
		GROUP BY o.id ORDER BY o.host_name, o.project, o.name`, repo.ID, host)
	if err != nil {
		return nil, err
	}
	out := []ObjectStat{}
	byID := map[string]int{}
	last := map[string]*time.Time{}
	for rows.Next() {
		var id string
		var o ObjectStat
		var l *time.Time
		if err := rows.Scan(&id, &o.GroupID, &o.Host, &o.Project, &o.Name, &o.Kind, &o.DeclaredPolicy, &o.RetentionPolicy, &o.Notes, &o.FirstSeen,
			&o.Snapshots, &o.Protected, &o.FailedVerify, &l, &o.UploadedBytes); err != nil {
			rows.Close()
			return nil, err
		}
		o.History = []SnapPoint{}
		byID[id] = len(out)
		last[id] = l
		out = append(out, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// the newest 30 snapshots per object: size history, and row 1 is the latest
	hrows, err := p.Query(ctx, `SELECT object_id::text, snapshot_id, created_at, logical_bytes, duration_ms, verify_state, rn FROM (
			SELECT sn.object_id, sn.snapshot_id, sn.created_at, sn.logical_bytes, sn.duration_ms, sn.verify_state,
				row_number() OVER (PARTITION BY sn.object_id ORDER BY sn.created_at DESC, sn.snapshot_id DESC) rn
			FROM snapshots sn JOIN storage_objects o ON o.id=sn.object_id
			WHERE sn.repository_id=$1 AND ($2='' OR o.host_name=$2)) x
		WHERE rn <= 30 ORDER BY object_id, created_at, snapshot_id`, repo.ID, host)
	if err != nil {
		return nil, err
	}
	for hrows.Next() {
		var oid string
		var st SnapStat
		var rn int
		if err := hrows.Scan(&oid, &st.ID, &st.Created, &st.LogicalBytes, &st.DurationMs, &st.VerifyState, &rn); err != nil {
			hrows.Close()
			return nil, err
		}
		i, ok := byID[oid]
		if !ok {
			continue
		}
		out[i].History = append(out[i].History, SnapPoint{At: st.Created, LogicalBytes: st.LogicalBytes})
		if rn == 1 {
			x := st
			out[i].Latest = &x
		}
	}
	hrows.Close()
	if err := hrows.Err(); err != nil {
		return nil, err
	}

	for id, i := range byID {
		o := &out[i]
		if o.Kind == "instance" {
			o.Freshness = FreshReference
			continue
		}
		// the same policy choice as the overview: the retention authority first, then the declared one
		sc, ok := sched[o.RetentionPolicy]
		o.Policy = o.RetentionPolicy
		if !ok {
			sc, ok = sched[o.DeclaredPolicy]
			o.Policy = o.DeclaredPolicy
		}
		if !ok {
			o.Policy = ""
		}
		o.Schedule = sc
		state, due, overdue := Freshness(sc, ok, last[id], now)
		o.Freshness = state
		if !due.IsZero() {
			o.Due, o.OverdueAt = &due, &overdue
		}
	}
	return out, nil
}
