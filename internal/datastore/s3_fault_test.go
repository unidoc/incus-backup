package datastore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/s3test/s3fake"
)

// faultProxy sits between the backend and the real S3 service and breaks connections on demand.
type faultProxy struct {
	ln       net.Listener
	target   string
	conns    atomic.Int64
	dropEach atomic.Int64 // reset every n-th connection immediately (0 = never)
	cutAfter atomic.Int64 // when > 0: forward only this many client->server bytes, then reset (a torn upload)
	cutOnce  atomic.Bool
	sawCopy  atomic.Bool // a request carried x-amz-copy-source (an S3 COPY)
}

func newFaultProxy(t *testing.T, target string) *faultProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &faultProxy{ln: ln, target: target}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c)
		}
	}()
	return p
}

func (p *faultProxy) addr() string { return p.ln.Addr().String() }

func (p *faultProxy) serve(c net.Conn) {
	n := p.conns.Add(1)
	if de := p.dropEach.Load(); de > 0 && n%de == 0 {
		c.(*net.TCPConn).SetLinger(0)
		c.Close()
		return
	}
	s, err := net.Dial("tcp", p.target)
	if err != nil {
		c.Close()
		return
	}
	go io.Copy(c, s)
	watch := &copyWatcher{w: s, flag: &p.sawCopy}
	if ca := p.cutAfter.Load(); ca > 0 && p.cutOnce.CompareAndSwap(false, true) {
		io.CopyN(watch, c, ca)
		s.Close()
		c.(*net.TCPConn).SetLinger(0)
		c.Close()
		return
	}
	io.Copy(watch, c)
	s.Close()
	c.Close()
}

// copyWatcher forwards bytes and notes whether an S3 COPY header went by.
type copyWatcher struct {
	w    io.Writer
	flag *atomic.Bool
	tail []byte
}

func (c *copyWatcher) Write(b []byte) (int, error) {
	buf := append(c.tail, b...)
	if bytes.Contains(bytes.ToLower(buf), []byte("x-amz-copy-source")) {
		c.flag.Store(true)
	}
	if len(buf) > 32 {
		c.tail = append([]byte(nil), buf[len(buf)-32:]...)
	} else {
		c.tail = buf
	}
	return c.w.Write(b)
}

