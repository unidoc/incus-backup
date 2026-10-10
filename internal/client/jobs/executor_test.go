package jobs_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/client/incusx/fake"
	"github.com/unidoc/incus-backup/internal/client/jobs"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/jobspec"
	"github.com/unidoc/incus-backup/internal/recovery"
	"github.com/unidoc/incus-backup/internal/remote"
	srvjobs "github.com/unidoc/incus-backup/internal/server/jobs"
	"github.com/unidoc/incus-backup/internal/testdb"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func TestMain(m *testing.M) {
	remote.RetryDelay = time.Millisecond
	testdb.Main(m)
}

const MiB = 1 << 20

type world struct {
	t     *testing.T
	n     *testsupport.Node
	js    *srvjobs.Service
	inc   *fake.Incus
	eng   *engine.Engine
	x     *jobs.Executor
	keys  *format.Keys
	state string
	docs  string
	now   time.Time
	creds testsupport.Credentials
}

func masterKeys() *format.Keys {
	m := make([]byte, 32)
	for i := range m {
		m[i] = byte(i + 7)
	}
	k, _ := format.DeriveKeys(m)
	return k
}

func rnd(n int, seed int64) []byte {
	b := make([]byte, n)
	mrand.New(mrand.NewSource(seed)).Read(b)
	return b
}

// newWorld: one server, one host "benfica" with a running VM "bosch" (12 MiB root disk) and a
// filesystem volume "docs", both backed up once.
func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, n: testsupport.NewNode(t), inc: fake.New(t.TempDir()), keys: masterKeys(), state: t.TempDir(), now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	w.js = srvjobs.For(w.n.Svc)
	w.js.PollInterval = 20 * time.Millisecond
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w.creds = w.n.Enrol("benfica", api.ScopeBackup)
	w.eng = &engine.Engine{Inc: w.inc, Targets: map[string]*engine.Target{"cikarang": {Name: "cikarang", Client: w.n.Client(w.creds, w.keys.DomainID), Keys: w.keys}},
		Host: "benfica", SpoolDir: t.TempDir(), MaxFreeze: time.Minute, MaxLag: 7 * 24 * time.Hour, Workers: 3, Log: log,
		Policies: map[string]api.Policy{"prod": {Name: "prod", Targets: []string{"cikarang"}, CommitRequired: "all"}}}
	w.eng.Now = func() time.Time { return w.now }
	w.inc.AddVM("default", "bosch", map[string]int{"root": 12 * MiB}, true)
	w.inc.SetConfig("default", "bosch", engine.KeyPolicy, "prod")
	w.inc.GuestWrite("default", "bosch", "root", 0, rnd(12*MiB, 1))
	w.docs = t.TempDir()
	writeTree(t, w.docs, map[string]string{"secret-plans.txt": "the eagle lands at noon", "dir-zebra/big-ledger.bin": string(rnd(600000, 2)), "dir-zebra/empty-marker": ""})
	w.inc.AddFSVolume("default", "docs", w.docs)
	w.inc.Vols["default/docs"].Config[engine.KeyPolicy] = "prod"
	w.backup()
	all := map[string]bool{}
	for _, typ := range jobspec.Types {
		all[typ] = true
	}
	w.x = &jobs.Executor{Eng: w.eng, Log: log, StateDir: w.state, Allow: all, RecoveryImage: "images:alpine/edge/cloud", RecoveryPool: "default",
		Heartbeat: 30 * time.Millisecond, Wait: time.Second,
		BackupNow: func(ctx context.Context, p jobspec.BackupNow) (any, error) {
			objs := w.objects()
			rep := w.eng.Run(ctx, objs, engine.RunOptions{Names: []string{p.Object}})
			if rep.Failed() > 0 || len(rep.Results) == 0 {
				return nil, &engine.PublicError{Msg: "backup failed"}
			}
			return map[string]int{"objects": len(rep.Results)}, nil
		}}
	return w
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (w *world) objects() []*engine.Object {
	objs, err := w.eng.Discover(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	w.eng.Resolve(objs)
	return objs
}

func (w *world) backup() {
	w.t.Helper()
	w.now = w.now.Add(time.Hour)
	if rep := w.eng.Run(context.Background(), w.objects(), engine.RunOptions{}); rep.Failed() != 0 || len(rep.Results) == 0 {
		w.t.Fatalf("backup failed: %+v", rep.Results)
	}
}

func (w *world) create(typ string, params any) *srvjobs.Job {
	w.t.Helper()
	raw, _ := json.Marshal(params)
	j, err := w.js.CreateJob(context.Background(), "alice", "custody", "benfica", typ, raw)
	if err != nil {
		w.t.Fatal(err)
	}
	return j
}

// run lets the executor pick up and finish one job, then returns the server's view of it.
func (w *world) run(id string) *srvjobs.Job {
	w.t.Helper()
	ran, err := w.x.Poll(context.Background(), "cikarang", 2*time.Second)
	if err != nil || !ran {
		w.t.Fatalf("poll: %v %v", ran, err)
	}
	j, err := w.js.GetJob(context.Background(), id)
	if err != nil {
		w.t.Fatal(err)
	}
	return j
}

func (w *world) scratchVolumes() []string {
	var out []string
	vols, err := w.inc.Volumes(context.Background(), "default") // takes the fake's lock (a job may be running)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, v := range vols {
		if strings.HasPrefix(v.Name, engine.RestoreTestPrefix) || strings.HasPrefix(v.Name, "ibk-rec-") {
			out = append(out, v.Project+"/"+v.Name)
		}
	}
	return out
}

