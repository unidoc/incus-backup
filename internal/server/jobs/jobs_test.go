package jobs_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/jobspec"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/server/jobs"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/testdb"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func TestMain(m *testing.M) {
	remote.RetryDelay = time.Millisecond
	testdb.AtExit(s3test.Start())
	testdb.Main(m)
}

const domain = "0123456789abcdef"

type env struct {
	t  *testing.T
	n  *testsupport.Node
	js *jobs.Service
	a  *remote.Client // host "benfica"
	b  *remote.Client // host "porto"
}

func newEnv(t *testing.T, s3 bool) *env {
	t.Helper()
	var n *testsupport.Node
	if s3 {
		n = testsupport.NewS3Node(t)
	} else {
		n = testsupport.NewNode(t)
	}
	e := &env{t: t, n: n, js: jobs.For(n.Svc)}
	e.js.PollInterval = 20 * time.Millisecond
	e.a = n.Client(n.Enrol("benfica", api.ScopeBackup), domain)
	e.b = n.Client(n.Enrol("porto", api.ScopeBackup), domain)
	return e
}

func (e *env) create(host, typ string, params any) *jobs.Job {
	e.t.Helper()
	raw, _ := json.Marshal(params)
	j, err := e.js.CreateJob(context.Background(), "alice", "custody", host, typ, raw)
	if err != nil {
		e.t.Fatalf("create %s: %v", typ, err)
	}
	return j
}

func (e *env) get(id string) *jobs.Job {
	e.t.Helper()
	j, err := e.js.GetJob(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func recipient(t *testing.T) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
}

var rt = jobspec.RestoreTest{Project: "default", Object: "instance/vm1/root", Mode: "deep"}

func TestCreateValidatesTypeParamsAndHost(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	cases := []struct {
		host, typ, params string
	}{
		{"benfica", "format_disk", `{}`},
		{"benfica", "restore_test", `{"project":"default","object":"instance/vm1/root","mode":"wild"}`},
		{"benfica", "restore_test", `{"project":"default","object":"../etc","mode":"deep"}`},
		{"benfica", "restore_test", `{"project":"default","object":"instance/vm1/root","mode":"deep","extra":1}`},
		{"benfica", "fs_extract", `{"project":"default","object":"volume/v","paths":["../../x"],"recipient_public_key":"` + recipient(t) + `"}`},
		{"benfica", "fs_extract", `{"project":"default","object":"volume/v","paths":["etc/passwd"],"recipient_public_key":"AAAA"}`},
		{"benfica", "backup_now", `{}`},
		{"benfica", "restore_test", `{"project":"default","object":"instance/vm1/root","snapshot":"not-a-snapshot"}`},
		{"nobody", "restore_test", `{"project":"default","object":"instance/vm1/root"}`},
	}
	for _, c := range cases {
		if _, err := e.js.CreateJob(ctx, "alice", "custody", c.host, c.typ, json.RawMessage(c.params)); err == nil {
			t.Errorf("%s %s %s must be refused", c.host, c.typ, c.params)
		}
	}
	// a sync peer is not a job host
	e.n.Enrol("peer", api.ScopeSyncRead)
	if _, err := e.js.CreateJob(ctx, "alice", "custody", "peer", "deep_verify", nil); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("a sync-scope peer must not get jobs: %v", err)
	}
	// defaults and canonical params
	j := e.create("benfica", "fs_extract", map[string]any{"project": "default", "object": "volume/v", "paths": []string{"/etc//passwd", "a/./b"}, "recipient_public_key": recipient(t)})
	var p jobspec.FSExtract
	_ = json.Unmarshal(j.Params, &p)
	if strings.Join(p.Paths, ",") != "etc/passwd,a/b" || j.Permission != "jobs.files" || j.CreatedBy != "alice" || j.State != "queued" || j.MaxAttempts != 2 {
		t.Fatalf("%+v %+v", j, p)
	}
	if jobs.RequiredPermission("restore_instance") != "jobs.restore" || jobs.RequiredPermission("nope") != "" {
		t.Fatal("permissions")
	}
}

