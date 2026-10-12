package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/schedule"
)

func intOrNil(v int) *int {
	if v <= 0 {
		return nil
	}
	return &v
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// SetPolicy creates or replaces a policy by name.
func (s *Service) SetPolicy(ctx context.Context, repoName string, p api.Policy) error {
	if p.Name == "" {
		return bad("policy needs a name")
	}
	if p.CommitRequired == "" {
		p.CommitRequired = "all"
	}
	if p.CommitRequired != "all" && p.CommitRequired != "any" {
		return bad("commit_required must be all or any")
	}
	if p.Schedule == "" {
		p.Schedule = "03:00"
	}
	// a schedule the client cannot parse would silently stop every backup of the policy
	for name, spec := range map[string]string{"schedule": p.Schedule, "verify schedule": p.VerifySchedule, "restore test schedule": p.RestoreTestSchedule} {
		if spec == "" {
			continue
		}
		if _, err := schedule.Parse(spec); err != nil {
			return bad("%s: %v", name, err)
		}
	}
	if p.VerifySchedule == "" {
		p.VerifySchedule = "weekly"
	}
	if p.RestoreTestSchedule == "" {
		p.RestoreTestSchedule = "monthly"
	}
	if p.Targets == nil {
		p.Targets = []string{}
	}
	if p.Timezone == "" {
		p.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return bad("unknown time zone %q (use an IANA name such as Europe/Berlin or Atlantic/Reykjavik)", p.Timezone)
	}
	if p.ReverifyDays < 0 {
		return bad("reverify_days must not be negative")
	}
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	_, err = s.DB.Pool().Exec(ctx, `INSERT INTO policies
		(repository_id, name, schedule, targets, commit_required, keep_last, keep_hourly, keep_daily, keep_weekly, keep_monthly, keep_yearly, verify_schedule, restore_test_schedule, timezone, reverify_days)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (repository_id, name) DO UPDATE SET schedule=EXCLUDED.schedule, targets=EXCLUDED.targets,
		 commit_required=EXCLUDED.commit_required, keep_last=EXCLUDED.keep_last, keep_hourly=EXCLUDED.keep_hourly,
		 keep_daily=EXCLUDED.keep_daily, keep_weekly=EXCLUDED.keep_weekly, keep_monthly=EXCLUDED.keep_monthly,
		 keep_yearly=EXCLUDED.keep_yearly, verify_schedule=EXCLUDED.verify_schedule, restore_test_schedule=EXCLUDED.restore_test_schedule,
		 timezone=EXCLUDED.timezone, reverify_days=EXCLUDED.reverify_days`,
		repo.ID, p.Name, p.Schedule, p.Targets, p.CommitRequired, intOrNil(p.KeepLast), intOrNil(p.KeepHourly), intOrNil(p.KeepDaily),
		intOrNil(p.KeepWeekly), intOrNil(p.KeepMonthly), intOrNil(p.KeepYearly), p.VerifySchedule, p.RestoreTestSchedule, p.Timezone, p.ReverifyDays)
	return err
}

func scanPolicy(r pgx.Row) (api.Policy, error) {
	var p api.Policy
	var kl, kh, kd, kw, km, ky *int
	err := r.Scan(&p.Name, &p.Schedule, &p.Targets, &p.CommitRequired, &kl, &kh, &kd, &kw, &km, &ky, &p.VerifySchedule, &p.RestoreTestSchedule, &p.Timezone, &p.ReverifyDays)
	p.KeepLast, p.KeepHourly, p.KeepDaily, p.KeepWeekly, p.KeepMonthly, p.KeepYearly =
		derefInt(kl), derefInt(kh), derefInt(kd), derefInt(kw), derefInt(km), derefInt(ky)
	return p, err
}

const policyCols = `name, schedule, targets, commit_required, keep_last, keep_hourly, keep_daily, keep_weekly, keep_monthly, keep_yearly, verify_schedule, restore_test_schedule, timezone, reverify_days`

// Policies lists the policies of a repository by repository id.
func (s *Service) policiesByRepoID(ctx context.Context, repoID string) ([]api.Policy, error) {
	rows, err := s.DB.Pool().Query(ctx, `SELECT `+policyCols+` FROM policies WHERE repository_id=$1 ORDER BY name`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Policy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Policies lists the policies of a repository.
func (s *Service) Policies(ctx context.Context, repoName string) ([]api.Policy, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	return s.policiesByRepoID(ctx, repo.ID)
}

// DeletePolicy removes a policy and its assignments.
func (s *Service) DeletePolicy(ctx context.Context, repoName, name string) error {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	ct, err := s.DB.Pool().Exec(ctx, `DELETE FROM policies WHERE repository_id=$1 AND name=$2`, repo.ID, name)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: policy %q", ErrNotFound, name)
	}
	return nil
}

// Assign assigns a policy to a scope of a host: project "" = whole host,
// object "" = whole project.
func (s *Service) Assign(ctx context.Context, repoName, hostName, project, object, policyName string) error {
	if project == "" && object != "" {
		return bad("an object assignment needs a project")
	}
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	var hostID, policyID string
	if err := s.DB.Pool().QueryRow(ctx, `SELECT id FROM hosts WHERE repository_id=$1 AND name=$2 AND scope='backup'`, repo.ID, hostName).Scan(&hostID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: backup host %q (enrol it first)", ErrNotFound, hostName)
		}
		return err
	}
	if err := s.DB.Pool().QueryRow(ctx, `SELECT id FROM policies WHERE repository_id=$1 AND name=$2`, repo.ID, policyName).Scan(&policyID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: policy %q", ErrNotFound, policyName)
		}
		return err
	}
	_, err = s.DB.Pool().Exec(ctx, `INSERT INTO policy_assignments (host_id, project, object, policy_id) VALUES ($1,$2,$3,$4)
		ON CONFLICT (host_id, project, object) DO UPDATE SET policy_id=EXCLUDED.policy_id`, hostID, project, object, policyID)
	return err
}

