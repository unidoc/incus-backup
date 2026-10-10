package mgmt_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/config"
	"github.com/unidoc/incus-backup/internal/server/logring"
	"github.com/unidoc/incus-backup/internal/server/metrics/promtest"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/sysinfo"
	"github.com/unidoc/incus-backup/internal/server/tasks"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func sampleOf(f *promtest.Family, kv ...string) (float64, bool) {
	for _, s := range f.Samples {
		ok := true
		for i := 0; i+1 < len(kv); i += 2 {
			if s.Labels[kv[i]] != kv[i+1] {
				ok = false
			}
		}
		if ok {
			return s.Value, true
		}
	}
	return 0, false
}

// GET /api/v1/metrics: a readonly token may scrape, anonymous may not; the exposition is valid and
// its numbers agree with the API.
func TestMetricsScrape(t *testing.T) {
	ctx := context.Background()
	w := newWeb(t)
	n := w.node
	k := masterKeys()
	for i := 1; i <= 2; i++ {
		backup(t, n, k, fmt.Sprintf("2026100%d120000-%08x", i, i), n.Now.Add(time.Duration(i)*time.Hour), fmt.Sprintf("m%d", i))
	}
	if err := n.Svc.SampleCapacity(ctx, "custody"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.auth.CreateUser(ctx, "viewer", pw, auth.ReadOnly, "", "", false); err != nil {
		t.Fatal(err)
	}
	w.login("viewer")
	var tok struct {
		Secret string `json:"secret"`
		Token  string `json:"token"`
	}
	code, b := w.req("POST", "/api/v1/me/tokens", map[string]any{"name": "prom", "role": "readonly"}, w.h())
	if code != 201 && code != 200 {
		t.Fatalf("token: %d %s", code, b)
	}
	_ = json.Unmarshal(b, &tok)
	secret := tok.Secret + tok.Token
	// some API traffic for the request metrics
	w.req("GET", "/api/v1/overview", nil, nil)
	w.req("GET", "/api/v1/snapshots/nope/x/verifications", nil, nil)
	anon := &http.Client{}
	resp, err := anon.Get(w.ts.URL + "/api/v1/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous scrape -> %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", w.ts.URL+"/api/v1/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err = anon.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("scrape -> %d %s\n%s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	fams, err := promtest.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("invalid exposition: %v\n%s", err, body)
	}
	want := map[string][]string{
		"incus_backup_repository_snapshots":          {"repository", "custody"},
		"incus_backup_repository_stored_bytes":       {"repository", "custody"},
		"incus_backup_repository_mode":               {"repository", "custody", "mode", "online"},
		"incus_backup_host_up":                       {"host", "benfica"},
		"incus_backup_host_objects":                  {"host", "benfica"},
		"incus_backup_alerts_firing":                 {"severity", "critical"},
		"incus_backup_certificate_days_left":         {"certificate", "ca"},
		"incus_backup_build_info":                    {"version", "test"},
		"incus_backup_db_pool_connections":           {"state", "in_use"},
		"incus_backup_http_requests_total":           {"route", "GET /api/v1/overview", "code", "200"},
		"incus_backup_http_request_duration_seconds": {"route", "GET /api/v1/overview", "le", "+Inf"},
		"incus_backup_tasks_running":                 {"kind", "gc"},
		"incus_backup_scrape_ok":                     nil,
	}
	for name, kv := range want {
		f := fams[name]
		if f == nil {
			t.Errorf("missing family %s", name)
			continue
		}
		if _, ok := sampleOf(f, kv...); !ok {
			t.Errorf("%s has no sample %v", name, kv)
		}
	}
	if v, _ := sampleOf(fams["incus_backup_repository_snapshots"], "repository", "custody"); v != 2 {
		t.Errorf("snapshots %v", v)
	}
	if v, _ := sampleOf(fams["incus_backup_repository_mode"], "mode", "online"); v != 1 {
		t.Errorf("mode %v", v)
	}
	if v, _ := sampleOf(fams["incus_backup_scrape_ok"]); v != 1 {
		t.Errorf("scrape_ok %v", v)
	}
	if v, _ := sampleOf(fams["incus_backup_http_requests_total"], "route", "GET /api/v1/snapshots/{group}/{snap}/verifications"); v != 1 {
		t.Errorf("route pattern label (not the raw path): %v", v)
	}

	// the dedicated listener: token required unless explicitly unauthenticated
	mh := httptest.NewServer(w.srv.MetricsHandler(false))
	defer mh.Close()
	if r, _ := anon.Get(mh.URL + "/metrics"); r.StatusCode != 401 {
		t.Fatalf("metrics listener without a token -> %d", r.StatusCode)
	}
	req, _ = http.NewRequest("GET", mh.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	if r, _ := anon.Do(req); r.StatusCode != 200 {
		t.Fatalf("metrics listener with a token -> %d", r.StatusCode)
	}
	if r, _ := anon.Get(mh.URL + "/other"); r.StatusCode != 404 {
		t.Fatal("the metrics listener serves only /metrics")
	}
	open := httptest.NewServer(w.srv.MetricsHandler(true))
	defer open.Close()
	r, _ := anon.Get(open.URL + "/metrics")
	ob, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if _, err := promtest.Parse(bytes.NewReader(ob)); r.StatusCode != 200 || err != nil {
		t.Fatalf("unauthenticated listener -> %d %v", r.StatusCode, err)
	}
}

// Status page, samples, the log viewer and the update notice.
func TestSystemStatusLogsAndUpdate(t *testing.T) {
	n := testsupport.NewNode(t)
	ctx := context.Background()
	ring := logring.New(100)
	log := slog.New(logring.NewHandler(ring, nil))
	e := newOps(t, n, mgmt.Ops{Logs: ring, Listeners: []mgmt.Listener{{Name: "agent", Addr: ":8007"}}})
	var st mgmt.SystemStatus
	e.c.must("GET", "/api/v1/system/status", nil, &st)
	if !st.Database.OK || st.Database.SizeBytes == 0 || st.Version != "test" || st.Host.CPUs == 0 || st.Host.MemTotalBytes == 0 ||
		len(st.Datastores) != 1 || st.Datastores[0].TotalBytes == 0 || st.Listeners[0].Addr != ":8007" || st.Process.Goroutines == 0 {
		t.Fatalf("status %+v", st)
	}
	found := false
	for _, m := range st.Database.Migrations {
		found = found || m == "20261008000000_initial_schema.sql"
	}
	if !found {
		t.Fatalf("migrations %v", st.Database.Migrations)
	}
	if err := os.MkdirAll(n.Svc.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := (&sysinfo.Sampler{Pool: n.Svc.DB.Pool(), StateDir: n.Svc.StateDir}).Take(ctx); err != nil {
		t.Fatal(err)
	}
	var samples []sysinfo.Sample
	e.c.must("GET", "/api/v1/system/samples?hours=1", nil, &samples)
	if len(samples) != 1 || samples[0].DBBytes == 0 || samples[0].DiskTotal == 0 {
		t.Fatalf("samples %+v", samples)
	}

	log.Info("listening", "addr", ":8443")
	log.Warn("slow request", "path", "/api/v1/gc", "token", "ibk_SECRET")
	log.Error("datastore unreachable", "repo", "offsite")
	var lr logring.Result
	e.c.must("GET", "/api/v1/system/logs?level=warn", nil, &lr)
	if len(lr.Lines) != 2 || lr.Lines[0].Message != "slow request" || strings.Contains(lr.Lines[0].Attrs, "SECRET") {
		t.Fatalf("warn+ %+v", lr.Lines)
	}
	e.c.must("GET", "/api/v1/system/logs?q=offsite&after=1", nil, &lr)
	if len(lr.Lines) != 1 || lr.LastSeq != 3 {
		t.Fatalf("search %+v", lr)
	}
	if code, _ := e.c.do("GET", "/api/v1/system/logs?level=loud", nil, nil); code != 400 {
		t.Fatal("bad level")
	}

	// update check: off by default; on, against a fake release feed, it finds a newer version and the
	// update_available alert fires
	n.Svc.Version = "0.3.0" // the running version the feed is compared with
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v9.1.0", http.StatusFound)
	}))
	defer feed.Close()
	var us struct {
		Enabled         bool `json:"enabled"`
		UpdateAvailable bool `json:"update_available"`
	}
	e.c.must("GET", "/api/v1/system/update", nil, &us)
	if us.Enabled || us.UpdateAvailable {
		t.Fatalf("update check must be off by default: %+v", us)
	}
	var check struct {
		Status struct {
			Enabled         bool   `json:"enabled"`
			Latest          string `json:"latest"`
			UpdateAvailable bool   `json:"update_available"`
		} `json:"status"`
		Error string `json:"error"`
	}
	e.c.must("POST", "/api/v1/system/update/check", nil, &check)
	if check.Error == "" || check.Status.Enabled {
		t.Fatalf("check while off: %+v", check)
	}
	e.c.must("PUT", "/api/v1/settings", map[string]string{"update_check": "on", "update_check_url": feed.URL + "/releases/latest"}, nil)
	if code, _ := e.c.do("PUT", "/api/v1/settings", map[string]string{"update_check": "maybe"}, nil); code != 400 {
		t.Fatal("bad update_check value")
	}
	check.Error = ""
	e.c.must("POST", "/api/v1/system/update/check", nil, &check)
	if check.Error != "" || !check.Status.UpdateAvailable || check.Status.Latest != "9.1.0" {
		t.Fatalf("check: %+v", check)
	}
	ne := mgmt.NewNotifyEngine(n.Svc)
	upd := e.s.Ops.Update
	ne.UpdateStatus = func(ctx context.Context) (string, string, string, bool) {
		s := upd.Status(ctx)
		return s.Latest, s.Current, s.ReleaseURL, s.Enabled && s.UpdateAvailable
	}
	if _, err := ne.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var c int
	_ = n.Svc.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM notify_alerts WHERE type='update_available' AND state='firing' AND severity='info'`).Scan(&c)
	if c != 1 {
		t.Fatal("update_available did not fire")
	}
	e.c.must("PUT", "/api/v1/settings", map[string]string{"update_check": "off"}, nil)
	n.Now = n.Now.Add(time.Minute)
	_, _ = ne.Tick(ctx)
	_ = n.Svc.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM notify_alerts WHERE type='update_available' AND state='firing'`).Scan(&c)
	if c != 0 {
		t.Fatal("turning checks off must resolve the alert")
	}
}