func TestAHostSeesAndTouchesOnlyItsOwnJobs(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	j := e.create("benfica", "restore_test", rt)
	if got, err := e.b.NextJob(ctx, 0, nil); err != nil || got != nil {
		t.Fatalf("porto must not receive benfica's job: %v %v", got, err)
	}
	got, err := e.a.NextJob(ctx, 0, nil)
	if err != nil || got == nil || got.ID != j.ID || got.Attempt != 1 || got.LeaseSeconds != 300 {
		t.Fatalf("%+v %v", got, err)
	}
	// porto cannot learn about, renew, cancel-check, upload to or finish it: 404 every time
	if _, err := e.b.JobProgress(ctx, j.ID, jobspec.Progress{Attempt: 1, Progress: 50}); !remote.IsStatus(err, 404) {
		t.Fatalf("progress by another host: %v", err)
	}
	if _, err := e.b.JobCancelRequested(ctx, j.ID); !remote.IsStatus(err, 404) {
		t.Fatalf("cancel-requested by another host: %v", err)
	}
	if err := e.b.FinishJob(ctx, j.ID, jobspec.Finish{Attempt: 1, State: "succeeded"}); !remote.IsStatus(err, 404) {
		t.Fatalf("finish by another host: %v", err)
	}
	if _, err := e.b.PutDelivery(ctx, j.ID, 1, strings.NewReader("x")); !remote.IsStatus(err, 404) {
		t.Fatalf("delivery by another host: %v", err)
	}
	// the owner reports progress with logs and finishes
	if r, err := e.a.JobProgress(ctx, j.ID, jobspec.Progress{Attempt: 1, Progress: 40, Message: "reading\x00chunks", Log: []jobspec.LogLine{{Level: "info", Line: "hello"}, {Level: "bogus", Line: "x\ny"}}}); err != nil || r.CancelRequested {
		t.Fatalf("%v %+v", err, r)
	}
	if g := e.get(j.ID); g.Message != "reading chunks" || g.Progress != 40 || g.LeaseUntil == nil {
		t.Fatalf("%+v", g)
	}
	res, _ := json.Marshal(jobspec.RestoreTestResult{Project: "default", Object: "instance/vm1/root", Snapshot: "20261007120000-0a0a0a0a", Method: "deep", Passed: true, Bytes: 42, Chunks: 3})
	if err := e.a.FinishJob(ctx, j.ID, jobspec.Finish{Attempt: 1, State: "succeeded", Result: res}); err != nil {
		t.Fatal(err)
	}
	got2 := e.get(j.ID)
	if got2.State != "succeeded" || got2.Progress != 100 || got2.FinishedAt == nil || len(got2.Log) < 3 {
		t.Fatalf("%+v", got2)
	}
	var lines []string
	for _, l := range got2.Log {
		lines = append(lines, l.Level+":"+l.Line)
	}
	if !strings.Contains(strings.Join(lines, "|"), "info:hello|info:x y") {
		t.Fatalf("log lines not sanitised/stored: %v", lines)
	}
	// the restore test is recorded
	tests, err := e.js.ListRestoreTests(ctx, jobs.RestoreTestFilter{Repository: "custody"})
	if err != nil || len(tests) != 1 || tests[0].State != "passed" || tests[0].SnapshotID != "20261007120000-0a0a0a0a" || tests[0].JobID == nil || *tests[0].JobID != j.ID || tests[0].Bytes != 42 {
		t.Fatalf("%v %+v", err, tests)
	}
	// finishing twice is refused
	if err := e.a.FinishJob(ctx, j.ID, jobspec.Finish{Attempt: 1, State: "failed"}); !remote.IsStatus(err, 409) {
		t.Fatalf("second finish: %v", err)
	}
}

