package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/schedule"
	"github.com/unidoc/incus-backup/internal/server/service"
)

type evaluator func(ctx context.Context, c condCfg, now time.Time) ([]observation, error)

// evaluate runs every enabled condition. A condition that fails to evaluate is reported and left
// out of `evaluated`, so its open alerts are neither resolved nor refreshed (no false "resolved").
func (e *Engine) evaluate(ctx context.Context, conds map[string]condCfg, now time.Time) ([]observation, map[string]bool, []string) {
	evs := []struct {
		typ string
		f   evaluator
	}{
		{TypeBackupFailed, e.evalBackupFailed},
		{TypeBackupMissed, e.evalBackupMissed},
		{TypeHostOffline, e.evalHostOffline},
		{TypeMaintenanceFailed, e.evalMaintenanceFailed},
		{TypeCorruption, e.evalCorruption},
		{TypeSyncFailed, e.evalSyncFailed},
		{TypeSyncLag, e.evalSyncLag},
		{TypeCapacityWarning, e.evalCapacity},
		{TypeRepositoryUnreachable, e.evalUnreachable},
		{TypeCertExpiry, e.evalCerts},
		{TypeRestoreTestFailed, e.evalRestoreTests},
		{TypeUpdateAvailable, e.evalUpdate},
	}
	var obs []observation
	evaluated := map[string]bool{}
	var errs []string
	for _, ev := range evs {
		c, ok := conds[ev.typ]
		if !ok || !c.enabled {
			evaluated[ev.typ] = true // disabled: its open alerts end (silently)
			continue
		}
		if ev.typ == TypeCertExpiry && e.CertStatus == nil || ev.typ == TypeRestoreTestFailed && e.RestoreTests == nil || ev.typ == TypeUpdateAvailable && e.UpdateStatus == nil {
			continue // not wired: leave whatever exists untouched
		}
		o, err := ev.f(ctx, c, now)
		if err != nil {
			if ctx.Err() != nil {
				errs = append(errs, ev.typ+": "+ctx.Err().Error())
				continue
			}
			errs = append(errs, ev.typ+": "+err.Error())
			continue
		}
		evaluated[ev.typ] = true
		for i := range o {
			o[i].Type = ev.typ
			if o[i].Severity == "" {
				o[i].Severity = c.severity
			}
			o[i].Title = clean(o[i].Title, 200)
			o[i].Detail = cleanBlock(o[i].Detail, 4000)
			o[i].Host, o[i].Repo, o[i].Object = clean(o[i].Host, 200), clean(o[i].Repo, 200), clean(o[i].Object, 300)
		}
		obs = append(obs, o...)
	}
	return obs, evaluated, errs
}

// displayObject is how people name an object: "bosch" (default project) or "web/bosch".
func displayObject(project, name string) string {
	n := name
	switch {
	case strings.HasPrefix(n, "instance/"):
		n = strings.TrimSuffix(strings.TrimPrefix(n, "instance/"), "/root")
	case strings.HasPrefix(n, "volume/"):
		n = strings.TrimPrefix(n, "volume/")
	}
	if project != "" && project != "default" {
		return project + "/" + n
	}
	return n
}

// ownerName mirrors service.ownerName: the assignment name of an object.
func ownerName(name string) string {
	switch {
	case strings.HasPrefix(name, "instance/"):
		return strings.TrimSuffix(strings.TrimPrefix(name, "instance/"), "/root")
	case strings.HasPrefix(name, "volume/"):
		return strings.TrimPrefix(name, "volume/")
	}
	return name
}

