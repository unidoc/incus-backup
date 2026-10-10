package service

import (
	"context"
	"sort"
	"time"
)

// Overview is the read model of the dashboard: everything an operator wants to see at a glance, across
// every repository, computed with a handful of aggregate queries (it must stay cheap with thousands of
// snapshots).
type Overview struct {
	GeneratedAt  time.Time      `json:"generated_at"`
	Repositories []RepoOverview `json:"repositories"`
	Hosts        []HostOverview `json:"hosts"`
	Protection   Protection     `json:"protection"`
	Runs24h      RunCounts      `json:"runs_24h"`
	Problems     []Problem      `json:"problems"`
}

// RepoOverview is one repository's health line.
type RepoOverview struct {
	Name          string     `json:"name"`
	Backend       string     `json:"backend"`
	Mode          string     `json:"mode"`
	Hosts         int        `json:"hosts"`
	Objects       int        `json:"objects"`
	Snapshots     int        `json:"snapshots"`
	LogicalBytes  int64      `json:"logical_bytes"`  // last capacity sample
	PhysicalBytes int64      `json:"physical_bytes"` // last capacity sample
	DedupRatio    float64    `json:"dedup_ratio"`
	FSTotal       int64      `json:"fs_total"`
	FSFree        int64      `json:"fs_free"`
	Chunks        int64      `json:"chunks"`
	CorruptChunks int        `json:"corrupt_chunks"`
	FailedVerify  int        `json:"failed_verify_snapshots"`
	LastBackup    *time.Time `json:"last_backup"`
	LastVerify    *time.Time `json:"last_verify"`
	LastGC        *time.Time `json:"last_gc"`
	LastRetention *time.Time `json:"last_retention"`
	SyncJobs      int        `json:"sync_jobs"`
	SyncFailing   int        `json:"sync_failing"`
}

// HostOverview is one enrolled backup host.
type HostOverview struct {
	Repository  string     `json:"repository"`
	Name        string     `json:"name"`
	Disabled    bool       `json:"disabled"`
	LastSeen    *time.Time `json:"last_seen"`
	Online      bool       `json:"online"`
	Instances   int        `json:"instances"` // storage objects of kind "instance"
	Disks       int        `json:"disks"`     // block objects
	Filesystems int        `json:"filesystems"`
	Objects     int        `json:"objects"`
	Stale       int        `json:"stale"` // objects overdue by their policy schedule
	NeverBackup int        `json:"never_backed_up"`
	LastBackup  *time.Time `json:"last_backup"`
	LastRunOK   *string    `json:"last_run_status"`
}

// Protection summarises how many objects are covered.
type Protection struct {
	Objects     int `json:"objects"`
	Protected   int `json:"protected"` // has a snapshot and is not overdue
	Stale       int `json:"stale"`     // overdue by its policy schedule
	NeverBackup int `json:"never_backed_up"`
	Unscheduled int `json:"unscheduled"` // no policy schedule known: freshness cannot be judged
}

// RunCounts counts backup runs.
type RunCounts struct {
	OK      int `json:"ok"`
	Partial int `json:"partial"`
	Failed  int `json:"failed"`
}

// Problem is something an operator should look at, derived from the same data (the alert engine adds
// history, repeat and acknowledgement on top).
type Problem struct {
	Severity   string `json:"severity"` // warning | critical
	Kind       string `json:"kind"`     // stale_backup | host_offline | corrupt_chunks | verify_failed | sync_failing | no_backup
	Repository string `json:"repository"`
	Subject    string `json:"subject"`
	Detail     string `json:"detail"`
}

// HostOfflineAfter is how long a host may be silent before it counts as offline in the overview.
const HostOfflineAfter = 6 * time.Hour

// OverviewAll builds the dashboard model.
func (s *Service) OverviewAll(ctx context.Context) (*Overview, error) { return s.OverviewFor(ctx, nil) }

