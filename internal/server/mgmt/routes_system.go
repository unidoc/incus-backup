package mgmt

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/server/logring"
	"github.com/unidoc/incus-backup/internal/server/metrics"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/sysinfo"
	"github.com/unidoc/incus-backup/internal/server/updatecheck"
)

func init() {
	perms["GET /api/v1/metrics"] = rd
	perms["GET /api/v1/system/status"] = rd
	perms["GET /api/v1/system/samples"] = rd
	perms["GET /api/v1/system/logs"] = ad
	perms["POST /api/v1/system/support-report"] = ad
	perms["GET /api/v1/system/update"] = rd
	perms["POST /api/v1/system/update/check"] = ad
}

// Listener describes one address the daemon listens on.
type Listener struct {
	Name   string `json:"name"` // agent, web, admin, metrics
	Addr   string `json:"addr"`
	Detail string `json:"detail,omitempty"`
}

// Ops are the operations extras the daemon wires in (all optional: a test server works without them).
type Ops struct {
	// Logs is the in-memory log ring of the daemon (GET /system/logs, the support report).
	Logs *logring.Ring
	// Listeners are reported on the status page.
	Listeners []Listener
	// Config returns the bootstrap configuration with secrets redacted (support report).
	Config func() map[string]any
	// Update is the opt-in release check.
	Update *updatecheck.Checker
	// HTTP counts management API requests (created on first use).
	HTTP *metrics.HTTPStats
}

// SystemStatus is GET /system/status.
type SystemStatus struct {
	Version       string              `json:"version"`
	GoVersion     string              `json:"go_version"`
	ServerTime    time.Time           `json:"server_time"`
	Started       time.Time           `json:"started"`
	UptimeSeconds int64               `json:"uptime_seconds"`
	InstanceName  string              `json:"instance_name"`
	Host          sysinfo.Host        `json:"host"`
	Process       sysinfo.Process     `json:"process"`
	StateDir      sysinfo.Disk        `json:"state_dir"`
	Datastores    []sysinfo.Disk      `json:"datastores"`
	Database      sysinfo.Database    `json:"database"`
	Listeners     []Listener          `json:"listeners"`
	Update        *updatecheck.Status `json:"update,omitempty"`
}