// Unassign removes an assignment.
func (s *Service) Unassign(ctx context.Context, repoName, hostName, project, object string) error {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	ct, err := s.DB.Pool().Exec(ctx, `DELETE FROM policy_assignments a USING hosts h
		WHERE a.host_id=h.id AND h.repository_id=$1 AND h.name=$2 AND h.scope='backup' AND a.project=$3 AND a.object=$4`,
		repo.ID, hostName, project, object)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: assignment", ErrNotFound)
	}
	return nil
}

// AssignmentRow is an assignment with its host name.
type AssignmentRow struct {
	Host    string `json:"host"`
	Project string `json:"project"`
	Object  string `json:"object"`
	Policy  string `json:"policy"`
}

// Assignments lists all assignments of a repository.
func (s *Service) Assignments(ctx context.Context, repoName string) ([]AssignmentRow, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool().Query(ctx, `SELECT h.name, a.project, a.object, p.name FROM policy_assignments a
		JOIN hosts h ON h.id=a.host_id JOIN policies p ON p.id=a.policy_id
		WHERE h.repository_id=$1 ORDER BY h.name, a.project, a.object`, repo.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssignmentRow
	for rows.Next() {
		var a AssignmentRow
		if err := rows.Scan(&a.Host, &a.Project, &a.Object, &a.Policy); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DesiredState returns what this server wants from a host.
func (s *Service) DesiredState(ctx context.Context, h *Host) (*api.DesiredState, error) {
	if h.Scope != api.ScopeBackup {
		return nil, ErrForbidden
	}
	pols, err := s.policiesByRepoID(ctx, h.RepoID)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool().Query(ctx, `SELECT a.project, a.object, p.name FROM policy_assignments a JOIN policies p ON p.id=a.policy_id
		WHERE a.host_id=$1 ORDER BY a.project, a.object`, h.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ds := &api.DesiredState{Policies: pols, Assignments: []api.Assignment{}}
	for rows.Next() {
		var a api.Assignment
		if err := rows.Scan(&a.Project, &a.Object, &a.Policy); err != nil {
			return nil, err
		}
		ds.Assignments = append(ds.Assignments, a)
	}
	return ds, rows.Err()
}

// ResolvePolicy applies the server-side precedence for one object: an object
// assignment beats a project assignment beats a host assignment. Returns ""
// when nothing matches.
func ResolvePolicy(as []api.Assignment, project, object string) string {
	best, rank := "", -1
	for _, a := range as {
		r := -1
		switch {
		case a.Project == "" && a.Object == "":
			r = 0
		case a.Project == project && a.Object == "":
			r = 1
		case a.Project == project && a.Object == object:
			r = 2
		}
		if r > rank {
			best, rank = a.Policy, r
		}
	}
	return best
}

// UpsertInventory records the storage objects a host reports, including who
// shares them, and keeps the name <-> group-id mapping the UI needs.
func (s *Service) UpsertInventory(ctx context.Context, h *Host, inv api.Inventory) error {
	if h.Scope != api.ScopeBackup {
		return ErrForbidden
	}
	tx, err := s.DB.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, o := range inv.Objects {
		if len(o.GroupID) != 32 || o.Name == "" {
			return bad("invalid inventory entry")
		}
		switch o.Kind {
		case "block", "filesystem", "instance":
		default:
			return bad("invalid object kind %q", o.Kind)
		}
		shared := o.SharedBy
		if shared == nil {
			shared = []string{}
		}
		if err := claimObject(ctx, tx, h, o.GroupID, o.Project, o.Name, o.Kind, o.Policy, shared, s.Now()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// execer is the part of pgx.Tx / pool used by claimObject.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// claimObject records that host h backs up the object with this group id. The identity of an object
// (its owner host, project, name, kind and retention policy) is PINNED when it is first seen:
//
//   - the same host may refresh what it declares (policy name, who shares it) but never rename it;
//   - another backup-scope host gets 403: it cannot take over an object (and with it the assignments
//     that decide its retention) by opening a session on a group id it merely learned from the object list;
//   - an object with no owner yet (created by sync or rebuilt from the datastore after a disaster) is
//     claimed by the first host that announces it;
//   - an administrator moves an object to another host explicitly (`object transfer`).
//
// The retention policy is pinned from the policy the host declares at first sight; it is never changed here.
func claimObject(ctx context.Context, q execer, h *Host, group, project, name, kind, policy string, shared []string, now time.Time) error {
	// first/last seen come from the service clock (as every other timestamp the server judges by), not
	// PostgreSQL's now(): a host's clock-driven tests and the dev demo's simulated history rely on it
	if _, err := q.Exec(ctx, `INSERT INTO storage_objects (repository_id, host_id, host_name, project, name, kind, group_id, policy_name, retention_policy, shared_by, first_seen_at, last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,$9,$10,$10) ON CONFLICT (repository_id, group_id) DO NOTHING`,
		h.RepoID, h.ID, h.Name, project, name, kind, group, policy, shared, now); err != nil {
		return err
	}
	// an unowned object (sync, reconcile) is claimed by the first announcing host
	if _, err := q.Exec(ctx, `UPDATE storage_objects SET host_id=$3, host_name=$4, project=$5, name=$6, kind=$7,
		retention_policy=CASE WHEN retention_policy='' THEN $8 ELSE retention_policy END
		WHERE repository_id=$1 AND group_id=$2 AND host_id IS NULL`, h.RepoID, group, h.ID, h.Name, project, name, kind, policy); err != nil {
		return err
	}
	var owner *string
	if err := q.QueryRow(ctx, `SELECT host_id::text FROM storage_objects WHERE repository_id=$1 AND group_id=$2`, h.RepoID, group).Scan(&owner); err != nil {
		return err
	}
	if owner == nil || *owner != h.ID {
		return fmt.Errorf("%w: this object belongs to another host (an administrator can transfer it: `object transfer`)", ErrForbidden)
	}
	_, err := q.Exec(ctx, `UPDATE storage_objects SET policy_name=$3, shared_by=$4, last_seen_at=$5,
		retention_policy=CASE WHEN retention_policy='' THEN $3 ELSE retention_policy END
		WHERE repository_id=$1 AND group_id=$2`, h.RepoID, group, policy, shared, now)
	return err
}

// TransferObject hands an object to another enrolled backup host (administrator only), e.g. after an
// instance migrated between hosts. Its snapshots and pinned retention stay.
func (s *Service) TransferObject(ctx context.Context, repoName, group, toHost string) error {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	var hid string
	if err := s.DB.Pool().QueryRow(ctx, `SELECT id::text FROM hosts WHERE repository_id=$1 AND name=$2 AND scope='backup'`, repo.ID, toHost).Scan(&hid); err != nil {
		return fmt.Errorf("%w: backup host %q", ErrNotFound, toHost)
	}
	ct, err := s.DB.Pool().Exec(ctx, `UPDATE storage_objects SET host_id=$3::uuid, host_name=$4 WHERE repository_id=$1 AND group_id=$2`, repo.ID, group, hid, toHost)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: object", ErrNotFound)
	}
	return nil
}