// OverviewFor builds the dashboard model over the repositories allow accepts (nil = all); totals are
// computed over those repositories only, so a scoped user learns nothing about the others.
func (s *Service) OverviewFor(ctx context.Context, allow func(repo string) bool) (*Overview, error) {
	now := s.Now()
	p := s.DB.Pool()
	ov := &Overview{GeneratedAt: now, Repositories: []RepoOverview{}, Hosts: []HostOverview{}, Problems: []Problem{}}
	repos, err := s.Repositories(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range repos {
		if allow != nil && !allow(r.Name) {
			continue
		}
		ro := RepoOverview{Name: r.Name, Backend: r.Backend, Mode: r.Mode}
		_ = p.QueryRow(ctx, `SELECT count(*) FROM hosts WHERE repository_id=$1 AND scope='backup'`, r.ID).Scan(&ro.Hosts)
		_ = p.QueryRow(ctx, `SELECT count(*) FROM storage_objects WHERE repository_id=$1`, r.ID).Scan(&ro.Objects)
		_ = p.QueryRow(ctx, `SELECT count(*), max(committed_at), count(*) FILTER (WHERE verify_state='failed') FROM snapshots WHERE repository_id=$1`, r.ID).Scan(&ro.Snapshots, &ro.LastBackup, &ro.FailedVerify)
		_ = p.QueryRow(ctx, `SELECT physical_bytes, logical_bytes, chunk_count, fs_total_bytes, fs_free_bytes FROM capacity_samples WHERE repository_id=$1 ORDER BY taken_at DESC LIMIT 1`, r.ID).
			Scan(&ro.PhysicalBytes, &ro.LogicalBytes, &ro.Chunks, &ro.FSTotal, &ro.FSFree)
		if ro.PhysicalBytes > 0 {
			ro.DedupRatio = float64(ro.LogicalBytes) / float64(ro.PhysicalBytes)
		}
		_ = p.QueryRow(ctx, `SELECT count(*) FROM corrupt_chunks WHERE repository_id=$1 AND state <> 'healed'`, r.ID).Scan(&ro.CorruptChunks)
		for kind, dst := range map[string]**time.Time{"verify": &ro.LastVerify, "gc": &ro.LastGC, "retention": &ro.LastRetention} {
			_ = p.QueryRow(ctx, `SELECT max(finished_at) FROM maintenance_runs WHERE repository_id=$1 AND kind=$2 AND status='ok'`, r.ID, kind).Scan(dst)
		}
		_ = p.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE (SELECT status FROM sync_runs x WHERE x.job_id=j.id ORDER BY started_at DESC LIMIT 1) IN ('failed','partial')) FROM sync_jobs j WHERE repository_id=$1`, r.ID).Scan(&ro.SyncJobs, &ro.SyncFailing)
		var ok, part, fail int
		_ = p.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='ok'), count(*) FILTER (WHERE status='partial'), count(*) FILTER (WHERE status='failed')
			FROM backup_runs WHERE repository_id=$1 AND started_at > $2`, r.ID, now.Add(-24*time.Hour)).Scan(&ok, &part, &fail)
		ov.Runs24h.OK, ov.Runs24h.Partial, ov.Runs24h.Failed = ov.Runs24h.OK+ok, ov.Runs24h.Partial+part, ov.Runs24h.Failed+fail
		ov.Repositories = append(ov.Repositories, ro)

		if ro.CorruptChunks > 0 {
			ov.Problems = append(ov.Problems, Problem{"critical", "corrupt_chunks", r.Name, r.Name, "quarantined chunks need healing (a host holding the data must re-upload them)"})
		}
		if ro.FailedVerify > 0 {
			ov.Problems = append(ov.Problems, Problem{"critical", "verify_failed", r.Name, r.Name, "snapshots failed verification"})
		}
		if ro.SyncFailing > 0 {
			ov.Problems = append(ov.Problems, Problem{"warning", "sync_failing", r.Name, r.Name, "a sync job's last run failed"})
		}
		if err := s.overviewObjects(ctx, r, now, ov); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(ov.Problems, func(i, j int) bool {
		return ov.Problems[i].Severity == "critical" && ov.Problems[j].Severity != "critical"
	})
	return ov, nil
}