func collect[T any](rows pgx.Rows, scan func(pgx.Rows) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---- backup_failed ------------------------------------------------------------------------

func (e *Engine) evalBackupFailed(ctx context.Context, c condCfg, now time.Time) ([]observation, error) {
	rows, err := e.Svc.DB.Pool().Query(ctx, `SELECT DISTINCT ON (br.host_id) r.name, h.name, br.status, br.error, br.finished_at, br.objects_failed, br.objects_total
		FROM backup_runs br JOIN hosts h ON h.id=br.host_id JOIN repositories r ON r.id=br.repository_id
		WHERE br.finished_at IS NOT NULL AND br.status <> 'deferred' AND NOT h.disabled ORDER BY br.host_id, br.started_at DESC, br.finished_at DESC`)
	if err != nil {
		return nil, err
	}
	type row struct {
		repo, host, status, errText string
		at                          time.Time
		failed, total               int
	}
	rs, err := collect(rows, func(r pgx.Rows) (row, error) {
		var x row
		return x, r.Scan(&x.repo, &x.host, &x.status, &x.errText, &x.at, &x.failed, &x.total)
	})
	if err != nil {
		return nil, err
	}
	var out []observation
	for _, x := range rs {
		if x.status != "failed" && x.status != "partial" {
			continue
		}
		o := observation{Key: "backup_failed:" + x.repo + "/" + x.host, Host: x.host, Repo: x.repo}
		if x.status == "failed" {
			o.Severity = c.str("failed_severity", SeverityCritical)
			o.Title = x.host + ": backup run failed"
		} else {
			o.Title = fmt.Sprintf("%s: backup run partly failed (%d of %d objects)", x.host, x.failed, x.total)
		}
		o.Detail = fmt.Sprintf("The last backup run of host %s (repository %s) finished %s with status %s.", x.host, x.repo, x.at.UTC().Format(time.RFC3339), x.status)
		if x.errText != "" {
			o.Detail += "\nError: " + x.errText
		}
		out = append(out, o)
	}
	return out, nil
}

// ---- backup_missed ------------------------------------------------------------------------

// scheduleInterval is the longest gap between consecutive firings (the "every N" grid restarts at
// midnight, monthly months differ): the conservative interval to judge lateness by.
func scheduleInterval(spec string, now time.Time) (time.Duration, bool) {
	sp, err := schedule.Parse(spec)
	if err != nil || sp.Manual() {
		return 0, false
	}
	t := sp.Next(now.UTC())
	if t.IsZero() {
		return 0, false
	}
	var max time.Duration
	for i := 0; i < 4; i++ {
		n := sp.Next(t)
		if n.IsZero() {
			break
		}
		if d := n.Sub(t); d > max {
			max = d
		}
		t = n
	}
	return max, max > 0
}

func (e *Engine) evalBackupMissed(ctx context.Context, c condCfg, now time.Time) ([]observation, error) {
	obs, _, err := e.backupMissed(ctx, c, now)
	return obs, err
}

// backupMissed also returns how many objects carry a backup promise (owned, current, scheduled policy).
func (e *Engine) backupMissed(ctx context.Context, c condCfg, now time.Time) ([]observation, int, error) {
	p := e.Svc.DB.Pool()
	factor := c.num("interval_factor", 1.5)
	grace := time.Duration(c.num("grace_minutes", 60)) * time.Minute
	critFactor := c.num("critical_factor", 3)
	stale := time.Duration(c.num("stale_object_days", 7) * float64(24*time.Hour))
	// policies per repository
	prow, err := p.Query(ctx, `SELECT repository_id::text, name, schedule FROM policies`)
	if err != nil {
		return nil, 0, err
	}
	type pol struct{ repo, name, sched string }
	pols, err := collect(prow, func(r pgx.Rows) (pol, error) { var x pol; return x, r.Scan(&x.repo, &x.name, &x.sched) })
	if err != nil {
		return nil, 0, err
	}
	schedOf := map[string]string{}
	for _, x := range pols {
		schedOf[x.repo+"/"+x.name] = x.sched
	}
	arow, err := p.Query(ctx, `SELECT a.host_id::text, a.project, a.object, p.name FROM policy_assignments a JOIN policies p ON p.id=a.policy_id`)
	if err != nil {
		return nil, 0, err
	}
	type asg struct {
		host string
		a    api.Assignment
	}
	as, err := collect(arow, func(r pgx.Rows) (asg, error) {
		var x asg
		return x, r.Scan(&x.host, &x.a.Project, &x.a.Object, &x.a.Policy)
	})
	if err != nil {
		return nil, 0, err
	}
	byHost := map[string][]api.Assignment{}
	for _, x := range as {
		byHost[x.host] = append(byHost[x.host], x.a)
	}
	// Objects without an owner host (sync destinations, rebuilt from the datastore) are not
	// backed up here; sync_lag covers replicas. Objects a host stopped reporting for stale days
	// (instance deleted) are retired from this check.
	orow, err := p.Query(ctx, `SELECT o.repository_id::text, r.name, o.group_id, o.host_id::text, o.host_name, o.project, o.name, o.retention_policy, o.first_seen_at,
		(SELECT max(s.committed_at) FROM snapshots s WHERE s.repository_id=o.repository_id AND s.group_id=o.group_id)
		FROM storage_objects o JOIN repositories r ON r.id=o.repository_id JOIN hosts h ON h.id=o.host_id
		WHERE NOT h.disabled AND o.last_seen_at >= $1`, now.Add(-stale))
	if err != nil {
		return nil, 0, err
	}
	type obj struct {
		repoID, repo, group, hostID, host, project, name, pinned string
		first                                                    time.Time
		last                                                     *time.Time
	}
	objs, err := collect(orow, func(r pgx.Rows) (obj, error) {
		var x obj
		return x, r.Scan(&x.repoID, &x.repo, &x.group, &x.hostID, &x.host, &x.project, &x.name, &x.pinned, &x.first, &x.last)
	})
	if err != nil {
		return nil, 0, err
	}
	var out []observation
	eligible := 0
	for _, o := range objs {
		polName := service.ResolvePolicy(byHost[o.hostID], o.project, ownerName(o.name))
		if polName == "" {
			polName = o.pinned
		}
		spec, ok := schedOf[o.repoID+"/"+polName]
		if !ok {
			continue // no known policy: nothing promised
		}
		iv, ok := scheduleInterval(spec, now)
		if !ok {
			continue // manual schedule
		}
		eligible++
		threshold := time.Duration(factor*float64(iv)) + grace
		base := o.first
		if o.last != nil && o.last.After(base) {
			base = *o.last
		}
		age := now.Sub(base)
		if age <= threshold {
			continue
		}
		disp := displayObject(o.project, o.name)
		ob := observation{Key: "backup_missed:" + o.repo + "/" + o.group, Host: o.host, Repo: o.repo, Object: o.project + "/" + ownerName(o.name)}
		if age > time.Duration(critFactor*float64(iv))+grace {
			ob.Severity = SeverityCritical
		}
		if o.last != nil {
			ob.Title = fmt.Sprintf("%s: no successful backup for %s", disp, human(age))
			ob.Detail = fmt.Sprintf("The last successful backup of %s (host %s, policy %s, schedule %q) was committed %s, %s ago. Expected at least every %s (alert after %s).",
				disp, o.host, polName, spec, o.last.UTC().Format(time.RFC3339), human(age), human(iv), human(threshold))
		} else {
			ob.Title = fmt.Sprintf("%s: never backed up (known for %s)", disp, human(age))
			ob.Detail = fmt.Sprintf("%s (host %s, policy %s, schedule %q) was first reported %s and has no snapshot yet.",
				disp, o.host, polName, spec, o.first.UTC().Format(time.RFC3339))
		}
		out = append(out, ob)
	}
	return out, eligible, nil
}

// ---- host_offline -------------------------------------------------------------------------

func (e *Engine) evalHostOffline(ctx context.Context, c condCfg, now time.Time) ([]observation, error) {
	after := time.Duration(c.num("after_minutes", 120)) * time.Minute
	crit := time.Duration(c.num("critical_after_minutes", 1440)) * time.Minute
	rows, err := e.Svc.DB.Pool().Query(ctx, `SELECT r.name, h.name, COALESCE(h.last_seen_at, h.enrolled_at), h.last_seen_at IS NULL
		FROM hosts h JOIN repositories r ON r.id=h.repository_id WHERE h.scope='backup' AND NOT h.disabled`)
	if err != nil {
		return nil, err
	}
	type row struct {
		repo, host string
		seen       time.Time
		never      bool
	}
	rs, err := collect(rows, func(r pgx.Rows) (row, error) { var x row; return x, r.Scan(&x.repo, &x.host, &x.seen, &x.never) })
	if err != nil {
		return nil, err
	}
	var out []observation
	for _, x := range rs {
		age := now.Sub(x.seen)
		if age <= after {
			continue
		}
		o := observation{Key: "host_offline:" + x.repo + "/" + x.host, Host: x.host, Repo: x.repo}
		if crit > 0 && age > crit {
			o.Severity = SeverityCritical
		}
		if x.never {
			o.Title = fmt.Sprintf("%s: never contacted the server since enrolment %s ago", x.host, human(age))
		} else {
			o.Title = fmt.Sprintf("%s: offline for %s", x.host, human(age))
		}
		o.Detail = fmt.Sprintf("Host %s (repository %s) was last seen %s.", x.host, x.repo, x.seen.UTC().Format(time.RFC3339))
		out = append(out, o)
	}
	return out, nil
}

// ---- maintenance_failed -------------------------------------------------------------------

func (e *Engine) evalMaintenanceFailed(ctx context.Context, c condCfg, now time.Time) ([]observation, error) {
	rows, err := e.Svc.DB.Pool().Query(ctx, `SELECT DISTINCT ON (m.repository_id, m.kind) r.name, m.kind, m.status, m.error, COALESCE(m.finished_at, m.started_at)
		FROM maintenance_runs m JOIN repositories r ON r.id=m.repository_id WHERE m.status <> 'running'
		ORDER BY m.repository_id, m.kind, m.started_at DESC`)
	if err != nil {
		return nil, err
	}
	type row struct {
		repo, kind, status, errText string
		at                          time.Time
	}
	rs, err := collect(rows, func(r pgx.Rows) (row, error) {
		var x row
		return x, r.Scan(&x.repo, &x.kind, &x.status, &x.errText, &x.at)
	})
	if err != nil {
		return nil, err
	}
	var out []observation
	for _, x := range rs {
		if x.status != "failed" {
			continue
		}
		o := observation{Key: "maintenance_failed:" + x.repo + "/" + x.kind, Repo: x.repo,
			Title:  fmt.Sprintf("%s: %s failed", x.repo, x.kind),
			Detail: fmt.Sprintf("The last %s run of repository %s failed at %s.", x.kind, x.repo, x.at.UTC().Format(time.RFC3339))}
		if x.errText != "" {
			o.Detail += "\nError: " + x.errText
		}
		out = append(out, o)
	}
	return out, nil
}

// ---- corruption ---------------------------------------------------------------------------

func (e *Engine) evalCorruption(ctx context.Context, c condCfg, now time.Time) ([]observation, error) {
	p := e.Svc.DB.Pool()
	rows, err := p.Query(ctx, `SELECT r.name, s.group_id, COALESCE(o.host_name,''), COALESCE(o.project,''), COALESCE(o.name,''), count(*), max(s.verified_at)
		FROM snapshots s JOIN repositories r ON r.id=s.repository_id
		LEFT JOIN storage_objects o ON o.repository_id=s.repository_id AND o.group_id=s.group_id
		WHERE s.verify_state='failed' GROUP BY r.name, s.group_id, o.host_name, o.project, o.name`)
	if err != nil {
		return nil, err
	}
	type row struct {
		repo, group, host, project, name string
		n                                int
		at                               *time.Time
	}
	rs, err := collect(rows, func(r pgx.Rows) (row, error) {
		var x row
		return x, r.Scan(&x.repo, &x.group, &x.host, &x.project, &x.name, &x.n, &x.at)
	})
	if err != nil {
		return nil, err
	}
	var out []observation
	for _, x := range rs {
		disp := displayObject(x.project, x.name)
		if disp == "" {
			disp = "group " + x.group
		}
		o := observation{Key: "corruption:" + x.repo + "/" + x.group, Host: x.host, Repo: x.repo, Object: x.project + "/" + ownerName(x.name),
			Title:  fmt.Sprintf("%s: %d snapshot(s) failed verification", disp, x.n),
			Detail: fmt.Sprintf("%d snapshot(s) of %s in repository %s failed verification.", x.n, disp, x.repo)}
		if x.project == "" && x.name == "" {
			o.Object = ""
		}
		out = append(out, o)
	}
	crow, err := p.Query(ctx, `SELECT r.name, count(*), min(c.detected_at) FROM corrupt_chunks c JOIN repositories r ON r.id=c.repository_id
		WHERE c.state='quarantined' GROUP BY r.name`)
	if err != nil {
		return nil, err
	}
	type crow_ struct {
		repo string
		n    int
		at   time.Time
	}
	cs, err := collect(crow, func(r pgx.Rows) (crow_, error) { var x crow_; return x, r.Scan(&x.repo, &x.n, &x.at) })
	if err != nil {
		return nil, err
	}
	for _, x := range cs {
		out = append(out, observation{Key: "corrupt_chunks:" + x.repo, Repo: x.repo,
			Title:  fmt.Sprintf("%s: %d corrupt chunk(s) quarantined", x.repo, x.n),
			Detail: fmt.Sprintf("Verification quarantined %d chunk(s) in repository %s (first detected %s). They stay quarantined until a backup or a sync heals them.", x.n, x.repo, x.at.UTC().Format(time.RFC3339))})
	}
	return out, nil
}

// ---- sync ---------------------------------------------------------------------------------

func (e *Engine) evalSyncFailed(ctx context.Context, c condCfg, now time.Time) ([]observation, error) {
	rows, err := e.Svc.DB.Pool().Query(ctx, `SELECT DISTINCT ON (j.id) r.name, j.name, j.remote, sr.status, sr.error, COALESCE(sr.finished_at, sr.started_at)
		FROM sync_runs sr JOIN sync_jobs j ON j.id=sr.job_id JOIN repositories r ON r.id=j.repository_id
		WHERE j.enabled AND sr.status <> 'running' ORDER BY j.id, sr.started_at DESC`)
	if err != nil {
		return nil, err
	}
	type row struct {
		repo, job, remote, status, errText string
		at                                 time.Time
	}
	rs, err := collect(rows, func(r pgx.Rows) (row, error) {
		var x row
		return x, r.Scan(&x.repo, &x.job, &x.remote, &x.status, &x.errText, &x.at)
	})
	if err != nil {
		return nil, err
	}
	var out []observation
	for _, x := range rs {
		if x.status != "failed" && x.status != "partial" {
			continue
		}
		o := observation{Key: "sync_failed:" + x.repo + "/" + x.job, Repo: x.repo,
			Title:  fmt.Sprintf("sync %s (%s): last run %s", x.job, x.repo, x.status),
			Detail: fmt.Sprintf("The last run of sync job %s (repository %s, remote %s) ended %s at %s.", x.job, x.repo, x.remote, x.status, x.at.UTC().Format(time.RFC3339))}
		if x.errText != "" {
			o.Detail += "\nError: " + x.errText
		}
		out = append(out, o)
	}
	return out, nil
}

func (e *Engine) evalSyncLag(ctx context.Context, c condCfg, now time.Time) ([]observation, error) {
	lag := time.Duration(c.num("lag_hours", 48) * float64(time.Hour))
	rows, err := e.Svc.DB.Pool().Query(ctx, `SELECT r.name, j.name, j.schedule, COALESCE(j.last_success_at, j.created_at), j.last_success_at IS NULL
		FROM sync_jobs j JOIN repositories r ON r.id=j.repository_id WHERE j.enabled`)
	if err != nil {
		return nil, err
	}
	type row struct {
		repo, job, sched string
		at               time.Time
		never            bool
	}
	rs, err := collect(rows, func(r pgx.Rows) (row, error) { var x row; return x, r.Scan(&x.repo, &x.job, &x.sched, &x.at, &x.never) })
	if err != nil {
		return nil, err
	}
	var out []observation
	for _, x := range rs {
		if sp, err := schedule.Parse(x.sched); err != nil || sp.Manual() {
			continue // a manual job promises nothing
		}
		age := now.Sub(x.at)
		if age <= lag {
			continue
		}
		o := observation{Key: "sync_lag:" + x.repo + "/" + x.job, Repo: x.repo}
		if x.never {
			o.Title = fmt.Sprintf("sync %s (%s): never succeeded (created %s ago)", x.job, x.repo, human(age))
		} else {
			o.Title = fmt.Sprintf("sync %s (%s): no successful run for %s", x.job, x.repo, human(age))
		}
		o.Detail = fmt.Sprintf("Sync job %s of repository %s last succeeded %s; the threshold is %s.", x.job, x.repo, x.at.UTC().Format(time.RFC3339), human(lag))
		out = append(out, o)
	}
	return out, nil
}

// ---- capacity -----------------------------------------------------------------------------

func (e *Engine) evalCapacity(ctx context.Context, c condCfg, now time.Time) ([]observation, error) {
	repos, err := e.Svc.Repositories(ctx)
	if err != nil {
		return nil, err
	}
	within := time.Duration(c.num("full_within_days", 14) * float64(24*time.Hour))
	crit := time.Duration(c.num("critical_within_days", 3) * float64(24*time.Hour))
	usedPct := c.num("used_percent", 90)
	var out []observation
	for _, r := range repos {
		cap, err := e.Svc.Capacity(ctx, r.Name)
		if errors.Is(err, service.ErrNotFound) {
			continue // no samples yet
		}
		if err != nil {
			return nil, err
		}
		if cap.FSTotal <= 0 {
			continue // object storage: no honest free-space figure
		}
		pct := 100 * float64(cap.FSTotal-cap.FSFree) / float64(cap.FSTotal)
		var reasons []string
		sev := ""
		if usedPct > 0 && pct >= usedPct {
			reasons = append(reasons, fmt.Sprintf("%.0f%% used", pct))
		}
		if cap.ProjectedFull != nil && cap.ProjectedFull.Sub(now) < within {
			left := cap.ProjectedFull.Sub(now)
			reasons = append(reasons, "full in about "+human(left))
			if left < crit {
				sev = SeverityCritical
			}
		}
		if cap.FSFree <= 0 {
			sev = SeverityCritical
			reasons = append(reasons, "no free space")
		}
		if len(reasons) == 0 {
			continue
		}
		o := observation{Key: "capacity:" + r.Name, Repo: r.Name, Severity: sev,
			Title:  fmt.Sprintf("%s: storage %s", r.Name, strings.Join(reasons, ", ")),
			Detail: fmt.Sprintf("Repository %s: %d of %d bytes free (%.1f%% used).", r.Name, cap.FSFree, cap.FSTotal, pct)}
		if cap.ProjectedFull != nil {
			o.Detail += fmt.Sprintf(" At the current growth rate the filesystem is full around %s.", cap.ProjectedFull.UTC().Format("2006-01-02"))
		}
		out = append(out, o)
	}
	return out, nil
}

// ---- repository_unreachable ---------------------------------------------------------------

// probeRepositories probes the S3 repositories that are due (probe_minutes, service clock), each
// bounded by ProbeTimeout, and records the result.
func (e *Engine) probeRepositories(ctx context.Context, c condCfg, now time.Time, paused map[string]bool) {
	if !c.enabled {
		return
	}
	every := time.Duration(c.num("probe_minutes", 10)) * time.Minute
	repos, err := e.Svc.Repositories(ctx)
	if err != nil {
		e.log.Error("notify: listing repositories for probing", "err", err)
		return
	}
	for _, r := range repos {
		if r.Backend != service.BackendS3 || paused[r.Name] {
			continue
		}
		var last *time.Time
		_ = e.Svc.DB.Pool().QueryRow(ctx, `SELECT last_probe_at FROM notify_probe_state WHERE repository_id=$1`, r.ID).Scan(&last)
		if last != nil && now.Sub(*last) < every {
			continue
		}
		msg := e.probeOne(ctx, r.Name)
		ok := msg == ""
		if _, err := e.Svc.DB.Pool().Exec(ctx, `INSERT INTO notify_probe_state (repository_id, last_probe_at, ok, consecutive_failures, error)
			VALUES ($1,$2,$3,CASE WHEN $3 THEN 0 ELSE 1 END,$4) ON CONFLICT (repository_id) DO UPDATE SET last_probe_at=EXCLUDED.last_probe_at, ok=EXCLUDED.ok,
			consecutive_failures=CASE WHEN EXCLUDED.ok THEN 0 ELSE notify_probe_state.consecutive_failures+1 END, error=EXCLUDED.error`,
			r.ID, now, ok, msg); err != nil {
			e.log.Error("notify: recording a probe", "repo", r.Name, "err", err)
		}
	}
}

func (e *Engine) probeOne(ctx context.Context, name string) string {
	pctx, cancel := context.WithTimeout(ctx, e.ProbeTimeout)
	defer cancel()
	type result struct {
		rs  []datastore.ProbeResult
		err error
	}
	ch := make(chan result, 1)
	go func() { rs, err := e.Probe(pctx, name); ch <- result{rs, err} }()
	select {
	case <-pctx.Done():
		return "probe timed out after " + e.ProbeTimeout.String()
	case r := <-ch:
		if r.err != nil {
			return r.err.Error()
		}
		for _, p := range r.rs {
			if !p.OK {
				return p.Op + ": " + p.Err
			}
		}
		return ""
	}
}

func (e *Engine) evalUnreachable(ctx context.Context, c condCfg, now time.Time) ([]observation, error) {
	need := int(c.num("failures", 2))
	if need < 1 {
		need = 1
	}
	rows, err := e.Svc.DB.Pool().Query(ctx, `SELECT r.name, p.consecutive_failures, p.error, p.last_probe_at FROM notify_probe_state p
		JOIN repositories r ON r.id=p.repository_id WHERE NOT p.ok AND p.consecutive_failures >= $1`, need)
	if err != nil {
		return nil, err
	}
	type row struct {
		repo, errText string
		n             int
		at            time.Time
	}
	rs, err := collect(rows, func(r pgx.Rows) (row, error) { var x row; return x, r.Scan(&x.repo, &x.n, &x.errText, &x.at) })
	if err != nil {
		return nil, err
	}
	var out []observation
	for _, x := range rs {
		out = append(out, observation{Key: "repository_unreachable:" + x.repo, Repo: x.repo,
			Title:  fmt.Sprintf("%s: repository storage unreachable", x.repo),
			Detail: fmt.Sprintf("%d consecutive probes of repository %s failed (last %s).\nError: %s", x.n, x.repo, x.at.UTC().Format(time.RFC3339), x.errText)})
	}
	return out, nil
}

// ---- certificates and restore tests (wired by the daemon) ---------------------------------

func (e *Engine) evalCerts(ctx context.Context, c condCfg, now time.Time) ([]observation, error) {
	items, err := e.CertStatus(ctx)
	if err != nil {
		return nil, err
	}
	warn := time.Duration(c.num("warn_days", 21) * float64(24*time.Hour))
	crit := time.Duration(c.num("critical_days", 7) * float64(24*time.Hour))
	var out []observation
	for _, it := range items {
		o := observation{Key: "cert_expiry:" + it.Name}
		left := it.NotAfter.Sub(now)
		switch {
		case it.Err != "":
			o.Title = fmt.Sprintf("certificate %s cannot be checked", it.Name)
			o.Detail = it.Err
		case left <= 0:
			o.Severity = SeverityCritical
			o.Title = fmt.Sprintf("certificate %s has expired", it.Name)
			o.Detail = fmt.Sprintf("%s (%s) expired %s.", it.Name, it.Subject, it.NotAfter.UTC().Format(time.RFC3339))
		case left < warn:
			if left < crit {
				o.Severity = SeverityCritical
			}
			o.Title = fmt.Sprintf("certificate %s expires in %s", it.Name, human(left))
			o.Detail = fmt.Sprintf("%s (%s) is valid until %s.", it.Name, it.Subject, it.NotAfter.UTC().Format(time.RFC3339))
		default:
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

func (e *Engine) evalRestoreTests(ctx context.Context, c condCfg, now time.Time) ([]observation, error) {
	rs, err := e.RestoreTests.LatestRestoreTests(ctx)
	if err != nil {
		return nil, err
	}
	var out []observation
	for _, r := range rs {
		if r.OK {
			continue
		}
		id := r.GroupID
		if id == "" {
			id = r.Host + "/" + r.Object
		}
		o := observation{Key: "restore_test_failed:" + r.Repository + "/" + id, Host: r.Host, Repo: r.Repository, Object: r.Object,
			Title:  fmt.Sprintf("%s: restore test failed", r.Object),
			Detail: fmt.Sprintf("The restore test of %s (host %s, repository %s) at %s failed.", r.Object, r.Host, r.Repository, r.At.UTC().Format(time.RFC3339))}
		if r.Error != "" {
			o.Detail += "\nError: " + r.Error
		}
		out = append(out, o)
	}
	return out, nil
}