func (w *world) scratchDirsLeft() []string {
	ents, _ := os.ReadDir(filepath.Join(w.state, "scratch"))
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func logText(j *srvjobs.Job) string {
	var b strings.Builder
	for _, l := range j.Log {
		b.WriteString(l.Level + " " + l.Line + "\n")
	}
	return b.String()
}

func mustState(t *testing.T, j *srvjobs.Job, state string) {
	t.Helper()
	if j.State != state {
		t.Fatalf("job %s (%s) is %s, want %s: %q\nlog:\n%s", j.ID, j.Type, j.State, state, j.Message, logText(j))
	}
}

func guestOutput(t *testing.T, root, userData string) []byte {
	var paths []string
	for _, m := range regexp.MustCompile(`(?m)^      path=(.*)$`).FindAllStringSubmatch(userData, -1) {
		paths = append(paths, m[1])
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	h := recovery.Header{Status: "ok", Partition: "/dev/sdb1", FS: "ext4", Paths: map[string]string{}}
	for _, p := range paths {
		h.Paths[p] = "ok"
		base := filepath.Join(root, strings.TrimPrefix(p, "/"))
		_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			fi, _ := d.Info()
			hdr, _ := tar.FileInfoHeader(fi, "")
			hdr.Name = rel
			if d.IsDir() {
				hdr.Name += "/"
			}
			_ = tw.WriteHeader(hdr)
			if fi.Mode().IsRegular() {
				b, _ := os.ReadFile(path)
				_, _ = tw.Write(b)
			}
			return nil
		})
	}
	_ = tw.Close()
	return append(recovery.EncodeHeader(h), buf.Bytes()...)
}