func TestADisabledHostGetsNoJobs(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	e.create("benfica", "deep_verify", map[string]int{"last": 1})
	if err := e.n.Svc.SetHostDisabled(ctx, "custody", "benfica", api.ScopeBackup, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.NextJob(ctx, 0, nil); !remote.IsStatus(err, 403) {
		t.Fatalf("a disabled host must be refused: %v", err)
	}
	if _, err := e.js.CreateJob(ctx, "alice", "custody", "benfica", "deep_verify", nil); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("no new jobs for a disabled host: %v", err)
	}
}

func TestLeaseExpiryRequeuesThenFailsAndFencesStaleAttempts(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	j := e.create("benfica", "restore_test", rt) // max 2 attempts
	if g, _ := e.a.NextJob(ctx, 0, nil); g == nil || g.Attempt != 1 {
		t.Fatal("claim 1")
	}
	// progress renews the lease: 4 minutes later it is still alive
	e.n.Now = e.n.Now.Add(4 * time.Minute)
	if _, err := e.a.JobProgress(ctx, j.ID, jobspec.Progress{Attempt: 1, Progress: 10}); err != nil {
		t.Fatal(err)
	}
	e.n.Now = e.n.Now.Add(4 * time.Minute)
	if st, _ := e.js.Maintain(ctx); st.Requeued != 0 || e.get(j.ID).State != "running" {
		t.Fatalf("a renewed lease must not expire: %+v", st)
	}
	e.n.Now = e.n.Now.Add(2 * time.Minute) // 6 minutes of silence
	st, err := e.js.Maintain(ctx)
	if err != nil || st.Requeued != 1 {
		t.Fatalf("%v %+v", err, st)
	}
	if g := e.get(j.ID); g.State != "queued" || !strings.Contains(g.Message, "stopped reporting") {
		t.Fatalf("%+v", g)
	}
	// the dead host wakes up: its attempt is stale
	if _, err := e.a.JobProgress(ctx, j.ID, jobspec.Progress{Attempt: 1, Progress: 90}); !remote.IsStatus(err, 409) {
		t.Fatalf("stale progress must get 409: %v", err)
	}
	g, _ := e.a.NextJob(ctx, 0, nil)
	if g == nil || g.Attempt != 2 {
		t.Fatalf("re-claim: %+v", g)
	}
	if err := e.a.FinishJob(ctx, j.ID, jobspec.Finish{Attempt: 1, State: "succeeded"}); !remote.IsStatus(err, 409) {
		t.Fatalf("a stale finish must get 409: %v", err)
	}
	e.n.Now = e.n.Now.Add(6 * time.Minute)
	if st, _ := e.js.Maintain(ctx); st.Failed != 1 {
		t.Fatalf("out of attempts: %+v", st)
	}
	if g := e.get(j.ID); g.State != "failed" || g.FinishedAt == nil {
		t.Fatalf("%+v", g)
	}
	tests, _ := e.js.ListRestoreTests(ctx, jobs.RestoreTestFilter{Repository: "custody"})
	if len(tests) != 1 || tests[0].State != "failed" || !strings.Contains(string(tests[0].Details), "stopped reporting") {
		t.Fatalf("%+v", tests)
	}
	// a non-retryable type fails at once
	j2 := e.create("benfica", "restore_instance", map[string]string{"project": "default", "instance": "vm1", "as_name": "vm1-copy"})
	e.a.NextJob(ctx, 0, nil)
	e.n.Now = e.n.Now.Add(6 * time.Minute)
	e.js.Maintain(ctx)
	if g := e.get(j2.ID); g.State != "failed" {
		t.Fatalf("restore_instance must not be retried: %+v", g)
	}
}