func (s *Service) overviewObjects(ctx context.Context, r Repo, now time.Time, ov *Overview) error {
	p := s.DB.Pool()
	// the policy a schedule judgement uses: the retention authority (admin assignment overrides are
	// resolved by the host's own scheduler; the overview uses what the server knows)
	pol := map[string]string{}
	prows, err := p.Query(ctx, `SELECT name, schedule FROM policies WHERE repository_id=$1`, r.ID)
	if err != nil {
		return err
	}
	for prows.Next() {
		var n, sc string
		if err := prows.Scan(&n, &sc); err != nil {
			prows.Close()
			return err
		}
		pol[n] = sc
	}
	prows.Close()

	rows, err := p.Query(ctx, `SELECT h.name, h.disabled, h.last_seen_at,
			(SELECT status FROM backup_runs b WHERE b.host_id=h.id AND b.finished_at IS NOT NULL ORDER BY b.started_at DESC LIMIT 1)
		FROM hosts h WHERE h.repository_id=$1 AND h.scope='backup' ORDER BY h.name`, r.ID)
	if err != nil {
		return err
	}
	hosts := map[string]*HostOverview{}
	var order []string
	for rows.Next() {
		h := &HostOverview{Repository: r.Name}
		if err := rows.Scan(&h.Name, &h.Disabled, &h.LastSeen, &h.LastRunOK); err != nil {
			rows.Close()
			return err
		}
		h.Online = h.LastSeen != nil && now.Sub(*h.LastSeen) < HostOfflineAfter && !h.Disabled
		hosts[h.Name] = h
		order = append(order, h.Name)
	}
	rows.Close()

	orows, err := p.Query(ctx, `SELECT o.host_name, o.project, o.name, o.kind, o.policy_name, o.retention_policy, o.first_seen_at,
			(SELECT max(created_at) FROM snapshots sn WHERE sn.object_id=o.id)
		FROM storage_objects o WHERE o.repository_id=$1`, r.ID)
	if err != nil {
		return err
	}
	defer orows.Close()
	for orows.Next() {
		var host, project, name, kind, polName, retPol string
		var first time.Time
		var last *time.Time
		if err := orows.Scan(&host, &project, &name, &kind, &polName, &retPol, &first, &last); err != nil {
			return err
		}
		h := hosts[host]
		ov.Protection.Objects++
		if h != nil {
			h.Objects++
			switch kind {
			case "instance":
				h.Instances++
			case "block":
				h.Disks++
			case "filesystem":
				h.Filesystems++
			}
			if last != nil && (h.LastBackup == nil || last.After(*h.LastBackup)) {
				h.LastBackup = last
			}
		}
		if kind == "instance" { // an instance object is a reference record; its disks carry the data
			ov.Protection.Objects--
			continue
		}
		subject := host + "/" + project + "/" + name
		if last == nil {
			ov.Protection.NeverBackup++
			if h != nil {
				h.NeverBackup++
			}
			if now.Sub(first) > 24*time.Hour {
				ov.Problems = append(ov.Problems, Problem{"warning", "no_backup", r.Name, subject, "no snapshot exists yet"})
			}
			continue
		}
		sc, ok := pol[retPol]
		if !ok {
			sc, ok = pol[polName]
		}
		state, _, _ := Freshness(sc, ok, last, now) // the one rule (infra_views.go), shared with GET /objects/stats
		if state == FreshUnscheduled {
			ov.Protection.Unscheduled++
			ov.Protection.Protected++
			continue
		}
		if state == FreshStale {
			ov.Protection.Stale++
			if h != nil {
				h.Stale++
			}
			ov.Problems = append(ov.Problems, Problem{"warning", "stale_backup", r.Name, subject, "last snapshot " + last.UTC().Format(time.RFC3339) + " is older than its schedule allows"})
			continue
		}
		ov.Protection.Protected++
	}
	if err := orows.Err(); err != nil {
		return err
	}
	for _, n := range order {
		h := hosts[n]
		ov.Hosts = append(ov.Hosts, *h)
		if !h.Disabled && !h.Online {
			ov.Problems = append(ov.Problems, Problem{"warning", "host_offline", r.Name, h.Name, "not seen for more than 6 hours"})
		}
	}
	return nil
}

// CapacityPoint is one sample for charts.
type CapacityPoint struct {
	At       time.Time `json:"at"`
	Physical int64     `json:"physical_bytes"`
	Logical  int64     `json:"logical_bytes"`
	Free     int64     `json:"fs_free"`
	Total    int64     `json:"fs_total"`
	Chunks   int64     `json:"chunks"`
}

// CapacitySeries returns the samples of the last days, thinned to at most ~400 points.
func (s *Service) CapacitySeries(ctx context.Context, repoName string, days int) ([]CapacityPoint, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	if days <= 0 || days > 730 {
		days = 30
	}
	rows, err := s.DB.Pool().Query(ctx, `SELECT taken_at, physical_bytes, logical_bytes, fs_free_bytes, fs_total_bytes, chunk_count
		FROM capacity_samples WHERE repository_id=$1 AND taken_at > $2 ORDER BY taken_at`, repo.ID, s.Now().AddDate(0, 0, -days))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []CapacityPoint
	for rows.Next() {
		var c CapacityPoint
		if err := rows.Scan(&c.At, &c.Physical, &c.Logical, &c.Free, &c.Total, &c.Chunks); err != nil {
			return nil, err
		}
		all = append(all, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(all) <= 400 {
		return append([]CapacityPoint{}, all...), nil
	}
	out := make([]CapacityPoint, 0, 400)
	step := float64(len(all)-1) / 399
	for i := 0; i < 400; i++ {
		out = append(out, all[int(float64(i)*step)])
	}
	return out, nil
}