// unsealTar fetches a job's delivery, checks the stored bytes are opaque, opens it with the
// recipient's private key and returns the files of the tar.
func (w *world) unsealTar(jobID, typ string, params any, priv *ecdh.PrivateKey, plaintexts ...string) map[string]string {
	w.t.Helper()
	ctx := context.Background()
	rc, size, sum, err := w.js.OpenDelivery(ctx, jobID)
	if err != nil {
		w.t.Fatal(err)
	}
	sealed, err := io.ReadAll(rc)
	if err != nil || int64(len(sealed)) != size || len(sum) != 64 {
		w.t.Fatalf("%v %d/%d", err, len(sealed), size)
	}
	// what the server STORES is opaque: no plaintext in any stored object
	st, _ := w.n.Svc.Store(w.n.Repo)
	var stored []byte
	_ = st
	_ = filepath.WalkDir(filepath.Join(w.n.Repo.Path, "deliveries"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			stored = append(stored, b...)
		}
		return nil
	})
	if len(stored) < len(sealed) {
		w.t.Fatalf("stored %d bytes, delivered %d", len(stored), len(sealed))
	}
	for _, p := range plaintexts {
		if bytes.Contains(stored, []byte(p)) {
			w.t.Fatalf("the server stores plaintext %q", p)
		}
	}
	// the browser's side: the digest of the parameters IT sent, the job id, the repository key
	raw, _ := json.Marshal(params)
	digest, err := jobspec.RequestDigest(typ, raw)
	if err != nil {
		w.t.Fatal(err)
	}
	b := format.SealBinding{JobID: jobID, Request: digest}
	r, err := format.NewSealReader(bytes.NewReader(sealed), w.keys, priv.Bytes(), b)
	if err != nil {
		w.t.Fatal(err)
	}
	tr := tar.NewReader(r)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			w.t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			out[strings.TrimPrefix(h.Name, "./")] = string(b)
		}
	}
	// a wrong key cannot open it
	other, _ := ecdh.X25519().GenerateKey(rand.Reader)
	if r2, err := format.NewSealReader(bytes.NewReader(sealed), w.keys, other.Bytes(), b); err == nil {
		if _, err := io.ReadAll(r2); err == nil {
			w.t.Fatal("a delivery opened with the wrong key")
		}
	}
	// nor as the delivery of another job, or of other parameters, or without the repository key
	otherKeys, _ := format.DeriveKeys(bytes.Repeat([]byte{0x5a}, 32))
	for name, try := range map[string]func() (io.Reader, error){
		"another job id": func() (io.Reader, error) {
			return format.NewSealReader(bytes.NewReader(sealed), w.keys, priv.Bytes(), format.SealBinding{JobID: jobID + "x", Request: digest})
		},
		"other parameters": func() (io.Reader, error) {
			return format.NewSealReader(bytes.NewReader(sealed), w.keys, priv.Bytes(), format.SealBinding{JobID: jobID, Request: make([]byte, 32)})
		},
		"another repository": func() (io.Reader, error) {
			return format.NewSealReader(bytes.NewReader(sealed), otherKeys, priv.Bytes(), b)
		},
	} {
		if r2, err := try(); err == nil {
			if _, err := io.ReadAll(r2); err == nil {
				w.t.Fatalf("a delivery opened as %s", name)
			}
		}
	}
	return out
}

func recipient(t *testing.T) (*ecdh.PrivateKey, string) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k, base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
}