func TestCancelQueuedAndRunning(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	q := e.create("benfica", "deep_verify", nil)
	if j, err := e.js.Cancel(ctx, "alice", q.ID); err != nil || j.State != "cancelled" {
		t.Fatalf("%v %+v", err, j)
	}
	if got, _ := e.a.NextJob(ctx, 0, nil); got != nil {
		t.Fatal("a cancelled job must not be handed out")
	}
	r := e.create("benfica", "deep_verify", nil)
	e.a.NextJob(ctx, 0, nil)
	if j, err := e.js.Cancel(ctx, "alice", r.ID); err != nil || j.State != "running" || !j.CancelRequested {
		t.Fatalf("%v %+v", err, j)
	}
	if cs, err := e.a.JobCancelRequested(ctx, r.ID); err != nil || !cs.CancelRequested {
		t.Fatalf("%v %+v", err, cs)
	}
	if p, err := e.a.JobProgress(ctx, r.ID, jobspec.Progress{Attempt: 1}); err != nil || !p.CancelRequested {
		t.Fatalf("%v %+v", err, p)
	}
	if err := e.a.FinishJob(ctx, r.ID, jobspec.Finish{Attempt: 1, State: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.js.Cancel(ctx, "alice", r.ID); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("cancelling a finished job: %v", err)
	}
	if _, err := e.js.Cancel(ctx, "alice", "not-a-uuid"); !errors.Is(err, service.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestLongPollWakesWhenAJobIsCreated(t *testing.T) {
	e := newEnv(t, false)
	e.js.PollInterval = time.Hour // only the in-process bell can wake it
	ctx := context.Background()
	type res struct {
		j   *jobspec.Job
		err error
		d   time.Duration
	}
	ch := make(chan res, 1)
	go func() {
		start := time.Now()
		j, err := e.a.NextJob(ctx, 20*time.Second, nil)
		ch <- res{j, err, time.Since(start)}
	}()
	time.Sleep(300 * time.Millisecond)
	want := e.create("benfica", "deep_verify", nil)
	r := <-ch
	if r.err != nil || r.j == nil || r.j.ID != want.ID || r.d > 5*time.Second {
		t.Fatalf("%+v", r)
	}
	// and an empty poll returns 204 after the wait
	start := time.Now()
	if j, err := e.a.NextJob(ctx, time.Second, nil); err != nil || j != nil || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("%v %v %s", j, err, time.Since(start))
	}
	// types restrict what is claimed
	e.create("benfica", "restore_test", rt)
	if j, _ := e.a.NextJob(ctx, 0, []string{"deep_verify"}); j != nil {
		t.Fatal("a job of a type the host did not ask for was handed out")
	}
	if j, _ := e.a.NextJob(ctx, 0, []string{"restore_test"}); j == nil {
		t.Fatal("restore_test not handed out")
	}
}

func TestConcurrentClaimsHandEachJobOutOnce(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		e.create("benfica", "deep_verify", nil)
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := e.a.NextJob(ctx, 0, nil)
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 20 {
		t.Fatalf("%d distinct jobs claimed", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("job %s claimed %d times", id, n)
		}
	}
}

func deliveryJob(e *env) *jobspec.Job {
	e.t.Helper()
	e.create("benfica", "fs_extract", map[string]any{"project": "default", "object": "volume/v", "paths": []string{"etc"}, "recipient_public_key": recipient(e.t)})
	j, err := e.a.NextJob(context.Background(), 0, nil)
	if err != nil || j == nil {
		e.t.Fatal(err)
	}
	return j
}

func storeKeys(t *testing.T, e *env, prefix string) []string {
	st, err := e.n.Svc.Store(e.n.Repo)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := st.Backend().List(prefix, func(k string, _ datastore.Info) error { keys = append(keys, k); return nil }); err != nil {
		t.Fatal(err)
	}
	return keys
}

func testDelivery(t *testing.T, s3 bool) {
	e := newEnv(t, s3)
	ctx := context.Background()
	old := jobs.DeliveryPartSize
	jobs.DeliveryPartSize = 1 << 20
	t.Cleanup(func() { jobs.DeliveryPartSize = old })
	j := deliveryJob(e)
	payload := make([]byte, 3<<20+12345)
	rand.New(rand.NewSource(1)).Read(payload)
	rc, err := e.a.PutDelivery(ctx, j.ID, j.Attempt, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if rc.Size != int64(len(payload)) {
		t.Fatalf("%+v", rc)
	}
	if keys := storeKeys(t, e, "deliveries/"+j.ID+"/1/"); len(keys) != 4 {
		t.Fatalf("parts: %v", keys)
	}
	// a second upload of the same attempt is refused
	if _, err := e.a.PutDelivery(ctx, j.ID, j.Attempt, strings.NewReader("again")); !remote.IsStatus(err, 409) {
		t.Fatalf("second upload: %v", err)
	}
	res, _ := json.Marshal(jobspec.DeliveryResult{Size: rc.Size, SHA256: rc.SHA256, Files: 1})
	if err := e.a.FinishJob(ctx, j.ID, jobspec.Finish{Attempt: 1, State: "succeeded", Result: res}); err != nil {
		t.Fatal(err)
	}
	if g := e.get(j.ID); g.State != "succeeded" || g.Delivery == nil || g.Delivery.Size != int64(len(payload)) {
		t.Fatalf("%+v", g)
	}
	// repository housekeeping does not see or touch deliveries/
	if _, err := e.n.Svc.GC(ctx, "custody", service.MinGCGrace, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.n.Svc.VerifyJob(ctx, "custody", service.VerifyRequest{Mode: "all"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.n.Svc.Reconcile(ctx, "custody"); err != nil {
		t.Fatal(err)
	}
	st, _ := e.n.Svc.Store(e.n.Repo)
	if _, err := st.CleanOrphans(0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CleanTmp(0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.js.Maintain(ctx); err != nil { // not expired yet, job finished: kept
		t.Fatal(err)
	}
	r, size, sum, err := e.js.OpenDelivery(ctx, j.ID)
	if err != nil || size != int64(len(payload)) || sum != rc.SHA256 {
		t.Fatalf("%v %d %s", err, size, sum)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("delivery round trip: %v (%d bytes)", err, len(got))
	}
	// a damaged part is detected by the reader, never relayed as complete
	parts := storeKeys(t, e, "deliveries/"+j.ID+"/1/")
	if err := st.Backend().Put(parts[1], bytes.Repeat([]byte{0}, 1<<20), false); err != nil {
		t.Fatal(err)
	}
	r, _, _, _ = e.js.OpenDelivery(ctx, j.ID)
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("a corrupted delivery was read without error")
	}
	// after the TTL the reaper deletes it
	e.n.Now = e.n.Now.Add(61 * time.Minute)
	if st, err := e.js.Maintain(ctx); err != nil || st.Deliveries != 1 {
		t.Fatalf("%v %+v", err, st)
	}
	if _, _, _, err := e.js.OpenDelivery(ctx, j.ID); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("expired delivery: %v", err)
	}
	if keys := storeKeys(t, e, "deliveries/"); len(keys) != 0 {
		t.Fatalf("reaper left %v", keys)
	}
}

func TestSealedDeliveryRoundTripAndReaperFilesystem(t *testing.T) { testDelivery(t, false) }
func TestSealedDeliveryRoundTripAndReaperS3(t *testing.T)         { testDelivery(t, true) }

func TestDeliveryRulesAndOrphans(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	// a job type without a delivery
	e.create("benfica", "deep_verify", nil)
	dv, _ := e.a.NextJob(ctx, 0, nil)
	if _, err := e.a.PutDelivery(ctx, dv.ID, dv.Attempt, strings.NewReader("x")); !remote.IsStatus(err, 400) {
		t.Fatalf("delivery for deep_verify: %v", err)
	}
	j := deliveryJob(e)
	if _, err := e.a.PutDelivery(ctx, j.ID, j.Attempt+1, strings.NewReader("x")); !remote.IsStatus(err, 409) {
		t.Fatalf("wrong attempt: %v", err)
	}
	if _, err := e.a.PutDelivery(ctx, j.ID, j.Attempt, strings.NewReader("")); !remote.IsStatus(err, 400) {
		t.Fatalf("empty delivery: %v", err)
	}
	// success without a delivery is turned into a failure
	res, _ := json.Marshal(jobspec.DeliveryResult{Size: 5, SHA256: "00"})
	if err := e.a.FinishJob(ctx, j.ID, jobspec.Finish{Attempt: 1, State: "succeeded", Result: res}); err != nil {
		t.Fatal(err)
	}
	if g := e.get(j.ID); g.State != "failed" || !strings.Contains(g.Message, "no complete delivery") {
		t.Fatalf("%+v", g)
	}
	// a checksum mismatch also fails
	j2 := deliveryJob(e)
	if _, err := e.a.PutDelivery(ctx, j2.ID, 1, strings.NewReader("sealed bytes")); err != nil {
		t.Fatal(err)
	}
	res, _ = json.Marshal(jobspec.DeliveryResult{Size: 12, SHA256: "deadbeef"})
	e.a.FinishJob(ctx, j2.ID, jobspec.Finish{Attempt: 1, State: "succeeded", Result: res})
	if g := e.get(j2.ID); g.State != "failed" || !strings.Contains(g.Message, "differs") {
		t.Fatalf("%+v", g)
	}
	if _, _, _, err := e.js.OpenDelivery(ctx, j2.ID); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("a failed job's delivery must not be handed out: %v", err)
	}
	if keys := storeKeys(t, e, "deliveries/"+j2.ID+"/"); len(keys) != 0 {
		t.Fatalf("a failed job's delivery was kept: %v", keys)
	}
	// nor the delivery of a job that is still running
	jr := deliveryJob(e)
	if _, err := e.a.PutDelivery(ctx, jr.ID, 1, strings.NewReader("sealed")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.js.OpenDelivery(ctx, jr.ID); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("a running job's delivery must not be handed out: %v", err)
	}
	e.a.FinishJob(ctx, jr.ID, jobspec.Finish{Attempt: 1, State: "cancelled"})
	// orphaned parts (an upload whose job is gone) are removed; a running upload's are not
	st, _ := e.n.Svc.Store(e.n.Repo)
	b := st.Backend()
	j3 := deliveryJob(e)
	orphan := fmt.Sprintf("deliveries/%s/7/p-000000", j.ID)
	inflight := fmt.Sprintf("deliveries/%s/1/p-000000", j3.ID)
	for _, k := range []string{orphan, inflight} {
		if err := b.Put(k, []byte("x"), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.js.ReapDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	keys := strings.Join(storeKeys(t, e, "deliveries/"), ",")
	if strings.Contains(keys, orphan) || !strings.Contains(keys, inflight) {
		t.Fatalf("orphan reaping wrong: %s", keys)
	}
	// a re-queued attempt loses its partial parts
	e.n.Now = e.n.Now.Add(6 * time.Minute)
	e.js.Maintain(ctx)
	if keys := storeKeys(t, e, "deliveries/"+j3.ID+"/"); len(keys) != 0 {
		t.Fatalf("partial parts of an expired attempt left: %v", keys)
	}
}

func TestDeliverySizeCap(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	oldMax, oldPart := jobs.MaxDeliveryBytes, jobs.DeliveryPartSize
	jobs.MaxDeliveryBytes, jobs.DeliveryPartSize = 3000, 1000
	t.Cleanup(func() { jobs.MaxDeliveryBytes, jobs.DeliveryPartSize = oldMax, oldPart })
	j := deliveryJob(e)
	// chunked (no Content-Length): the cap is enforced while streaming
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 10; i++ {
			if _, err := pw.Write(make([]byte, 1000)); err != nil {
				return
			}
		}
		pw.Close()
	}()
	if _, err := e.a.PutDelivery(ctx, j.ID, 1, pr); !remote.IsStatus(err, 400) {
		t.Fatalf("an oversized delivery must be refused with 400: %v", err)
	}
	pr.Close()
	if keys := storeKeys(t, e, "deliveries/"); len(keys) != 0 {
		t.Fatalf("parts of a refused upload left: %v", keys)
	}
	// with a Content-Length the request is refused before reading
	if _, err := e.a.PutDelivery(ctx, j.ID, 1, bytes.NewReader(make([]byte, 3001))); !remote.IsStatus(err, 400) {
		t.Fatalf("%v", err)
	}
	// exactly at the cap works
	rc, err := e.a.PutDelivery(ctx, j.ID, 1, bytes.NewReader(make([]byte, 3000)))
	if err != nil || rc.Size != 3000 {
		t.Fatalf("%v %+v", err, rc)
	}
}

func TestLogIsCapped(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	e.create("benfica", "deep_verify", nil)
	j, _ := e.a.NextJob(ctx, 0, nil)
	for i := 0; i < 6; i++ {
		var lines []jobspec.LogLine
		for k := 0; k < jobspec.MaxLinesPerCall; k++ {
			lines = append(lines, jobspec.LogLine{Line: strings.Repeat("y", 3000)})
		}
		if _, err := e.a.JobProgress(ctx, j.ID, jobspec.Progress{Attempt: 1, Log: lines}); err != nil {
			t.Fatal(err)
		}
	}
	g := e.get(j.ID)
	if len(g.Log) != jobspec.MaxLogLines+1 || !strings.Contains(g.Log[len(g.Log)-1].Line, "truncated") {
		t.Fatalf("%d lines, last %q", len(g.Log), g.Log[len(g.Log)-1].Line)
	}
	if len(g.Log[5].Line) > jobspec.MaxLogLineLen+4 {
		t.Fatalf("line not bounded: %d", len(g.Log[5].Line))
	}
	var tooMany []jobspec.LogLine
	for k := 0; k <= jobspec.MaxLinesPerCall; k++ {
		tooMany = append(tooMany, jobspec.LogLine{Line: "z"})
	}
	if _, err := e.a.JobProgress(ctx, j.ID, jobspec.Progress{Attempt: 1, Log: tooMany}); !remote.IsStatus(err, 400) {
		t.Fatalf("%v", err)
	}
}

func TestRetentionKeepsRecentFinishedJobsAndRestoreTests(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	e.js.KeepPerRepo = 3
	defer func() { e.js.KeepPerRepo = 1000 }()
	var ids []string
	for i := 0; i < 5; i++ {
		j := e.create("benfica", "restore_test", rt)
		e.a.NextJob(ctx, 0, nil)
		e.n.Now = e.n.Now.Add(time.Minute)
		if err := e.a.FinishJob(ctx, j.ID, jobspec.Finish{Attempt: 1, State: "failed", Message: "x"}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	running := e.create("benfica", "deep_verify", nil)
	if st, err := e.js.Maintain(ctx); err != nil || st.Deleted != 2 {
		t.Fatalf("%v %+v", err, st)
	}
	if _, err := e.js.GetJob(ctx, ids[0]); !errors.Is(err, service.ErrNotFound) {
		t.Fatal("oldest job kept")
	}
	if _, err := e.js.GetJob(ctx, ids[4]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.js.GetJob(ctx, running.ID); err != nil {
		t.Fatal("a queued job must never be deleted by retention")
	}
	tests, _ := e.js.ListRestoreTests(ctx, jobs.RestoreTestFilter{Repository: "custody"})
	if len(tests) != 5 {
		t.Fatalf("restore test records must outlive their jobs: %d", len(tests))
	}
	// 30 days later everything finished goes
	e.n.Now = e.n.Now.Add(31 * 24 * time.Hour)
	e.js.KeepPerRepo = 1000
	if st, _ := e.js.Maintain(ctx); st.Deleted != 3 || st.Expired != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestHubIsNonBlockingAndFlagsLag(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	ch, stop := e.js.Subscribe()
	defer stop()
	slow, stopSlow := e.js.Subscribe()
	j := e.create("benfica", "deep_verify", nil)
	ev := <-ch
	if ev.JobID != j.ID || ev.State != "queued" || ev.Host != "benfica" || ev.Repository != "custody" {
		t.Fatalf("%+v", ev)
	}
	e.a.NextJob(ctx, 0, nil)
	if ev := <-ch; ev.State != "running" || ev.Attempt != 1 {
		t.Fatalf("%+v", ev)
	}
	// flood: the slow subscriber never reads, publishers never block
	done := make(chan struct{})
	go func() {
		for i := 0; i < jobs.SubscriberBuffer+20; i++ {
			e.a.JobProgress(ctx, j.ID, jobspec.Progress{Attempt: 1, Progress: i % 100})
		}
		close(done)
	}()
	for {
		select {
		case <-ch:
			continue
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("publishing blocked on a slow subscriber")
		}
		break
	}
	n, lagged := 0, false
	for len(slow) > 0 {
		<-slow
		n++
	}
	e.a.JobProgress(ctx, j.ID, jobspec.Progress{Attempt: 1, Progress: 1})
	ev = <-slow
	lagged = ev.Lagged
	if n != jobs.SubscriberBuffer || !lagged {
		t.Fatalf("buffered %d, lagged %v", n, lagged)
	}
	stopSlow()
	stopSlow() // idempotent
	if _, ok := <-slow; ok {
		t.Fatal("channel not closed")
	}
}

func TestScheduledRestoreTestReportsAndLatestPerObject(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	at := e.n.Now
	inv := api.Inventory{}
	for i, name := range []string{"volume/a", "volume/b"} {
		inv.Objects = append(inv.Objects, api.ObjectInfo{Project: "default", Name: name, Kind: "filesystem", GroupID: fmt.Sprintf("%032x", i+1)})
	}
	if err := e.a.PutInventory(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if err := e.b.PutInventory(ctx, api.Inventory{Objects: []api.ObjectInfo{{Project: "default", Name: "volume/p", Kind: "filesystem", GroupID: fmt.Sprintf("%032x", 99)}}}); err != nil {
		t.Fatal(err)
	}
	report := func(obj string, passed bool, start time.Time) {
		t.Helper()
		err := e.a.ReportRestoreTest(ctx, jobspec.RestoreTestReport{Result: jobspec.RestoreTestResult{Project: "default", Object: obj, Method: "extract", Passed: passed,
			Snapshot: "20261007120000-0a0a0a0a", Files: 7, Details: map[string]any{"disk_files": 7}}, Started: start, Finished: start.Add(time.Second)})
		if err != nil {
			t.Fatal(err)
		}
	}
	report("volume/a", false, at.Add(-2*time.Hour))
	report("volume/a", true, at.Add(-time.Hour))
	report("volume/b", false, at.Add(-time.Hour))
	if err := e.a.ReportRestoreTest(ctx, jobspec.RestoreTestReport{Result: jobspec.RestoreTestResult{Project: "default", Object: "volume/a", Method: "magic"}, Started: at, Finished: at}); !remote.IsStatus(err, 400) {
		t.Fatalf("bad method: %v", err)
	}
	// evidence only for the host's own objects
	forged := jobspec.RestoreTestReport{Result: jobspec.RestoreTestResult{Project: "default", Object: "volume/p", Method: "deep", Passed: true}, Started: at, Finished: at}
	if err := e.a.ReportRestoreTest(ctx, forged); !remote.IsStatus(err, 403) {
		t.Fatalf("a host reported a test for another host's object: %v", err)
	}
	forged.Result.Object = "volume/unknown"
	if err := e.a.ReportRestoreTest(ctx, forged); !remote.IsStatus(err, 404) {
		t.Fatalf("unknown object: %v", err)
	}
	// a running job-driven test does not hide the last finished result
	e.create("benfica", "restore_test", jobspec.RestoreTest{Project: "default", Object: "volume/a", Mode: "extract"})
	e.a.NextJob(ctx, 0, nil)
	latest, err := e.js.LatestRestoreTestPerObject(ctx, "custody")
	if err != nil || len(latest) != 2 || latest[0].Object != "default/volume/a" || latest[0].State != "passed" || latest[1].State != "failed" || latest[0].Files != 7 {
		t.Fatalf("%v %+v", err, latest)
	}
	all, _ := e.js.ListRestoreTests(ctx, jobs.RestoreTestFilter{Repository: "custody", Object: "default/volume/a"})
	if len(all) != 3 || all[0].State != "running" {
		t.Fatalf("%+v", all)
	}
}
