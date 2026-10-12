package demo

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/fscat"
	"github.com/unidoc/incus-backup/internal/jobspec"
	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/jobs"
	"github.com/unidoc/incus-backup/internal/server/service"
)

func init() { auth.Argon2Memory, auth.Argon2Time = 1024, 1 } // logins in tests need not be expensive

// webClient is a browser: cookie jar, CSRF header on unsafe requests.
type webClient struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
}

func newWebClient(t *testing.T, base string) *webClient {
	jar, _ := cookiejar.New(nil)
	return &webClient{t: t, base: base, c: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

func (w *webClient) do(method, path string, body any, csrf bool) (int, []byte) {
	w.t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, w.base+path, rd)
	r.Header.Set("Content-Type", "application/json")
	if csrf {
		r.Header.Set("X-CSRF-Token", w.csrf)
	}
	resp, err := w.c.Do(r)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (w *webClient) login(name, pw string) {
	w.t.Helper()
	code, b := w.do("POST", "/api/v1/session", map[string]string{"name": name, "password": pw}, false)
	if code != 200 {
		w.t.Fatalf("login %s: %d %s", name, code, b)
	}
	var v struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.CSRF == "" {
		w.t.Fatalf("login answer without a CSRF token: %s", b)
	}
	w.csrf = v.CSRF
}

func (w *webClient) get(path string, out any) {
	w.t.Helper()
	code, b := w.do("GET", path, nil, false)
	if code != 200 {
		w.t.Fatalf("GET %s: %d %s", path, code, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			w.t.Fatalf("GET %s: %v", path, err)
		}
	}
}

func (w *webClient) raw(path string) []byte {
	w.t.Helper()
	code, b := w.do("GET", path, nil, false)
	if code != 200 {
		w.t.Fatalf("GET %s: %d %s", path, code, b)
	}
	return b
}

// TestDemoSeedsEverySituationAndServesItOverTheRealWebFlow starts the whole demo in-process with the
// reduced seed and checks, over plain HTTP with the real login/cookie/CSRF flow, that the dashboard
// shows every seeded situation and that a filesystem snapshot can be browsed and decrypted in the
// browser's way (raw index, manifest and chunks, opened with the printed recovery key only).
func TestDemoSeedsEverySituationAndServesItOverTheRealWebFlow(t *testing.T) {
	minioBin := os.Getenv("INCUS_BACKUP_TEST_MINIO")
	if minioBin == "" {
		t.Fatal("INCUS_BACKUP_TEST_MINIO must name a MinIO binary: the demo's second repository is an S3 one")
	}
	dir, err := os.MkdirTemp("", "ibdemo-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx := context.Background()
	began := time.Now()
	st, err := Start(ctx, Options{Dir: dir, Web: "127.0.0.1:0", Seed: true, Profile: TestProfile(), Minio: minioBin,
		Socket: filepath.Join(dir, "a.sock")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	took := time.Since(began)
	t.Logf("demo started in %s (seed %.1f s)", took.Round(time.Millisecond), st.State.SeedSeconds)
	if limit := 30.0; !raceEnabled && st.State.SeedSeconds > limit {
		t.Errorf("the reduced seed took %.0f s, more than %.0f s", st.State.SeedSeconds, limit)
	}
	f := st.State.Facts
	if f == nil {
		t.Fatal("no seed facts")
	}
	banner := st.Banner(took)
	for _, want := range []string{st.WebURL, st.State.RecoveryKey, "admin", "operator", "restorer", "viewer", DefaultPassword} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner lacks %q", want)
		}
	}

	w := newWebClient(t, st.WebURL)
	if code, _ := w.do("GET", "/api/v1/overview", nil, false); code != http.StatusUnauthorized {
		t.Fatalf("overview without a session: %d, want 401", code)
	}
	w.login("admin", DefaultPassword)

	var ov service.Overview
	w.get("/api/v1/overview", &ov)
	if len(ov.Repositories) != 2 {
		t.Fatalf("repositories: %+v", ov.Repositories)
	}
	backends := map[string]string{}
	for _, r := range ov.Repositories {
		backends[r.Name] = r.Backend
	}
	if backends["custody"] != "filesystem" || backends["offsite"] != "s3" {
		t.Fatalf("backends: %v", backends)
	}
	hosts := map[string]service.HostOverview{}
	for _, h := range ov.Hosts {
		hosts[h.Name] = h
	}
	if h := hosts["uni-02"]; h.Online || h.LastSeen == nil || time.Since(*h.LastSeen) < 70*time.Hour || time.Since(*h.LastSeen) > 80*time.Hour {
		t.Errorf("uni-02 must be offline, last seen about 3 days ago: %+v", h)
	}
	for _, n := range []string{"bosch", "benfica"} {
		if !hosts[n].Online {
			t.Errorf("%s must be online: %+v", n, hosts[n])
		}
	}
	problems := map[string][]string{}
	for _, p := range ov.Problems {
		problems[p.Kind] = append(problems[p.Kind], p.Subject)
	}
	has := func(kind, subject string) bool {
		for _, s := range problems[kind] {
			if subject == "" || s == subject {
				return true
			}
		}
		return false
	}
	for _, c := range []struct{ kind, subject string }{
		{"host_offline", "uni-02"}, {"stale_backup", f.StaleObject}, {"no_backup", f.NeverBackedUp},
		{"verify_failed", "custody"}, {"corrupt_chunks", "custody"}, {"sync_failing", "custody"},
	} {
		if !has(c.kind, c.subject) {
			t.Errorf("the overview lacks %s %s; problems: %v", c.kind, c.subject, problems)
		}
	}
	if hosts["bosch"].NeverBackup != 1 || ov.Protection.NeverBackup != 1 {
		t.Errorf("never backed up: host %d, protection %d", hosts["bosch"].NeverBackup, ov.Protection.NeverBackup)
	}
	if ov.Runs24h.Partial+ov.Runs24h.Failed == 0 || ov.Runs24h.OK == 0 {
		t.Errorf("runs of the last 24 h must include good and failed ones: %+v", ov.Runs24h)
	}
	if hosts["bosch"].LastRunOK == nil || *hosts["bosch"].LastRunOK == "ok" {
		t.Errorf("bosch's last run must show the failing ci-runner: %v", hosts["bosch"].LastRunOK)
	}
	if f.SyncJobs["offsite"] != "ok" || f.SyncJobs["offsite-archive"] != "failed" {
		t.Errorf("sync job states: %v", f.SyncJobs)
	}

	// several repositories: repository-scoped routes need the selector
	if code, _ := w.do("GET", "/api/v1/snapshots", nil, false); code != http.StatusBadRequest {
		t.Errorf("snapshots without ?repository with two repositories: %d, want 400", code)
	}
	var snaps []api.SnapshotRef
	w.get("/api/v1/snapshots?repository=custody&group="+f.Protected.Group, &snaps)
	var prot, failed *api.SnapshotRef
	for i := range snaps {
		switch snaps[i].SnapshotID {
		case f.Protected.Snapshot:
			prot = &snaps[i]
		case f.FailedVerify.Snapshot:
			failed = &snaps[i]
		}
	}
	if prot == nil || !prot.Protected || prot.Notes != f.Protected.Note {
		t.Errorf("protected snapshot: %+v", prot)
	}
	if failed == nil || failed.VerifyState != "failed" {
		t.Errorf("snapshot with the corrupted chunk: %+v", failed)
	}
	if len(snaps) < TestProfile().Days {
		t.Errorf("db-01 has only %d snapshots", len(snaps))
	}
	vh := w.raw("/api/v1/snapshots/" + f.FailedVerify.Group + "/" + f.FailedVerify.Snapshot + "/verifications?repository=custody")
	if !bytes.Contains(vh, []byte("failed")) {
		t.Errorf("verification history lacks the failure: %s", vh)
	}
	var hourly []api.SnapshotRef
	w.get("/api/v1/snapshots?repository=custody&group="+f.Groups[f.HourlyObject], &hourly)
	if len(hourly) < TestProfile().HourlyHours {
		t.Errorf("the hourly object has %d snapshots", len(hourly))
	}
	var offsite []api.SnapshotRef
	w.get("/api/v1/snapshots?repository=offsite", &offsite)
	if len(offsite) == 0 {
		t.Error("nothing was synced to the offsite repository")
	}
	var series []service.CapacityPoint
	w.get("/api/v1/capacity/series?repository=custody&days=30", &series)
	if len(series) < TestProfile().Days*4 {
		t.Errorf("capacity series has %d points", len(series))
	}

	// CSRF: a mutating request without the token is refused, with it accepted
	path := "/api/v1/snapshots/" + f.Protected.Group + "/" + f.Protected.Snapshot + "/notes?repository=custody"
	if code, _ := w.do("PUT", path, map[string]string{"notes": "x"}, false); code != http.StatusForbidden {
		t.Errorf("PUT without CSRF token: %d, want 403", code)
	}
	if code, b := w.do("PUT", path, map[string]string{"notes": f.Protected.Note}, true); code >= 300 {
		t.Errorf("PUT with CSRF token: %d %s", code, b)
	}

	// the viewer may not fetch raw repository material; the restorer may
	v := newWebClient(t, st.WebURL)
	v.login("viewer", DefaultPassword)
	g, sn := f.FSObject.Group, f.FSObject.Snapshot
	if code, _ := v.do("GET", "/api/v1/snapshots/"+g+"/"+sn+"/index?repository=custody", nil, false); code != http.StatusForbidden {
		t.Errorf("viewer fetched a raw index: %d", code)
	}
	r := newWebClient(t, st.WebURL)
	r.login("restorer", DefaultPassword)

	// browse and decrypt the filesystem snapshot with the printed recovery key alone
	master, err := format.DecodeRecoveryKey(st.State.RecoveryKey)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := format.DeriveKeys(master)
	if err != nil {
		t.Fatal(err)
	}
	if keys.DomainID != st.State.DomainID || keys.GroupID("default", "filesystem", "volume/nextcloud-data") != g {
		t.Fatalf("the printed key does not match the seeded key domain/group")
	}
	mb := r.raw("/api/v1/snapshots/" + g + "/" + sn + "/manifest?repository=custody")
	man, err := keys.OpenManifest(mb, g, sn)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	idx, err := format.ParseIndex(r.raw("/api/v1/snapshots/" + g + "/" + sn + "/index?repository=custody"))
	if err != nil {
		t.Fatal(err)
	}
	pay, err1 := strconv.ParseUint(man.Meta[engine.MetaFSPayload], 10, 64)
	cat, err2 := strconv.ParseUint(man.Meta[engine.MetaFSCatalog], 10, 64)
	if err1 != nil || err2 != nil || pay+cat != idx.Size {
		t.Fatalf("not a catalog snapshot: %v", man.Meta)
	}
	chunks := map[string][]byte{}
	read := func(from, to uint64) []byte { // plaintext bytes [from, to) of the snapshot stream
		var out []byte
		for i, e := range idx.Entries {
			off, n := idx.Span(i)
			if off+n <= from || off >= to {
				continue
			}
			pt, ok := chunks[e.ID.String()]
			if !ok {
				blob := r.raw("/api/v1/chunks/" + e.ID.String() + "?repository=custody")
				if pt, err = keys.DecodeChunk(blob, e.ID); err != nil {
					t.Fatalf("chunk %s: %v", e.ID, err)
				}
				chunks[e.ID.String()] = pt
			}
			lo, hi := max(from, off)-off, min(to, off+n)-off
			out = append(out, pt[lo:hi]...)
		}
		return out
	}
	tree, err := fscat.Decode(read(pay, pay+cat))
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if tree.Count < TestProfile().TreeFiles {
		t.Errorf("catalog has only %d entries", tree.Count)
	}
	ent, err := tree.Lookup(f.FSFile)
	if err != nil {
		t.Fatalf("%s not in the catalog: %v", f.FSFile, err)
	}
	content := read(ent.PayOff, ent.PayOff+ent.Size)
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != f.FSFileSHA256 || sum != ent.Sum {
		t.Fatalf("%s decrypted to the wrong content", f.FSFile)
	}
	var links, unicode int
	tree.Walk(func(p string, e *fscat.Entry) bool {
		if e.Kind == fscat.KSymlink {
			links++
		}
		if strings.ContainsAny(p, "ÞÁöΕ日🎉") {
			unicode++
		}
		return true
	})
	if links < 3 || unicode < 3 {
		t.Errorf("the tree lacks symlinks (%d) or unicode names (%d)", links, unicode)
	}

	// the admin socket answers without a login
	sc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", st.SocketPath)
	}}}
	resp, err := sc.Get("http://admin/api/v1/repositories")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(b, []byte("offsite")) {
		t.Errorf("admin socket: %d %s", resp.StatusCode, b)
	}
	fmt.Fprintf(os.Stderr, "demo test: %d snapshots, %d runs, seed %.1f s\n", f.Snapshots, f.BackupRuns, st.State.SeedSeconds)

	// host jobs, end to end through the seeded hosts' real executors
	if strings.Join(st.JobHosts, ",") != "bosch,benfica" {
		t.Fatalf("job executors: %v", st.JobHosts)
	}
	// fs_extract: the restorer asks benfica for one file, sealed to a key only the "browser" holds
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	params := jobspec.FSExtract{Project: "default", Object: "volume/nextcloud-data", Paths: []string{f.FSFile},
		RecipientPublicKey: base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())}
	j := r.job("restorer", map[string]any{"type": "fs_extract", "host": "benfica", "repository": "custody", "params": params})
	if j.Delivery == nil {
		t.Fatalf("fs_extract produced no delivery: %+v", j)
	}
	sealed := r.raw("/api/v1/jobs/" + j.ID + "/delivery")
	if int64(len(sealed)) != j.Delivery.Size || bytes.Contains(sealed, content[:min(len(content), 64)]) {
		t.Fatalf("delivery: %d bytes, metadata %d, or it carries plaintext", len(sealed), j.Delivery.Size)
	}
	if code, _ := v.do("GET", "/api/v1/jobs/"+j.ID+"/delivery", nil, false); code != http.StatusForbidden {
		t.Errorf("viewer downloaded a delivery: %d", code)
	}
	// the delivery is authenticated (IBS2): the "browser" binds the job id and the parameters it sent
	pj, _ := json.Marshal(params)
	digest, err := jobspec.RequestDigest("fs_extract", pj)
	if err != nil {
		t.Fatal(err)
	}
	sr, err := format.NewSealReader(bytes.NewReader(sealed), keys, priv.Bytes(), format.SealBinding{JobID: j.ID, Request: digest})
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(sr)
	got := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("unsealed tar: %v", err)
		}
		if h.Typeflag == tar.TypeReg {
			got[strings.TrimPrefix(h.Name, "./")], _ = io.ReadAll(tr)
		}
	}
	if len(got) != 1 {
		t.Fatalf("delivery holds %d files", len(got))
	}
	for name, b := range got {
		s := sha256.Sum256(b)
		if !strings.HasSuffix(name, filepath.Base(f.FSFile)) || hex.EncodeToString(s[:]) != f.FSFileSHA256 {
			t.Errorf("delivered %s with the wrong content", name)
		}
	}
	// restore_test: the operator proves the volume restores (extract into scratch, every file checked)
	op := newWebClient(t, st.WebURL)
	op.login("operator", DefaultPassword)
	j = op.job("operator", map[string]any{"type": "restore_test", "host": "benfica", "repository": "custody",
		"params": jobspec.RestoreTest{Project: "default", Object: "volume/nextcloud-data", Mode: "extract"}})
	var res jobspec.RestoreTestResult
	if err := json.Unmarshal(j.Result, &res); err != nil || !res.Passed || res.Files < int64(TestProfile().TreeFiles) {
		t.Errorf("restore test result: %v %s", err, j.Result)
	}
	var rts []struct {
		State string  `json:"state"`
		JobID *string `json:"job_id"`
	}
	op.get("/api/v1/restore-tests?repository=custody&host=benfica", &rts)
	found := false
	for _, x := range rts {
		found = found || (x.JobID != nil && *x.JobID == j.ID && x.State == "passed")
	}
	if !found {
		t.Errorf("the restore test is not recorded: %+v", rts)
	}

	// a restart finds the seeded directory again (same ports for the stored S3 endpoint and sync peer)
	st.Close()
	t1 := time.Now()
	st2, err := Start(ctx, Options{Dir: dir, Web: "127.0.0.1:0", Seed: true, Profile: TestProfile(), Minio: minioBin, Socket: filepath.Join(dir, "a.sock")})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer st2.Close()
	if st2.State.SeedSeconds != st.State.SeedSeconds || time.Since(t1) > 30*time.Second {
		t.Errorf("the restart seeded again (%s)", time.Since(t1))
	}
	w2 := newWebClient(t, st2.WebURL)
	w2.login("operator", DefaultPassword)
	var ov2 service.Overview
	w2.get("/api/v1/overview", &ov2)
	if len(ov2.Hosts) != 3 || len(ov2.Repositories) != 2 {
		t.Errorf("after restart: %d hosts, %d repositories", len(ov2.Hosts), len(ov2.Repositories))
	}
	var offsite2 []api.SnapshotRef
	w2.get("/api/v1/snapshots?repository=offsite", &offsite2) // the S3 repository is reachable again
	if len(offsite2) != len(offsite) {
		t.Errorf("offsite after restart: %d snapshots, before %d", len(offsite2), len(offsite))
	}
	// the executors are back after the restart: bosch, rebuilt from its state dir, runs a deep test
	if strings.Join(st2.JobHosts, ",") != "bosch,benfica" {
		t.Fatalf("job executors after restart: %v", st2.JobHosts)
	}
	j = w2.job("operator", map[string]any{"type": "restore_test", "host": "bosch", "repository": "custody",
		"params": jobspec.RestoreTest{Project: "prod", Object: "instance/api-01/root", Mode: "deep"}})
	if err := json.Unmarshal(j.Result, &res); err != nil || !res.Passed || res.Chunks == 0 {
		t.Errorf("deep restore test after restart: %v %s", err, j.Result)
	}
}

