package datastore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/s3test"
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

// A whole life of a repository on S3 - sessions that reuse chunks, commit, GC with pinned and unpinned
// chunks, verify, quarantine, protect, delete - never issues an S3 COPY: nothing is rewritten to
// keep it "alive", so no extra requests and no new object versions.
func TestS3NeverIssuesACopyRequest(t *testing.T) {
	svc, bucket := s3test.NewBucket(t)
	p := newFaultProxy(t, svc.Endpoint)
	s := s3Via(t, p, bucket, svc)
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
	if p.sawCopy.Load() {
		t.Fatal("an S3 COPY request was issued")
	}
	if p.conns.Load() < 20 {
		t.Fatalf("the proxy saw only %d connections: the watcher was not exercised", p.conns.Load())
	}
	// negative control: the watcher does notice a real COPY
	sb := s.Backend().(*S3Backend)
	_, err := sb.c.CopyObject(context.Background(),
		minio.CopyDestOptions{Bucket: sb.bucket, Object: sb.prefix + "copy-probe"},
		minio.CopySrcOptions{Bucket: sb.bucket, Object: sb.prefix + metaKey})
	if err != nil {
		t.Fatal(err)
	}
	if !p.sawCopy.Load() {
		t.Fatal("the COPY watcher is blind: the test above proves nothing")
	}
}
