package mgmt

import (
	"context"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/metrics"
	"github.com/unidoc/incus-backup/internal/server/sysinfo"
)

const mp = "incus_backup_"

// staleObjectAfter: an object without a snapshot for this long counts as stale in the host metrics.
const staleObjectAfter = 48 * time.Hour

func unix(t *time.Time) float64 {
	if t == nil || t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// CollectMetrics builds the exposition. It reads only PostgreSQL and in-memory state (never the
// datastore: capacity comes from the latest capacity sample), so a scrape is cheap on S3 too.
func (s *Server) CollectMetrics(ctx context.Context) *metrics.Set {
	s.init()
	set := &metrics.Set{}
	p := s.Svc.DB.Pool()
	now := s.Svc.Now()

	set.Gauge(mp+"build_info", "Version of the running server (value 1).", 1, "version", s.Svc.Version, "goversion", runtime.Version())
	set.Gauge(mp+"process_start_time_seconds", "Start time of the server process (unix seconds).", float64(processStarted.UnixNano())/1e9)
	scrapeOK := 1.0

	// ---- repositories
	repos, err := s.Svc.Repositories(ctx)
	if err != nil {
		scrapeOK = 0
	}
	for _, r := range repos {
		l := []string{"repository", r.Name, "backend", r.Backend}
		var phys, logical, chunks, fsTotal, fsFree int64
		var sampled *time.Time
		_ = p.QueryRow(ctx, `SELECT physical_bytes, logical_bytes, chunk_count, fs_total_bytes, fs_free_bytes, taken_at FROM capacity_samples WHERE repository_id=$1 ORDER BY taken_at DESC LIMIT 1`, r.ID).
			Scan(&phys, &logical, &chunks, &fsTotal, &fsFree, &sampled)
		var snaps, objects, corrupt, failedVerify int64
		var lastBackup *time.Time
		_ = p.QueryRow(ctx, `SELECT count(*), max(committed_at), count(*) FILTER (WHERE verify_state='failed') FROM snapshots WHERE repository_id=$1`, r.ID).Scan(&snaps, &lastBackup, &failedVerify)
		_ = p.QueryRow(ctx, `SELECT count(*) FROM storage_objects WHERE repository_id=$1`, r.ID).Scan(&objects)
		_ = p.QueryRow(ctx, `SELECT count(*) FROM corrupt_chunks WHERE repository_id=$1 AND state <> 'healed'`, r.ID).Scan(&corrupt)
		set.F(mp+"repository_stored_bytes", "Physical bytes stored in the datastore (latest capacity sample).", metrics.Gauge).Add(float64(phys), l...)
		set.F(mp+"repository_logical_bytes", "Logical bytes of all snapshots (latest capacity sample).", metrics.Gauge).Add(float64(logical), l...)
		dedup := 0.0
		if phys > 0 {
			dedup = float64(logical) / float64(phys)
		}
		set.F(mp+"repository_dedup_ratio", "Logical bytes divided by stored bytes (0 when nothing is stored).", metrics.Gauge).Add(dedup, l...)
		set.F(mp+"repository_chunks", "Chunk objects in the datastore (latest capacity sample).", metrics.Gauge).Add(float64(chunks), l...)
		set.F(mp+"repository_capacity_sample_timestamp_seconds", "Time of the latest capacity sample (0: none yet).", metrics.Gauge).Add(unix(sampled), l...)
		if fsTotal > 0 {
			set.F(mp+"repository_filesystem_size_bytes", "Size of the filesystem holding a filesystem repository.", metrics.Gauge).Add(float64(fsTotal), l...)
			set.F(mp+"repository_filesystem_free_bytes", "Free space of the filesystem holding a filesystem repository.", metrics.Gauge).Add(float64(fsFree), l...)
		}
		set.F(mp+"repository_snapshots", "Committed snapshots.", metrics.Gauge).Add(float64(snaps), l...)
		set.F(mp+"repository_objects", "Storage objects (instances and volumes) known.", metrics.Gauge).Add(float64(objects), l...)
		set.F(mp+"repository_corrupt_chunks", "Corrupt chunks not healed yet (quarantined or reported).", metrics.Gauge).Add(float64(corrupt), l...)
		set.F(mp+"repository_failed_verifications", "Snapshots whose last verification failed.", metrics.Gauge).Add(float64(failedVerify), l...)
		for _, m := range []string{"online", "read-only", "offline"} {
			v := 0.0
			if r.Mode == m {
				v = 1
			}
			set.F(mp+"repository_mode", "Maintenance mode (1 for the current mode).", metrics.Gauge).Add(v, "repository", r.Name, "mode", m)
		}
		set.F(mp+"repository_last_backup_timestamp_seconds", "Commit time of the newest snapshot (0: none).", metrics.Gauge).Add(unix(lastBackup), l...)
		for _, kind := range []string{"verify", "scrub", "gc", "retention", "reconcile"} {
			var t *time.Time
			_ = p.QueryRow(ctx, `SELECT max(finished_at) FROM maintenance_runs WHERE repository_id=$1 AND kind=$2 AND status='ok'`, r.ID, kind).Scan(&t)
			set.F(mp+"repository_last_success_timestamp_seconds", "Last successful maintenance run by kind (0: never).", metrics.Gauge).Add(unix(t), "repository", r.Name, "kind", kind)
		}
	}

	// ---- hosts
	rows, err := p.Query(ctx, `SELECT r.name, h.name, h.last_seen_at, h.disabled,
			(SELECT count(*) FROM storage_objects o WHERE o.host_id=h.id),
			(SELECT count(*) FROM storage_objects o WHERE o.host_id=h.id AND NOT EXISTS (SELECT 1 FROM snapshots sn WHERE sn.object_id=o.id AND sn.committed_at > $1))
		FROM hosts h JOIN repositories r ON r.id=h.repository_id WHERE h.scope='backup' ORDER BY r.name, h.name`, now.Add(-staleObjectAfter))
	if err == nil {
		for rows.Next() {
			var repo, host string
			var seen *time.Time
			var disabled bool
			var objs, stale int64
			if rows.Scan(&repo, &host, &seen, &disabled, &objs, &stale) != nil {
				continue
			}
			l := []string{"repository", repo, "host", host}
			up := 0.0
			if seen != nil && now.Sub(*seen) < 15*time.Minute && !disabled {
				up = 1
			}
			set.F(mp+"host_up", "1 when the host talked to the server in the last 15 minutes and is not disabled.", metrics.Gauge).Add(up, l...)
			set.F(mp+"host_last_seen_timestamp_seconds", "Last request of the host (0: never).", metrics.Gauge).Add(unix(seen), l...)
			set.F(mp+"host_objects", "Storage objects of the host.", metrics.Gauge).Add(float64(objs), l...)
			set.F(mp+"host_stale_objects", "Objects of the host without a snapshot in the last 48 hours.", metrics.Gauge).Add(float64(stale), l...)
		}
		rows.Close()
	} else {
		scrapeOK = 0
	}

	// ---- backup runs, tasks, alerts, jobs
	rows, err = p.Query(ctx, `SELECT r.name, b.status, count(*) FROM backup_runs b JOIN repositories r ON r.id=b.repository_id GROUP BY r.name, b.status ORDER BY 1, 2`)
	if err == nil {
		f := set.F(mp+"backup_runs_total", "Backup runs reported by hosts, by final status (counts the rows kept in PostgreSQL).", metrics.Counter)
		for rows.Next() {
			var repo, st string
			var n int64
			if rows.Scan(&repo, &st, &n) == nil {
				f.Add(float64(n), "repository", repo, "status", st)
			}
		}
		rows.Close()
	}
	if byKind, err := s.tasks().RunningByKind(ctx); err == nil {
		f := set.F(mp+"tasks_running", "Server tasks queued or running, by kind.", metrics.Gauge)
		for _, k := range []string{"gc", "verify", "scrub", "retention", "reconcile", "sync", "capacity", "probe", "repository_destroy", "group_delete"} {
			f.Add(float64(byKind[k]), "kind", k)
			delete(byKind, k)
		}
		for k, n := range byKind {
			f.Add(float64(n), "kind", k)
		}
	}
	{
		f := set.F(mp+"alerts_firing", "Alerts currently firing, by severity.", metrics.Gauge)
		counts := map[string]int64{"info": 0, "warning": 0, "critical": 0}
		if rows, err := p.Query(ctx, `SELECT severity, count(*) FROM notify_alerts WHERE state='firing' GROUP BY severity`); err == nil {
			for rows.Next() {
				var sev string
				var n int64
				if rows.Scan(&sev, &n) == nil {
					counts[sev] = n
				}
			}
			rows.Close()
		}
		for _, sev := range []string{"info", "warning", "critical"} {
			f.Add(float64(counts[sev]), "severity", sev)
		}
	}
	{
		var queued, running int64
		_ = p.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='queued'), count(*) FILTER (WHERE state='running') FROM host_jobs`).Scan(&queued, &running)
		set.Gauge(mp+"host_jobs_queued", "Host jobs waiting for their host to pick them up.", float64(queued))
		set.Gauge(mp+"host_jobs_running", "Host jobs running on a host.", float64(running))
	}
	if cr, err := s.Svc.CertificateStatus(ctx); err == nil {
		f := set.F(mp+"certificate_days_left", "Days until a certificate the server depends on expires.", metrics.Gauge)
		f.Add(float64(cr.CA.DaysLeft), "certificate", "ca")
		f.Add(float64(cr.Server.DaysLeft), "certificate", "server")
		for _, peer := range cr.SyncPeers {
			if !peer.NotAfter.IsZero() {
				f.Add(float64(int(peer.NotAfter.Sub(now).Hours()/24)), "certificate", "sync:"+peer.Repository+"/"+peer.Job)
			}
		}
	}
	if u := s.Ops.Update; u != nil {
		st := u.Status(ctx)
		v := 0.0
		if st.UpdateAvailable {
			v = 1
		}
		set.Gauge(mp+"update_available", "1 when the update check found a newer release (0 also when checks are off).", v, "latest", st.Latest)
	}

	// ---- process and database
	pr := sysinfo.ReadProcess()
	set.Gauge(mp+"process_goroutines", "Goroutines of the server process.", float64(pr.Goroutines))
	set.Gauge(mp+"process_heap_bytes", "Go heap in use.", float64(pr.HeapBytes))
	set.Gauge(mp+"process_heap_sys_bytes", "Go heap obtained from the OS.", float64(pr.HeapSysBytes))
	set.F(mp+"process_gc_cycles_total", "Completed Go garbage collection cycles.", metrics.Counter).Add(float64(pr.GCCycles))
	set.F(mp+"process_gc_pause_seconds_total", "Total Go GC stop-the-world pause.", metrics.Counter).Add(pr.GCPauseTotalS)
	set.Gauge(mp+"process_resident_memory_bytes", "Resident memory of the server process.", float64(pr.RSSBytes))
	set.Gauge(mp+"process_open_fds", "Open file descriptors of the server process.", float64(pr.OpenFDs))
	st := p.Stat()
	set.Gauge(mp+"db_pool_connections", "PostgreSQL pool connections by state.", float64(st.AcquiredConns()), "state", "in_use")
	set.Gauge(mp+"db_pool_connections", "", float64(st.IdleConns()), "state", "idle")
	set.Gauge(mp+"db_pool_max_connections", "PostgreSQL pool size limit.", float64(st.MaxConns()))
	set.F(mp+"db_pool_acquires_total", "Connections acquired from the pool.", metrics.Counter).Add(float64(st.AcquireCount()))
	set.F(mp+"db_pool_acquire_wait_seconds_total", "Time spent waiting for a pool connection.", metrics.Counter).Add(st.AcquireDuration().Seconds())
	set.F(mp+"db_pool_empty_acquires_total", "Acquires that had to wait because the pool was empty.", metrics.Counter).Add(float64(st.EmptyAcquireCount()))
	var dbBytes int64
	if err := p.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&dbBytes); err == nil {
		set.Gauge(mp+"db_size_bytes", "Size of the PostgreSQL database.", float64(dbBytes))
	} else {
		scrapeOK = 0
	}
	s.httpStats().Collect(set, mp)
	set.Gauge(mp+"scrape_ok", "1 when every metric source answered during this scrape.", scrapeOK)
	return set
}

func (s *Server) httpStats() *metrics.HTTPStats {
	s.statsOnce.Do(func() {
		if s.Ops.HTTP == nil {
			s.Ops.HTTP = metrics.NewHTTPStats()
		}
	})
	return s.Ops.HTTP
}

// serveMetrics writes the exposition.
func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	set := s.CollectMetrics(ctx)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = set.Write(w)
}

// MetricsHandler serves GET /metrics on a dedicated listener (config metrics_listen). Unless
// unauthenticated is set, it needs an API token (Authorization: Bearer ...) of any role.
func (s *Server) MetricsHandler(unauthenticated bool) http.Handler {
	s.init()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			http.NotFound(w, r)
			return
		}
		if !unauthenticated {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") || s.Auth == nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="incus-backup metrics"`)
				http.Error(w, "an API token is required (Authorization: Bearer ...)", http.StatusUnauthorized)
				return
			}
			p, err := s.Auth.AuthenticateToken(r.Context(), strings.TrimPrefix(h, "Bearer "))
			if err != nil || !p.Role.Allows(auth.PermRead) {
				http.Error(w, "invalid API token", http.StatusUnauthorized)
				return
			}
		}
		s.serveMetrics(w, r)
	})
}