func untar(t *testing.T, b []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		c, _ := io.ReadAll(tr)
		out[h.Name] = string(c)
	}
	return out
}

// The support report holds what support needs and none of the secrets this server knows.
func TestSupportReportHasNoSecrets(t *testing.T) {
	ctx := context.Background()
	w := newWeb(t)
	n := w.node
	k := masterKeys()
	backup(t, n, k, "20261007120000-00000001", n.Now, "x")
	ring := logring.New(1000)
	log := slog.New(logring.NewHandler(ring, nil))
	cfg := config.Defaults()
	cfg.DatabaseURL = "postgres://ib:DBPASS-7c1e@db.internal:5432/ib?sslmode=disable"
	w.srv.Ops = mgmt.Ops{Logs: ring, Config: cfg.Redacted}

	secrets := map[string]string{"database password": "DBPASS-7c1e"}
	// an S3 credential
	if _, err := n.Svc.CreateS3Credential(ctx, "wasabi", service.S3Secret{AccessKey: "AKIA-REPORT-1", SecretKey: "S3SECRET-REPORT-99"}); err != nil {
		t.Fatal(err)
	}
	secrets["s3 secret key"] = "S3SECRET-REPORT-99"
	// an enrolment token
	tok, err := n.Svc.CreateToken(ctx, "custody", "porto", "backup", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	secrets["enrolment token"] = tok
	// the setup token, logged by the daemon like serveWeb does
	setup, err := w.auth.SetupToken(ctx)
	if err != nil || setup == "" {
		t.Fatalf("setup token %q %v", setup, err)
	}
	secrets["setup token"] = setup
	log.Warn("no user exists yet: create the first administrator", "setup_token", setup)
	// an admin and an API token
	if _, err := w.auth.CreateUser(ctx, "root", pw, auth.Admin, "", "", false); err != nil {
		t.Fatal(err)
	}
	w.login("root")
	code, b := w.req("POST", "/api/v1/me/tokens", map[string]any{"name": "ci", "role": "admin"}, w.h())
	var at map[string]any
	_ = json.Unmarshal(b, &at)
	apiTok, _ := at["secret"].(string)
	if apiTok == "" {
		apiTok, _ = at["token"].(string)
	}
	if code >= 300 || apiTok == "" {
		t.Fatalf("api token %d %s", code, b)
	}
	secrets["api token"] = apiTok
	log.Info("request", "authorization", "Bearer "+apiTok, "password", "hunter2-PW")
	secrets["logged password"] = "hunter2-PW"

	code, rep := w.req("POST", "/api/v1/system/support-report", nil, w.h())
	if code != 200 {
		t.Fatalf("report -> %d %s", code, rep)
	}
	files := untar(t, rep)
	for _, f := range []string{"support/README.txt", "support/version.json", "support/config.json", "support/status.json", "support/overview.json",
		"support/repositories.json", "support/tasks.json", "support/alerts.json", "support/certificates.json", "support/settings.json", "support/logs.txt"} {
		if _, ok := files[f]; !ok {
			t.Errorf("missing %s", f)
		}
	}
	all := ""
	for _, c := range files {
		all += c
	}
	for what, s := range secrets {
		if strings.Contains(all, s) {
			t.Errorf("the support report contains the %s", what)
		}
	}
	if !strings.Contains(files["support/config.json"], "db.internal:5432") || !strings.Contains(files["support/logs.txt"], "no user exists yet") {
		t.Fatalf("report lacks content:\n%s\n%s", files["support/config.json"], files["support/logs.txt"])
	}
	// host names are anonymised by default, kept on request
	if strings.Contains(all, "benfica") || !strings.Contains(all, "host-") {
		t.Fatal("host names not anonymised")
	}
	code, rep = w.req("POST", "/api/v1/system/support-report", map[string]bool{"include_host_names": true}, w.h())
	if code != 200 || !strings.Contains(strings.Join(mapValues(untar(t, rep)), ""), "benfica") {
		t.Fatal("include_host_names")
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// Detach keeps the data and can be undone with attach; destroy deletes the datastore in a task; both
// need the repository offline, typed confirmation (destroy) and every enrolled identity named.
func TestRepositoryDetachAndDestroyFilesystem(t *testing.T) {
	ctx := context.Background()
	n := testsupport.NewNode(t)
	e := newOps(t, n, mgmt.Ops{})
	k := masterKeys()
	backup(t, n, k, "20261007120000-00000001", n.Now, "keep me")
	dsPath := n.Repo.Path
	del := func(body any) (int, string) { return e.c.do("DELETE", "/api/v1/repositories/custody", body, nil) }
	if code, b := del(map[string]any{"mode": "detach"}); code != 409 || !strings.Contains(b, "offline") {
		t.Fatalf("detach online -> %d %s", code, b)
	}
	e.c.must("PUT", "/api/v1/repositories/custody/mode", map[string]string{"mode": "offline"}, nil)
	if code, b := del(map[string]any{"mode": "detach"}); code != 409 || !strings.Contains(b, "benfica") {
		t.Fatalf("detach with an enrolled host -> %d %s", code, b)
	}
	if code, _ := del(map[string]any{"mode": "explode"}); code != 400 {
		t.Fatal("bad mode")
	}
	var dr service.DetachResult
	e.c.must("DELETE", "/api/v1/repositories/custody", map[string]any{"mode": "detach", "force_hosts": []string{"benfica"}, "clear_owner": true}, &dr)
	if !dr.OwnerCleared || dr.Location != dsPath {
		t.Fatalf("%+v", dr)
	}
	var repos []mgmt.RepoView
	e.c.must("GET", "/api/v1/repositories", nil, &repos)
	if len(repos) != 0 {
		t.Fatalf("still listed %+v", repos)
	}
	if len(chunkFiles(t, dsPath)) == 0 {
		t.Fatal("detach must leave the data")
	}
	// attach again (no --adopt needed: the owner was released), reconcile brings the snapshot back
	e.c.must("POST", "/api/v1/repositories/attach", map[string]any{"name": "custody", "kind": "filesystem", "path": dsPath}, nil)
	var rec struct{ Inserted int }
	e.c.must("POST", "/api/v1/reconcile?wait=1&repository=custody", nil, &rec)
	if rec.Inserted != 1 {
		t.Fatalf("reconcile after re-attach %+v", rec)
	}

	// destroy
	e.c.must("PUT", "/api/v1/repositories/custody/mode", map[string]string{"mode": "offline"}, nil)
	if code, b := del(map[string]any{"mode": "destroy", "confirm": "wrong"}); code != 400 || !strings.Contains(b, "type the repository name") {
		t.Fatalf("wrong confirm -> %d %s", code, b)
	}
	tk := e.start("DELETE", "/api/v1/repositories/custody", map[string]any{"mode": "destroy", "confirm": "custody"})
	if tk.Kind != "repository_destroy" || !tk.Exclusive {
		t.Fatalf("%+v", tk)
	}
	d := e.wait(tk.ID)
	if d.State != tasks.Succeeded || !strings.Contains(string(d.Result), `"objects_deleted"`) {
		t.Fatalf("destroy %+v %+v", d.Task, d.Log)
	}
	ents, err := os.ReadDir(dsPath)
	if err != nil || len(ents) != 0 {
		t.Fatalf("datastore directory after destroy: %v %v", ents, err)
	}
	e.c.must("GET", "/api/v1/repositories", nil, &repos)
	if len(repos) != 0 {
		t.Fatal("destroyed repository still listed")
	}
	// the task keeps its history although its repository is gone
	var tl []tasks.Task
	e.c.must("GET", "/api/v1/tasks?kind=repository_destroy", nil, &tl)
	if len(tl) != 1 || tl[0].Repository != "custody" {
		t.Fatalf("%+v", tl)
	}
	_ = ctx
}

// Destroying an S3 repository deletes exactly the objects under its prefix: a neighbour prefix that
// shares the first letters and a key at the bucket root survive.
func TestRepositoryDestroyS3KeepsEverythingOutsideThePrefix(t *testing.T) {
	ctx := context.Background()
	n := testsupport.NewS3Node(t)
	e := newOps(t, n, mgmt.Ops{})
	k := masterKeys()
	backup(t, n, k, "20261007120000-00000001", n.Now, "s3 data", "more")
	var set service.S3Settings
	if err := json.Unmarshal(n.Repo.S3Raw, &set); err != nil {
		t.Fatal(err)
	}
	svc := s3test.Service(t)
	sentinel := func(prefix string) *datastore.S3Backend {
		b, err := datastore.NewS3Backend(datastore.S3Config{Endpoint: svc.Endpoint, Bucket: set.Bucket, Prefix: prefix, AccessKey: svc.AccessKey, SecretKey: svc.SecretKey, NoTLS: svc.NoTLS, PathStyle: true})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	neighbour, root := sentinel("ib-other"), sentinel("")
	if err := neighbour.Put("keep/me.txt", []byte("neighbour"), false); err != nil {
		t.Fatal(err)
	}
	if err := root.Put("ibsentinel", []byte("root"), false); err != nil {
		t.Fatal(err)
	}
	own := sentinel("ib")
	count := func(b datastore.Backend) int {
		c := 0
		_ = b.List("", func(string, datastore.Info) error { c++; return nil })
		return c
	}
	if count(own) < 4 {
		t.Fatalf("repository has %d objects", count(own))
	}
	e.c.must("PUT", "/api/v1/repositories/custody/mode", map[string]string{"mode": "offline"}, nil)
	var res service.DestroyResult
	e.c.must("DELETE", "/api/v1/repositories/custody?wait=1", map[string]any{"mode": "destroy", "confirm": "custody", "force_hosts": []string{"benfica"}}, &res)
	if res.ObjectsDeleted < 4 {
		t.Fatalf("%+v", res)
	}
	if c := count(own); c != 0 {
		t.Fatalf("%d objects left under the prefix", c)
	}
	if b, err := neighbour.Get("keep/me.txt"); err != nil || string(b) != "neighbour" {
		t.Fatalf("neighbour prefix: %v", err)
	}
	if _, err := root.Get("ibsentinel"); err != nil {
		t.Fatalf("root key: %v", err)
	}
	_ = ctx
}

// Deleting a whole group: protected snapshots stop it unless forced; afterwards the object is gone from
// the API and the datastore, and other objects are untouched.
func TestDeleteWholeGroup(t *testing.T) {
	n := testsupport.NewNode(t)
	e := newOps(t, n, mgmt.Ops{})
	ctx := context.Background()
	k := masterKeys()
	cl := hostClient(n, k)
	const other = "fedcba9876543210fedcba9876543210"
	for i := 1; i <= 3; i++ {
		backupObject(t, cl, k, group, "default", "vm/root", "block", fmt.Sprintf("2026100%d120000-%08x", i, i), n.Now.Add(time.Duration(i)*time.Hour), fmt.Sprintf("g%d", i))
	}
	backupObject(t, cl, k, other, "default", "vm2/root", "block", "20261001120000-00000009", n.Now, "other")
	var snaps []api.SnapshotRef
	e.c.must("GET", "/api/v1/snapshots?group="+group, nil, &snaps)
	e.c.must("PUT", "/api/v1/snapshots/"+group+"/"+snaps[0].SnapshotID+"/protected", map[string]any{"protected": true}, nil)
	if code, b := e.c.do("DELETE", "/api/v1/objects/"+group, nil, nil); code != 409 || !strings.Contains(b, "protected") {
		t.Fatalf("delete with a protected snapshot -> %d %s", code, b)
	}
	if code, _ := e.c.do("DELETE", "/api/v1/objects/00000000000000000000000000000000", nil, nil); code != 404 {
		t.Fatal("unknown group")
	}
	if code, _ := e.c.do("DELETE", "/api/v1/objects/not-a-group", nil, nil); code != 400 {
		t.Fatal("invalid group")
	}
	tk := e.start("DELETE", "/api/v1/objects/"+group, map[string]any{"force_protected": true})
	d := e.wait(tk.ID)
	if d.State != tasks.Succeeded || !strings.Contains(string(d.Result), `"snapshots_deleted":3`) || !strings.Contains(string(d.Result), `"protected_overruled":1`) {
		t.Fatalf("%+v", d.Task)
	}
	var objs []struct {
		GroupID string `json:"group_id"`
	}
	e.c.must("GET", "/api/v1/objects", nil, &objs)
	if len(objs) != 1 || objs[0].GroupID != other {
		t.Fatalf("objects %+v", objs)
	}
	st, _ := n.Svc.Store(n.Repo)
	if l, _ := st.ListSnapshots(group); len(l) != 0 {
		t.Fatalf("%d snapshots left in the datastore", len(l))
	}
	if l, _ := st.ListSnapshots(other); len(l) != 1 {
		t.Fatal("the other object was touched")
	}
	if err := st.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	var vr struct{ Ok bool }
	e.c.must("POST", "/api/v1/verify?wait=1", map[string]any{"mode": "all"}, &vr)
	if !vr.Ok {
		t.Fatal("verify after the group delete")
	}
	_ = ctx
	_ = filepath.Join
}