func TestEveryJobTypeEndToEnd(t *testing.T) {
	w := newWorld(t)
	check := func(j *srvjobs.Job) {
		t.Helper()
		if !strings.Contains(logText(j), "started on benfica") || j.FinishedAt == nil || j.Progress != 100 {
			t.Fatalf("job %s: log/progress missing: %d\n%s", j.Type, j.Progress, logText(j))
		}
	}

	t.Run("backup_now", func(t *testing.T) {
		w.now = w.now.Add(time.Hour)
		j := w.run(w.create("backup_now", jobspec.BackupNow{Object: "docs"}).ID)
		mustState(t, j, "succeeded")
		check(j)
	})
	for _, c := range []struct{ object, mode string }{
		{"instance/bosch/root", "deep"}, {"volume/docs", "deep"}, {"volume/docs", "extract"}, {"instance/bosch/root", "full"}, {"volume/docs", "full"}, {"instance/bosch", "deep"},
	} {
		t.Run("restore_test "+c.mode+" "+c.object, func(t *testing.T) {
			j := w.run(w.create("restore_test", jobspec.RestoreTest{Project: "default", Object: c.object, Mode: c.mode}).ID)
			mustState(t, j, "succeeded")
			check(j)
			var res jobspec.RestoreTestResult
			_ = json.Unmarshal(j.Result, &res)
			if !res.Passed || res.Snapshot == "" || (c.object == "volume/docs" && c.mode != "deep" && res.Files != 3) {
				t.Fatalf("%+v", res)
			}
			if c.mode == "full" && c.object == "instance/bosch/root" && (res.Details["volume_sha256"] != res.Details["expected_sha256"] || res.Bytes != 12*MiB) {
				t.Fatalf("%+v", res.Details)
			}
			rt := testOfJob(t, w, j.ID)
			if rt.State != "passed" || rt.Method != c.mode || rt.Object != "default/"+c.object || rt.SnapshotID != res.Snapshot {
				t.Fatalf("%+v", rt)
			}
			if l := w.scratchVolumes(); len(l) != 0 {
				t.Fatalf("scratch volumes left: %v", l)
			}
		})
	}
	t.Run("restore_instance", func(t *testing.T) {
		j := w.run(w.create("restore_instance", jobspec.RestoreInstance{Project: "default", Instance: "bosch", AsName: "bosch-copy"}).ID)
		mustState(t, j, "succeeded")
		if !bytes.Equal(w.inc.Disk("default", "bosch-copy", "root")[:12*MiB], w.inc.Disk("default", "bosch", "root")) {
			t.Fatal("restored disk differs")
		}
		// never over an existing instance
		j = w.run(w.create("restore_instance", jobspec.RestoreInstance{Project: "default", Instance: "bosch"}).ID)
		mustState(t, j, "failed")
		if !strings.Contains(j.Message, "already exists") {
			t.Fatal(j.Message)
		}
	})
	t.Run("restore_volume", func(t *testing.T) {
		j := w.run(w.create("restore_volume", jobspec.RestoreVolume{Project: "default", Object: "volume/docs", Pool: "default", AsName: "docs-copy"}).ID)
		mustState(t, j, "succeeded")
		b, err := os.ReadFile(filepath.Join(w.inc.Vols["default/docs-copy"].Dir, "secret-plans.txt"))
		if err != nil || string(b) != "the eagle lands at noon" {
			t.Fatalf("%v %q", err, b)
		}
		j = w.run(w.create("restore_volume", jobspec.RestoreVolume{Project: "default", Object: "volume/docs", Pool: "default", AsName: "docs"}).ID)
		mustState(t, j, "failed")
	})
	t.Run("guest_files", func(t *testing.T) {
		guest := t.TempDir()
		writeTree(t, guest, map[string]string{"home/anna/notes.txt": "remember the milk, quietly", "etc/hostname": "bosch\n"})
		w.inc.Guest = func(_ []byte, ud string) []byte { return guestOutput(t, guest, ud) }
		priv, pub := recipient(t)
		params := jobspec.GuestFiles{Project: "default", Object: "instance/bosch/root", Paths: []string{"/home/anna"}, RecipientPublicKey: pub}
		j := w.run(w.create("guest_files", params).ID)
		mustState(t, j, "succeeded")
		files := w.unsealTar(j.ID, "guest_files", params, priv, "remember the milk")
		if files["home/anna/notes.txt"] != "remember the milk, quietly" || len(files) != 1 {
			t.Fatalf("%v", files)
		}
		var dr jobspec.DeliveryResult
		_ = json.Unmarshal(j.Result, &dr)
		if dr.Files != 1 || dr.Paths["/home/anna"] != "ok" || dr.Size != j.Delivery.Size {
			t.Fatalf("%+v", dr)
		}
		if l := w.inc.Leftovers(); len(l) != 0 {
			t.Fatalf("recovery leftovers: %v", l)
		}
	})
	t.Run("fs_extract", func(t *testing.T) {
		priv, pub := recipient(t)
		params := jobspec.FSExtract{Project: "default", Object: "volume/docs", Paths: []string{"secret-plans.txt"}, RecipientPublicKey: pub}
		j := w.run(w.create("fs_extract", params).ID)
		mustState(t, j, "succeeded")
		files := w.unsealTar(j.ID, "fs_extract", params, priv, "the eagle lands")
		if files["secret-plans.txt"] != "the eagle lands at noon" || len(files) != 1 {
			t.Fatalf("%v", files)
		}
		// the server never saw the name of a file it was not asked for
		if strings.Contains(logText(j)+string(j.Result)+j.Message, "big-ledger") {
			t.Fatal("an unrequested file name reached the server")
		}
	})
	t.Run("deep_verify", func(t *testing.T) {
		j := w.run(w.create("deep_verify", jobspec.DeepVerify{Last: 2}).ID)
		mustState(t, j, "succeeded")
		var out map[string]any
		_ = json.Unmarshal(j.Result, &out)
		if out["problem_count"].(float64) != 0 || out["snapshots"].(float64) < 3 {
			t.Fatalf("%v", out)
		}
	})
	if l := w.scratchDirsLeft(); len(l) != 0 {
		t.Fatalf("scratch directories left behind: %v", l)
	}
}