// job creates a job over the web API and waits until it finished; it must succeed.
func (w *webClient) job(who string, req map[string]any) *jobs.Job {
	w.t.Helper()
	code, b := w.do("POST", "/api/v1/jobs", req, true)
	if code != http.StatusCreated && code != http.StatusOK {
		w.t.Fatalf("%s creating %v: %d %s", who, req["type"], code, b)
	}
	var j jobs.Job
	if err := json.Unmarshal(b, &j); err != nil || j.ID == "" {
		w.t.Fatalf("job answer: %v %s", err, b)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		w.get("/api/v1/jobs/"+j.ID, &j)
		switch j.State {
		case "succeeded":
			return &j
		case "failed", "cancelled":
			w.t.Fatalf("%s job %s: %s: %s (log %+v)", j.Type, j.ID, j.State, j.Message, j.Log)
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("%s job %s still %s", j.Type, j.ID, j.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The seeded volume tree must equal its latest snapshot whatever the hour of the day the demo starts
// (the e2e test compares them file by file). At 22:47 the evening churn of "today" has no night backup
// after it, which the seed used to leave behind as edited files that are not in any snapshot.
func TestSeedKeepsTheVolumeTreeEqualToItsLatestSnapshotAtAnyHour(t *testing.T) {
	minioBin := os.Getenv("INCUS_BACKUP_TEST_MINIO")
	if minioBin == "" {
		t.Fatal("INCUS_BACKUP_TEST_MINIO must name a MinIO binary: the demo's second repository is an S3 one")
	}
	old := seedNow
	defer func() { seedNow = old }()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	for _, hm := range []time.Duration{22*time.Hour + 47*time.Minute, 10*time.Hour + 30*time.Minute} {
		at := today.Add(hm)
		seedNow = func() time.Time { return at }
		dir, err := os.MkdirTemp("", "ibdemo-hour-*")
		if err != nil {
			t.Fatal(err)
		}
		st, err := Start(context.Background(), Options{Dir: dir, Web: "127.0.0.1:0", Seed: true, Profile: TestProfile(), Minio: minioBin,
			Socket: filepath.Join(dir, "a.sock")})
		if err == nil {
			st.Close()
		}
		os.RemoveAll(dir)
		if err != nil {
			t.Errorf("seeding at %s: %v", at.Format("15:04"), err)
		}
	}
}