// SystemStatus collects the status page.
func (s *Server) SystemStatus(ctx context.Context) *SystemStatus {
	s.init()
	st := &SystemStatus{Version: s.Svc.Version, GoVersion: runtime.Version(), ServerTime: time.Now().UTC(), Started: processStarted,
		UptimeSeconds: int64(time.Since(processStarted).Seconds()), Listeners: s.Ops.Listeners, Datastores: []sysinfo.Disk{}}
	if st.Listeners == nil {
		st.Listeners = []Listener{}
	}
	st.Host = sysinfo.ReadHost(sysinfo.CPUPercent())
	st.Process = sysinfo.ReadProcess()
	if s.Svc.StateDir != "" {
		st.StateDir = sysinfo.StatFS(s.Svc.StateDir, "state directory")
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st.Database = sysinfo.ReadDatabase(dctx, s.Svc.DB.Pool())
	if repos, err := s.Svc.Repositories(dctx); err == nil {
		for _, r := range repos {
			if r.Backend == service.BackendS3 {
				st.Datastores = append(st.Datastores, sysinfo.Disk{Path: r.Path, Label: "repository " + r.Name, Error: "object storage: no filesystem to measure"})
				continue
			}
			st.Datastores = append(st.Datastores, sysinfo.StatFS(r.Path, "repository "+r.Name))
		}
	}
	st.InstanceName = s.Svc.Setting(dctx, "instance_name")
	if s.Ops.Update != nil {
		u := s.Ops.Update.Status(dctx)
		st.Update = &u
	}
	return st
}

// systemRoutes: status page, samples, the daemon log, the support report, the update notice and metrics.
func (s *Server) systemRoutes(h routeFn) {
	h("GET /api/v1/metrics", s.serveMetrics)
	h("GET /api/v1/system/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.SystemStatus(r.Context()))
	})
	h("GET /api/v1/system/samples", func(w http.ResponseWriter, r *http.Request) {
		hours, _ := strconv.ParseFloat(r.URL.Query().Get("hours"), 64)
		if hours <= 0 {
			hours = 24
		}
		if hours > 24*7 {
			hours = 24 * 7
		}
		pts, _ := strconv.Atoi(r.URL.Query().Get("points"))
		if pts <= 0 || pts > 2000 {
			pts = 500
		}
		l, err := sysinfo.Series(r.Context(), s.Svc.DB.Pool(), time.Now().Add(-time.Duration(hours*float64(time.Hour))), pts)
		s.reply(w, r, l, err)
	})
	h("GET /api/v1/system/logs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		lvl, err := logring.ParseLevel(q.Get("level"))
		if err != nil {
			s.fail(w, r, fmt.Errorf("%w: %v", service.ErrBadRequest, err))
			return
		}
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		if s.Ops.Logs == nil {
			writeJSON(w, 200, map[string]any{"lines": []logring.Line{}, "last_seq": 0, "capacity": 0, "dropped": 0,
				"note": "this server process does not keep its log in memory (only the daemon does)"})
			return
		}
		writeJSON(w, 200, s.Ops.Logs.Get(logring.Query{MinLevel: lvl, Contains: q.Get("q"), After: after, Limit: limit}))
	})
	h("POST /api/v1/system/support-report", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			// IncludeHostNames keeps host names (default: replaced by host-1, host-2, ...).
			IncludeHostNames bool `json:"include_host_names"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		b, err := s.SupportReport(ctx, !req.IncludeHostNames)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		name := "incus-backup-support-" + time.Now().UTC().Format("20060102-150405") + ".tar.gz"
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		_, _ = w.Write(b)
	})
	h("GET /api/v1/system/update", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.updateChecker().Status(r.Context()))
	})
	h("POST /api/v1/system/update/check", func(w http.ResponseWriter, r *http.Request) {
		st, err := s.updateChecker().Check(r.Context())
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"status": st, "error": err.Error()}) // a failed check is a result
			return
		}
		writeJSON(w, 200, map[string]any{"status": st})
	})
}

func (s *Server) updateChecker() *updatecheck.Checker {
	s.updOnce.Do(func() {
		if s.Ops.Update == nil {
			s.Ops.Update = &updatecheck.Checker{Current: s.Svc.Version, Settings: s.Svc.UpdateCheckSettings}
		}
	})
	return s.Ops.Update
}

// SupportReport builds a .tar.gz for a support ticket: version, redacted configuration, status, recent
// logs, overview, repositories (no credentials), recent tasks, alerts, certificates and settings. Secrets
// are never included: the configuration is redacted, credentials and tokens are never read, and the log
// ring redacts secret attributes when it captures them. With anonymise, host names become host-N.
func (s *Server) SupportReport(ctx context.Context, anonymise bool) ([]byte, error) {
	s.init()
	files := map[string][]byte{}
	add := func(name string, v any) {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			b = []byte(`{"error": ` + strconv.Quote(err.Error()) + `}`)
		}
		files[name] = append(b, '\n')
	}
	errOf := func(err error) map[string]string { return map[string]string{"error": err.Error()} }
	add("version.json", map[string]any{"version": s.Svc.Version, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
		"generated": time.Now().UTC(), "anonymised_host_names": anonymise})
	if s.Ops.Config != nil {
		add("config.json", s.Ops.Config())
	} else {
		add("config.json", map[string]string{"note": "not available in this process"})
	}
	add("status.json", s.SystemStatus(ctx))
	if ov, err := s.Svc.OverviewAll(ctx); err == nil {
		add("overview.json", ov)
	} else {
		add("overview.json", errOf(err))
	}
	if rs, err := s.Svc.Repositories(ctx); err == nil {
		out := make([]RepoView, 0, len(rs))
		for _, x := range rs {
			v := s.view(ctx, x, nil)
			v.Credential = "" // the credential's name is not needed
			out = append(out, v)
		}
		add("repositories.json", out)
	}
	if ts, err := s.tasks().List(ctx, tasksFilterRecent()); err == nil {
		add("tasks.json", ts)
	}
	add("alerts.json", s.alertSummary(ctx))
	if cr, err := s.Svc.CertificateStatus(ctx); err == nil {
		add("certificates.json", cr)
	} else {
		add("certificates.json", errOf(err))
	}
	if kv, err := s.Svc.Settings(ctx); err == nil {
		add("settings.json", kv)
	}
	if s.Ops.Logs != nil {
		res := s.Ops.Logs.Get(logring.Query{MinLevel: slog.LevelDebug, Limit: 1 << 30})
		var b strings.Builder
		for _, l := range res.Lines {
			fmt.Fprintf(&b, "%s %-5s %s %s\n", l.Time.Format(time.RFC3339Nano), l.Level, l.Message, l.Attrs)
		}
		files["logs.txt"] = []byte(b.String())
	} else {
		files["logs.txt"] = []byte("(this process keeps no log in memory)\n")
	}
	files["README.txt"] = []byte(`incus-backup-server support report

Generated by POST /api/v1/system/support-report. It contains the version, the bootstrap configuration
with secrets redacted, the server status, the recent daemon log (secret attributes redacted when
captured), the dashboard overview, repositories (no credentials), recent tasks, alerts, certificates
(public data only) and settings. It never contains database passwords, S3 keys, API or enrolment tokens.
` + map[bool]string{true: "Host names are replaced by host-1, host-2, ... (anonymised).\n", false: "Host names are included.\n"}[anonymise])
	if anonymise {
		names, err := s.hostNames(ctx)
		if err == nil {
			anonymiseFiles(files, names)
		}
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	now := time.Now()
	for _, k := range keys {
		if err := tw.WriteHeader(&tar.Header{Name: "support/" + k, Mode: 0o644, Size: int64(len(files[k])), ModTime: now}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[k]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *Server) hostNames(ctx context.Context) ([]string, error) {
	rows, err := s.Svc.DB.Pool().Query(ctx, `SELECT DISTINCT name FROM hosts UNION SELECT DISTINCT host_name FROM storage_objects WHERE host_name <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil && n != "" {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}

// anonymiseFiles replaces every host name (as a whole word) with host-N, longest names first so a
// name that contains another is replaced as a whole. The machine's own hostname is replaced too.
func anonymiseFiles(files map[string][]byte, names []string) {
	if h, err := os.Hostname(); err == nil && h != "" {
		names = append(names, h)
	}
	seen := map[string]bool{}
	var uniq []string
	for _, n := range names {
		if len(n) >= 2 && !seen[n] {
			seen[n] = true
			uniq = append(uniq, n)
		}
	}
	sort.Slice(uniq, func(i, j int) bool {
		if len(uniq[i]) != len(uniq[j]) {
			return len(uniq[i]) > len(uniq[j])
		}
		return uniq[i] < uniq[j]
	})
	type rep struct {
		re  *regexp.Regexp
		new string
	}
	var reps []rep
	for i, n := range uniq {
		reps = append(reps, rep{regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(n) + `($|[^A-Za-z0-9_-])`), fmt.Sprintf("${1}host-%d${2}", i+1)})
	}
	for k, b := range files {
		for _, r := range reps {
			// twice: adjacent matches share the separator
			b = r.re.ReplaceAll(b, []byte(r.new))
			b = r.re.ReplaceAll(b, []byte(r.new))
		}
		files[k] = b
	}
}

func (s *Server) alertSummary(ctx context.Context) any {
	rows, err := s.Svc.DB.Pool().Query(ctx, `SELECT type, severity, state, title, host, repository, first_seen, last_seen FROM notify_alerts
		WHERE state <> 'resolved' OR last_seen > now() - interval '7 days' ORDER BY last_seen DESC LIMIT 500`)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	type a struct {
		Type       string    `json:"type"`
		Severity   string    `json:"severity"`
		State      string    `json:"state"`
		Title      string    `json:"title"`
		Host       string    `json:"host"`
		Repository string    `json:"repository"`
		FirstSeen  time.Time `json:"first_seen"`
		LastSeen   time.Time `json:"last_seen"`
	}
	out := []a{}
	for rows.Next() {
		var x a
		if rows.Scan(&x.Type, &x.Severity, &x.State, &x.Title, &x.Host, &x.Repository, &x.FirstSeen, &x.LastSeen) == nil {
			out = append(out, x)
		}
	}
	return out
}