func s3Via(t *testing.T, proxy *faultProxy, bucket string, svc s3test.Config) *Store {
	t.Helper()
	s3TransportHook = func(tr *http.Transport) { tr.DisableKeepAlives = true }
	t.Cleanup(func() { s3TransportHook = nil })
	b, err := NewS3Backend(S3Config{Endpoint: proxy.addr(), Bucket: bucket, Prefix: "repo", AccessKey: svc.AccessKey, SecretKey: svc.SecretKey, NoTLS: true, PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitBackend(b); err != nil {
		t.Fatal(err)
	}
	s, err := OpenBackend(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Connections that die at random are retried by the client; a whole backup session still commits.
func TestS3RetriesBrokenConnectionsDuringASession(t *testing.T) {
	svc, bucket := s3test.NewBucket(t)
	p := newFaultProxy(t, svc.Endpoint)
	p.dropEach.Store(3)
	s := s3Via(t, p, bucket, svc)
	k := testKeys(t)
	var parts []string
	for i := 0; i < 25; i++ {
		parts = append(parts, fmt.Sprintf("chunk-%d-%s", i, "xxxxxxxxxxxxxxxxxxxxxxxx"))
	}
	snap := "20261007230000-0a0a0a0a"
	idx, man, ids := buildSnapshot(t, s, k, g1, snap, parts...)
	if err := commit(t, s, g1, snap, idx, man); err != nil {
		t.Fatalf("commit through a flaky network: %v", err)
	}
	if p.conns.Load() < 10 {
		t.Fatalf("the proxy saw only %d connections: the fault was not exercised", p.conns.Load())
	}
	for _, id := range ids {
		if !s.HasChunk(id) {
			t.Fatal("chunk lost")
		}
	}
	if err := s.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// A connection torn in the middle of an upload never leaves a half object: the key is absent
// (S3 PUT is atomic) or complete, and a retry of the same chunk succeeds.
func TestS3TornUploadLeavesNoPartialObject(t *testing.T) {
	svc, bucket := s3test.NewBucket(t)
	p := newFaultProxy(t, svc.Endpoint)
	s := s3Via(t, p, bucket, svc)
	k := testKeys(t)
	big := make([]byte, 3<<20)
	for i := range big {
		big[i] = byte(i * 7)
	}
	id, blob, _ := k.EncodeChunk(big, format.EncodeOpts{})
	p.cutAfter.Store(64 << 10) // reset the next connection after 64 KiB of the upload
	// the S3 client retries the torn attempt; what matters is that the tear really happened
	// and that the object that ended up in the bucket is whole
	created, err := s.PutChunk(id, blob)
	if err != nil || !created {
		t.Fatalf("PutChunk after a torn first attempt: %v %v", created, err)
	}
	got, err := s.GetChunk(id)
	if err != nil || len(got) != len(blob) {
		t.Fatalf("stored object is not complete: %v %d/%d", err, len(got), len(blob))
	}
	if err := s.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	if !p.cutOnce.Load() {
		t.Fatal("the tear was never injected")
	}
	if vs, err := s.Verify(context.Background()); err != nil || len(vs.Problems) != 0 {
		t.Fatalf("verify: %v %+v", err, vs)
	}
}

// A dead endpoint is an error, never a silent success, and nothing becomes visible.
func TestS3UnreachableEndpointFailsLoudly(t *testing.T) {
	svc, bucket := s3test.NewBucket(t)
	p := newFaultProxy(t, svc.Endpoint)
	s := s3Via(t, p, bucket, svc)
	k := testKeys(t)
	snap := "20261007230000-0a0a0a0a"
	idx, man, _ := buildSnapshot(t, s, k, g1, snap, "a")
	p.ln.Close() // the network goes away
	done := make(chan error, 1)
	go func() { done <- commit(t, s, g1, snap, idx, man) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("commit reported success with no network")
		}
	case <-time.After(3 * time.Minute):
		t.Fatal("commit hung")
	}
}

// exerciseWholeLife is a whole life of a repository on S3 - sessions that reuse chunks, commit, GC with pinned and
// unpinned chunks, verify, quarantine, protect, delete.
func exerciseWholeLife(t *testing.T, s *Store) {
	t.Helper()
	k := testKeys(t)
	snap1, snap2 := "20261007230000-0a0a0a0a", "20261007230001-0b0b0b0b"
	idx, man, ids := buildSnapshot(t, s, k, g1, snap1, "alpha", "beta", "gamma")
	if err := commit(t, s, g1, snap1, idx, man); err != nil {
		t.Fatal(err)
	}
	// a second session reuses every chunk ("present") and commits another snapshot
	sess, _ := s.BeginSession(g1, snap2)
	if miss, err := sess.Missing(ids); err != nil || len(miss) != 0 {
		t.Fatalf("%v %v", miss, err)
	}
	_ = sess.PutIndex(idx)
	_ = sess.PutManifest(man)
	if _, err := sess.Commit("client", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(context.Background(), time.Nanosecond, false); err != nil {
		t.Fatal(err)
	}
	_ = s.SetProtected(g1, snap1, true)
	_ = s.SetNotes(g1, snap1, "n")
	if _, err := s.VerifyWith(context.Background(), VerifyOpts{AllChunks: true, Record: true, Quarantine: true}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetProtected(g1, snap1, false)
	if err := s.DeleteSnapshot(g1, snap2); err != nil {
		t.Fatal(err)
	}
}

// copyProbe issues a real S3 COPY through the store's own client (the negative control of the tests below).
func copyProbe(t *testing.T, s *Store) {
	t.Helper()
	sb := s.Backend().(*S3Backend)
	_, err := sb.c.CopyObject(context.Background(),
		minio.CopyDestOptions{Bucket: sb.bucket, Object: sb.prefix + "copy-probe"},
		minio.CopySrcOptions{Bucket: sb.bucket, Object: sb.prefix + metaKey})
	if err != nil {
		t.Fatal(err)
	}
}

// A whole life of a repository on S3 never issues an S3 COPY: nothing is rewritten to
// keep it "alive", so no extra requests and no new object versions. (Observed with a TCP proxy, so it
// works against any S3 service the tests are pointed at.)
func TestS3NeverIssuesACopyRequest(t *testing.T) {
	svc, bucket := s3test.NewBucket(t)
	p := newFaultProxy(t, svc.Endpoint)
	s := s3Via(t, p, bucket, svc)
	exerciseWholeLife(t, s)
	if p.sawCopy.Load() {
		t.Fatal("an S3 COPY request was issued")
	}
	if p.conns.Load() < 20 {
		t.Fatalf("the proxy saw only %d connections: the watcher was not exercised", p.conns.Load())
	}
	// negative control: the watcher does notice a real COPY
	copyProbe(t, s)
	if !p.sawCopy.Load() {
		t.Fatal("the COPY watcher is blind: the test above proves nothing")
	}
}

// The same, observed from the server side: the in-process fake's request log names every operation.
func TestS3NeverIssuesACopyRequestFakeLog(t *testing.T) {
	srv, svc := s3test.NewServer(t)
	svc, bucket := s3test.NewBucketAt(t, svc)
	b, err := NewS3Backend(S3Config{Endpoint: svc.Endpoint, Bucket: bucket, Prefix: "repo", AccessKey: svc.AccessKey, SecretKey: svc.SecretKey, NoTLS: true, PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitBackend(b); err != nil {
		t.Fatal(err)
	}
	s, err := OpenBackend(b)
	if err != nil {
		t.Fatal(err)
	}
	srv.ResetLog()
	exerciseWholeLife(t, s)
	if n := srv.Count("CopyObject"); n != 0 {
		t.Fatalf("%d S3 COPY requests were issued", n)
	}
	if n := srv.Count("PutObject"); n < 10 {
		t.Fatalf("only %d PUTs were seen: the log was not exercised", n)
	}
	for _, r := range srv.Requests() {
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			t.Fatalf("a request carries x-amz-copy-source: %+v", r)
		}
	}
	for _, op := range []string{"UploadPart", "CreateMultipartUpload"} {
		if srv.Count(op) != 0 {
			t.Fatalf("the repository started using %s, which the S3 test server does not implement", op)
		}
	}
	// negative control: the log does show a real COPY
	copyProbe(t, s)
	if srv.Count("CopyObject") != 1 {
		t.Fatal("the request log is blind to COPY: the test above proves nothing")
	}
}

func s3OnFake(t *testing.T) (*s3test.Server, string, *Store) {
	t.Helper()
	srv, svc := s3test.NewServer(t)
	svc, bucket := s3test.NewBucketAt(t, svc)
	b, err := NewS3Backend(S3Config{Endpoint: svc.Endpoint, Bucket: bucket, Prefix: "repo", AccessKey: svc.AccessKey, SecretKey: svc.SecretKey, NoTLS: true, PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitBackend(b); err != nil {
		t.Fatal(err)
	}
	s, err := OpenBackend(b)
	if err != nil {
		t.Fatal(err)
	}
	return srv, bucket, s
}

// A burst of SlowDown answers (S3's "slow down" throttling) is ridden out by the client's retries and
// a whole session still commits.
func TestS3SlowDownBurstIsRiddenOut(t *testing.T) {
	srv, _, s := s3OnFake(t)
	k := testKeys(t)
	snap := "20261007230000-0a0a0a0a"
	idx, man, _ := buildSnapshot(t, s, k, g1, snap, "a", "b", "c", "d")
	r := srv.SlowDownBurst(3)
	r5 := srv.FailNext("PutObject", 2, 500, "InternalError")
	if err := commit(t, s, g1, snap, idx, man); err != nil {
		t.Fatalf("commit through a SlowDown burst: %v", err)
	}
	if r.Hits() != 3 || r5.Hits() != 2 {
		t.Fatalf("faults hit %d + %d times, want 3 + 2", r.Hits(), r5.Hits())
	}
	if err := s.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// A connection torn in the middle of an upload leaves NO object, ever: not while the client retries and
// not when every attempt fails; and the same chunk can be stored afterwards.
func TestS3TornUploadStoresNothingOnTheServer(t *testing.T) {
	old := minio.MaxRetry
	minio.MaxRetry = 3 // fewer retries: this test exhausts them on purpose
	defer func() { minio.MaxRetry = old }()
	srv, bucket, s := s3OnFake(t)
	k := testKeys(t)
	big := make([]byte, 2<<20)
	rand.New(rand.NewSource(7)).Read(big) // incompressible: the blob must be bigger than the tear point
	id, blob, _ := k.EncodeChunk(big, format.EncodeOpts{})
	key := "repo/" + chunkKey(id)
	// every attempt is torn: the PUT fails and nothing is stored
	rule := srv.AddRule(s3fake.Rule{Op: "PutObject", Key: "repo/chunks/", Action: s3fake.Action{DropAfterBytes: 100 << 10}})
	if _, err := s.PutChunk(id, blob); err == nil {
		t.Fatal("a chunk whose upload is torn on every attempt reported success")
	}
	if rule.Hits() < 2 {
		t.Fatalf("the tear was injected %d times", rule.Hits())
	}
	if _, ok := srv.Object(bucket, key); ok {
		t.Fatal("a torn upload left an object behind")
	}
	if s.HasChunk(id) {
		t.Fatal("the store believes a torn chunk is present")
	}
	// one torn attempt, then the retry succeeds: the object is whole
	rule.Remove()
	srv.TearNextUpload(100 << 10)
	created, err := s.PutChunk(id, blob)
	if err != nil || !created {
		t.Fatalf("PutChunk after one torn attempt: %v %v", created, err)
	}
	if got, ok := srv.Object(bucket, key); !ok || len(got) != len(blob) {
		t.Fatalf("stored object has %d bytes, want %d", len(got), len(blob))
	}
	if err := s.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// The answer to the commit marker's conditional PUT is lost (the write took effect, the client retries
// and sees 412): the commit must still end up committed, once, and consistent.
func TestS3LostResponseOfTheCommitMarker(t *testing.T) {
	srv, _, s := s3OnFake(t)
	k := testKeys(t)
	snap := "20261007230000-0a0a0a0a"
	idx, man, _ := buildSnapshot(t, s, k, g1, snap, "a", "b")
	rule := srv.AddRule(s3fake.Rule{Op: "PutObject", Func: func(r *s3fake.Request) bool { return strings.HasSuffix(r.Key, "/commit.json") },
		Times: 1, Action: s3fake.Action{DropResponse: true}})
	err := commit(t, s, g1, snap, idx, man)
	if rule.Hits() != 1 {
		t.Fatalf("the fault hit %d times: the commit marker is not named commit.json any more?", rule.Hits())
	}
	if err != nil {
		t.Fatalf("commit after a lost commit-marker response: %v", err)
	}
	if err := s.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}