func TestTheHostRefusesTypesItDoesNotAllowAndRevalidatesParams(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.x.Allow = nil // defaults: backup_now, restore_test, deep_verify
	j := w.run(w.create("restore_instance", jobspec.RestoreInstance{Project: "default", Instance: "bosch", AsName: "x"}).ID)
	mustState(t, j, "failed")
	if !strings.Contains(j.Message, "not enabled on this host") {
		t.Fatal(j.Message)
	}
	if _, ok := w.inc.VMs["default/x"]; ok {
		t.Fatal("a refused job did something")
	}
	// a job the server did not validate (inserted behind its back) is validated again
	var id string
	err := w.n.Svc.DB.Pool().QueryRow(ctx, `INSERT INTO host_jobs (repository_id, host_name, type, params, created_at, queue_until)
		VALUES ($1,'benfica','restore_test','{"project":"default","object":"../../etc","mode":"deep"}',$2,$3) RETURNING id::text`,
		w.n.Repo.ID, w.n.Now, w.n.Now.Add(time.Hour)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	j = w.run(id)
	mustState(t, j, "failed")
	if !strings.HasPrefix(j.Message, "refused: invalid job parameters") {
		t.Fatal(j.Message)
	}
	// another host's job is never picked up
	w.n.Enrol("porto", api.ScopeBackup)
	raw, _ := json.Marshal(jobspec.DeepVerify{Last: 1})
	other, _ := w.js.CreateJob(ctx, "alice", "custody", "porto", "deep_verify", raw)
	if ran, _ := w.x.Poll(ctx, "cikarang", 300*time.Millisecond); ran {
		t.Fatal("picked up a job of another host")
	}
	if j, _ := w.js.GetJob(ctx, other.ID); j.State != "queued" {
		t.Fatal(j.State)
	}
}

// slowChunks delays every chunk download of the engine's target.
func slowChunks(w *world, d time.Duration) *int32 {
	var n int32
	c := w.eng.Targets["cikarang"].Client
	inner := c.HTTP.Transport
	c.HTTP = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/v1/chunks/") {
			atomic.AddInt32(&n, 1)
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return inner.RoundTrip(r)
	})}
	return &n
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCancellingAFullRestoreTestMidwayLeavesNoScratchVolume(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	slowChunks(w, 400*time.Millisecond)
	j := w.create("restore_test", jobspec.RestoreTest{Project: "default", Object: "instance/bosch/root", Mode: "full"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.x.Poll(ctx, "cikarang", 2*time.Second)
	}()
	// wait until the scratch volume exists, then cancel
	deadline := time.Now().Add(10 * time.Second)
	for len(w.scratchVolumes()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the test never created its scratch volume")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := w.js.Cancel(ctx, "alice", j.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("cancellation did not stop the job")
	}
	g, _ := w.js.GetJob(ctx, j.ID)
	mustState(t, g, "cancelled")
	if l := w.scratchVolumes(); len(l) != 0 {
		t.Fatalf("scratch volume left after cancellation: %v", l)
	}
	if rts, _ := w.js.ListRestoreTests(ctx, srvjobs.RestoreTestFilter{Repository: "custody"}); len(rts) != 0 {
		t.Fatalf("a cancelled test is no evidence: %+v", rts)
	}
}

func TestFullRestoreTestFailuresLeaveNoScratchVolume(t *testing.T) {
	w := newWorld(t)
	for _, op := range []string{"CreateVolume", "VolumeNBD"} {
		w.inc.FailNext(op, errors.New("injected "+op+" failure"))
		j := w.run(w.create("restore_test", jobspec.RestoreTest{Project: "default", Object: "instance/bosch/root", Mode: "full"}).ID)
		mustState(t, j, "failed")
		if l := w.scratchVolumes(); len(l) != 0 {
			t.Fatalf("%s: scratch volume left: %v", op, l)
		}
	}
	// the read-back connection fails (the read-only VolumeNBD after the write)
	w.eng.Inc = &failReadBack{Incus: w.inc}
	j := w.run(w.create("restore_test", jobspec.RestoreTest{Project: "default", Object: "instance/bosch/root", Mode: "full"}).ID)
	mustState(t, j, "failed")
	if !strings.Contains(j.Message, "opening the scratch volume for reading failed") {
		t.Fatal(j.Message)
	}
	if l := w.scratchVolumes(); len(l) != 0 {
		t.Fatalf("scratch volume left: %v", l)
	}
	w.eng.Inc = w.inc
	// a filesystem full test passes and cleans up too
	j = w.run(w.create("restore_test", jobspec.RestoreTest{Project: "default", Object: "volume/docs", Mode: "full"}).ID)
	mustState(t, j, "succeeded")
	if l := w.scratchVolumes(); len(l) != 0 {
		t.Fatalf("scratch volume left: %v", l)
	}
	// deleting the scratch volume fails: the job still reports, the volume is named in the HOST log
	w.inc.FailNext("DeleteVolume", errors.New("injected"))
	j = w.run(w.create("restore_test", jobspec.RestoreTest{Project: "default", Object: "instance/bosch/root", Mode: "full"}).ID)
	mustState(t, j, "succeeded")
	if l := w.scratchVolumes(); len(l) != 1 {
		t.Fatalf("expected the undeletable volume to remain (and be logged): %v", l)
	}
}

type failReadBack struct{ *fake.Incus }

func (f *failReadBack) VolumeNBD(ctx context.Context, v incusx.VolRef, writable bool) (net.Conn, error) {
	if !writable {
		return nil, errors.New("injected read-back failure")
	}
	return f.Incus.VolumeNBD(ctx, v, writable)
}

func testOfJob(t *testing.T, w *world, id string) srvjobs.RestoreTest {
	t.Helper()
	rts, err := w.js.ListRestoreTests(context.Background(), srvjobs.RestoreTestFilter{Repository: "custody", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rts {
		if r.JobID != nil && *r.JobID == id {
			return r
		}
	}
	t.Fatalf("no restore test recorded for job %s", id)
	return srvjobs.RestoreTest{}
}

func corruptChunk(t *testing.T, w *world, kind, name string, entry int) string {
	t.Helper()
	ctx := context.Background()
	tg := w.eng.Targets["cikarang"]
	loc, err := w.eng.Locate(ctx, tg, "default", name, "")
	if err != nil {
		t.Fatal(err)
	}
	id := loc.Index.Entries[entry].ID.String()
	path := filepath.Join(w.n.Repo.Path, "chunks", id[:2], id[2:4], id)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[40] ^= 1
	if err := os.WriteFile(path, b, 0o640); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestARestoreTestDetectsACorruptedChunkWithoutLeakingFileNames(t *testing.T) {
	w := newWorld(t)
	bad := corruptChunk(t, w, "block", "instance/bosch/root", 1)
	for _, mode := range []string{"deep", "full"} {
		j := w.run(w.create("restore_test", jobspec.RestoreTest{Project: "default", Object: "instance/bosch/root", Mode: mode}).ID)
		mustState(t, j, "failed")
		if !strings.Contains(j.Message, bad) {
			t.Fatalf("%s: the message must name the bad chunk: %q", mode, j.Message)
		}
		if rt := testOfJob(t, w, j.ID); rt.State != "failed" || !strings.Contains(string(rt.Details), bad) {
			t.Fatalf("%s: %+v", mode, rt)
		}
		if l := w.scratchVolumes(); len(l) != 0 {
			t.Fatalf("scratch volume left: %v", l)
		}
	}
	// the filesystem object: corrupt its first payload chunk; extract must fail and nothing that
	// reaches the server may name a file inside the volume
	badFS := corruptChunk(t, w, "filesystem", "volume/docs", 0)
	j := w.run(w.create("restore_test", jobspec.RestoreTest{Project: "default", Object: "volume/docs", Mode: "extract"}).ID)
	mustState(t, j, "failed")
	seen := logText(j) + j.Message + string(j.Result) + string(testOfJob(t, w, j.ID).Details)
	if !strings.Contains(seen, badFS) {
		t.Fatalf("the bad chunk is not named: %s", seen)
	}
	for _, secret := range []string{"secret-plans", "dir-zebra", "big-ledger", "empty-marker", "eagle"} {
		if strings.Contains(seen, secret) {
			t.Fatalf("%q reached the server: %s", secret, seen)
		}
	}
	if l := w.scratchDirsLeft(); len(l) != 0 {
		t.Fatalf("scratch directories left: %v", l)
	}
	// the full detail is in the local job log only
	b, _ := os.ReadFile(filepath.Join(w.state, "jobs", j.ID+".log"))
	if !strings.Contains(string(b), "error:") {
		t.Fatalf("local log lacks the detail: %s", b)
	}
}

func TestADeadHostsJobIsRequeuedAndFinishedByTheNextAttempt(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	j := w.create("deep_verify", jobspec.DeepVerify{Last: 1})
	// a host process claims it and dies without a word
	dead := w.n.Client(w.creds, w.keys.DomainID)
	if g, err := dead.NextJob(ctx, 0, nil); err != nil || g == nil {
		t.Fatal(err)
	}
	if ran, _ := w.x.Poll(ctx, "cikarang", 200*time.Millisecond); ran {
		t.Fatal("a running job was handed out twice")
	}
	w.n.Now = w.n.Now.Add(6 * time.Minute)
	if st, err := w.js.Maintain(ctx); err != nil || st.Requeued != 1 {
		t.Fatalf("%v %+v", err, st)
	}
	g := w.run(j.ID)
	mustState(t, g, "succeeded")
	if g.Attempt != 2 {
		t.Fatalf("attempt %d", g.Attempt)
	}
	// the dead process comes back: its attempt is stale
	if err := dead.FinishJob(ctx, j.ID, jobspec.Finish{Attempt: 1, State: "failed"}); !remote.IsStatus(err, 409) {
		t.Fatalf("%v", err)
	}
}

func TestHeavyJobsRunOneAtATimeAndLightOnesBeside(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var running, maxRunning int32
	release := make(chan struct{})
	w.x.BackupNow = func(ctx context.Context, p jobspec.BackupNow) (any, error) {
		n := atomic.AddInt32(&running, 1)
		for {
			m := atomic.LoadInt32(&maxRunning)
			if n <= m || atomic.CompareAndSwapInt32(&maxRunning, m, n) {
				break
			}
		}
		defer atomic.AddInt32(&running, -1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return nil, nil
	}
	a := w.create("backup_now", jobspec.BackupNow{Object: "docs"})
	b := w.create("backup_now", jobspec.BackupNow{Object: "bosch"})
	_, pub := recipient(t)
	c := w.create("fs_extract", jobspec.FSExtract{Project: "default", Object: "volume/docs", Paths: []string{"secret-plans.txt"}, RecipientPublicKey: pub})
	go w.x.Run(ctx)
	waitState := func(id, state string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if j, _ := w.js.GetJob(context.Background(), id); j.State == state {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		j, _ := w.js.GetJob(context.Background(), id)
		t.Fatalf("%s stayed %s (want %s): %s", id, j.State, state, j.Message)
	}
	waitState(c.ID, "succeeded") // the light job is not stuck behind the heavy one
	waitState(a.ID, "running")   // FIFO: the older heavy job first
	time.Sleep(300 * time.Millisecond)
	if j, _ := w.js.GetJob(context.Background(), b.ID); j.State != "queued" {
		t.Fatalf("a second heavy job started next to the first: %s", j.State)
	}
	close(release)
	waitState(a.ID, "succeeded")
	waitState(b.ID, "succeeded")
	if maxRunning != 1 {
		t.Fatalf("%d heavy jobs ran at once", maxRunning)
	}
}

func TestAJobThatOverrunsItsTimeoutFails(t *testing.T) {
	w := newWorld(t)
	w.x.SetTimeouts(func(string) time.Duration { return 150 * time.Millisecond })
	w.x.BackupNow = func(ctx context.Context, _ jobspec.BackupNow) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	j := w.run(w.create("backup_now", jobspec.BackupNow{Object: "docs"}).ID)
	mustState(t, j, "failed")
	if !strings.Contains(j.Message, "timed out after 150ms") {
		t.Fatal(j.Message)
	}
}

func TestADaemonShutdownLeavesTheJobToItsLease(t *testing.T) {
	w := newWorld(t)
	started := make(chan struct{})
	w.x.BackupNow = func(ctx context.Context, _ jobspec.BackupNow) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	j := w.create("backup_now", jobspec.BackupNow{Object: "docs"})
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.x.Poll(ctx, "cikarang", 2*time.Second) }()
	<-started
	stop() // the daemon stops
	<-done
	g, _ := w.js.GetJob(context.Background(), j.ID)
	mustState(t, g, "running") // not burnt as "failed"
	w.n.Now = w.n.Now.Add(6 * time.Minute)
	w.js.Maintain(context.Background())
	if g, _ := w.js.GetJob(context.Background(), j.ID); g.State != "queued" || g.Attempt != 1 {
		t.Fatalf("the lease must re-queue it: %s attempt %d", g.State, g.Attempt)
	}
}
