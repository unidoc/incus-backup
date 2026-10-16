package datastore

// Benchmarks of the repository's request patterns against an S3 service (the in-process fake by default) behind a
// TCP delay line (internal/bench), so the cost of one request round trip is visible on one machine.
// They are the measurements behind docs/performance.md; `just bench` runs them.
//
// Sizes come from the environment so the default run stays short:
//
//	INCUS_BACKUP_BENCH_N    chunk counts            (default 11000)
//	INCUS_BACKUP_BENCH_RTT  round trips in ms       (default 0,20)
//	INCUS_BACKUP_BENCH_PAR  request parallelism     (default 1,32)
//
// Run with -benchtime=1x: one op is one whole Missing/Commit/GC/Verify over all N chunks.

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/bench"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/s3test"
)

// countingBackend counts requests by kind and the highest number of Stat/Get calls in flight.
type countingBackend struct {
	Backend
	stats, gets, puts, lists, deletes atomic.Int64
	inflight, maxInflight             atomic.Int64
	delay                             time.Duration // added to every Stat/Get (tests of parallelism)
}

func (c *countingBackend) enter() func() {
	n := c.inflight.Add(1)
	for {
		m := c.maxInflight.Load()
		if n <= m || c.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	return func() { c.inflight.Add(-1) }
}

func (c *countingBackend) Stat(k string) (Info, error) {
	c.stats.Add(1)
	defer c.enter()()
	return c.Backend.Stat(k)
}

func (c *countingBackend) Get(k string) ([]byte, error) {
	c.gets.Add(1)
	defer c.enter()()
	return c.Backend.Get(k)
}

func (c *countingBackend) Put(k string, d []byte, ifAbsent bool) error {
	c.puts.Add(1)
	return c.Backend.Put(k, d, ifAbsent)
}

func (c *countingBackend) List(p string, fn func(string, Info) error) error {
	c.lists.Add(1)
	return c.Backend.List(p, fn)
}

func (c *countingBackend) Delete(k string) error {
	c.deletes.Add(1)
	return c.Backend.Delete(k)
}

func (c *countingBackend) reset() {
	c.stats.Store(0)
	c.gets.Store(0)
	c.puts.Store(0)
	c.lists.Store(0)
	c.deletes.Store(0)
	c.maxInflight.Store(0)
}

func benchKeys(tb testing.TB) *format.Keys {
	m := make([]byte, 32)
	for i := range m {
		m[i] = byte(100 + i)
	}
	k, err := format.DeriveKeys(m)
	if err != nil {
		tb.Fatal(err)
	}
	return k
}

// s3Backend connects to the service at addr (normally a delay proxy) with production transport settings.
func s3BackendAt(tb testing.TB, addr, bucket string, svc s3test.Config) *S3Backend {
	tb.Helper()
	b, err := NewS3Backend(S3Config{Endpoint: addr, Bucket: bucket, Prefix: "repo", AccessKey: svc.AccessKey, SecretKey: svc.SecretKey, NoTLS: true, PathStyle: true})
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// population is a bucket holding a repository with n small chunks.
type population struct {
	svc     s3test.Config
	bucket  string
	present []format.ID // stored chunks
	absent  []format.ID // ids that are not stored
	blobs   map[format.ID][]byte
}

// populate creates a bucket with a repository and stores nPresent tiny chunks, uploading in
// parallel straight to the service (no delay line). nAbsent ids are made up and never stored.
func populate(tb testing.TB, nPresent, nAbsent int) *population {
	tb.Helper()
	svc, bucket := s3test.NewBucket(tb)
	raw := s3BackendAt(tb, svc.Endpoint, bucket, svc)
	if _, err := InitBackend(raw); err != nil {
		tb.Fatal(err)
	}
	k := benchKeys(tb)
	p := &population{svc: svc, bucket: bucket, blobs: map[format.ID][]byte{}}
	for i := 0; i < nPresent; i++ {
		id, blob, err := k.EncodeChunk([]byte(fmt.Sprintf("bench chunk %d %d", nPresent, i)), format.EncodeOpts{})
		if err != nil {
			tb.Fatal(err)
		}
		p.present = append(p.present, id)
		p.blobs[id] = blob
	}
	for i := 0; i < nAbsent; i++ {
		var id format.ID
		_, _ = rand.Read(id[:])
		p.absent = append(p.absent, id)
	}
	var wg sync.WaitGroup
	var bad atomic.Value
	work := make(chan format.ID)
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range work {
				if err := raw.Put(chunkKey(id), p.blobs[id], false); err != nil {
					bad.Store(err)
				}
			}
		}()
	}
	for _, id := range p.present {
		work <- id
	}
	close(work)
	wg.Wait()
	if err, _ := bad.Load().(error); err != nil {
		tb.Fatal(err)
	}
	return p
}

// open opens the population's repository through a delay line of the given RTT.
func (p *population) open(tb testing.TB, rtt time.Duration) (*Store, *countingBackend, *bench.DelayProxy) {
	tb.Helper()
	px, err := bench.NewDelayProxy(p.svc.Endpoint, rtt)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(px.Close)
	cb := &countingBackend{Backend: s3BackendAt(tb, px.Addr(), p.bucket, p.svc)}
	s, err := OpenBackend(cb)
	if err != nil {
		tb.Fatal(err)
	}
	return s, cb, px
}

func benchSizes() (ns []int, rtts []time.Duration, pars []int) {
	return bench.Ints("INCUS_BACKUP_BENCH_N", 11000),
		bench.Durations("INCUS_BACKUP_BENCH_RTT", 0, 20*time.Millisecond),
		bench.Ints("INCUS_BACKUP_BENCH_PAR", 1, 32)
}

func report(b *testing.B, cb *countingBackend, px *bench.DelayProxy, items int) {
	n := int64(b.N)
	b.ReportMetric(float64(cb.stats.Load())/float64(n), "HEAD/op")
	b.ReportMetric(float64(cb.gets.Load())/float64(n), "GET/op")
	b.ReportMetric(float64(cb.lists.Load())/float64(n), "LIST/op")
	b.ReportMetric(float64(px.Conns.Load()), "conns")
	b.ReportMetric(float64(items)*float64(b.N)/b.Elapsed().Seconds(), "ids/s")
}

// Missing of an incremental backup: 90 % of the ids are present. presence=1 loads the presence
// set first (its full listing is reported as a separate metric, "list-ms").
func BenchmarkS3Missing(b *testing.B) {
	ns, rtts, pars := benchSizes()
	presence := bench.Ints("INCUS_BACKUP_BENCH_PRESENCE", 0)
	for _, n := range ns {
		pop := populate(b, n-n/10, n/10)
		ids := append(append([]format.ID(nil), pop.present...), pop.absent...)
		for _, rtt := range rtts {
			for _, par := range pars {
				for _, pr := range presence {
					b.Run(fmt.Sprintf("n=%d/rtt=%s/par=%d/presence=%d", n, rtt, par, pr), func(b *testing.B) {
						s, cb, px := pop.open(b, rtt)
						setBenchParallel(s, par)
						var listed time.Duration
						if pr == 1 {
							t0 := time.Now()
							if err := setBenchPresence(s); err != nil {
								b.Fatal(err)
							}
							listed = time.Since(t0)
						}
						cb.reset()
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							miss, err := s.Missing(ids)
							if err != nil {
								b.Fatal(err)
							}
							if len(miss) != len(pop.absent) {
								b.Fatalf("missing %d, want %d", len(miss), len(pop.absent))
							}
						}
						b.StopTimer()
						report(b, cb, px, len(ids))
						if pr == 1 { // after ResetTimer, which drops reported metrics
							b.ReportMetric(float64(listed.Milliseconds()), "list-ms")
						}
					})
				}
			}
		}
	}
}

func benchIndex(tb testing.TB, k *format.Keys, group, snap string, ids []format.ID) (idx, man []byte) {
	x := &format.Index{Kind: format.IndexDynamic}
	for i, id := range ids {
		x.Entries = append(x.Entries, format.Entry{End: uint64(i+1) * 64, ID: id})
	}
	x.Size = uint64(len(ids)) * 64
	idx, err := x.Marshal()
	if err != nil {
		tb.Fatal(err)
	}
	m := &format.Manifest{SnapshotID: snap, Created: time.Now().UTC(), Object: format.ObjectInfo{Name: "o", Kind: "filesystem", Size: x.Size}, IndexDigest: format.IndexDigest(idx)}
	man, err = k.SealManifest(m, group, nil)
	if err != nil {
		tb.Fatal(err)
	}
	return idx, man
}

var benchSnapSeq atomic.Int64

func nextSnapID() string {
	v := benchSnapSeq.Add(1)
	return fmt.Sprintf("2026100%07d-%08x", v%10000000, uint32(v))
}

// Commit of a snapshot whose n chunks are all present (the existence check dominates).
func BenchmarkS3Commit(b *testing.B) {
	ns, rtts, pars := benchSizes()
	k := benchKeys(b)
	for _, n := range ns {
		pop := populate(b, n, 0)
		for _, rtt := range rtts {
			for _, par := range pars {
				b.Run(fmt.Sprintf("n=%d/rtt=%s/par=%d", n, rtt, par), func(b *testing.B) {
					s, cb, px := pop.open(b, rtt)
					setBenchParallel(s, par)
					type prepared struct {
						sess *Session
					}
					var ps []prepared
					for i := 0; i < b.N; i++ {
						snap := nextSnapID()
						idx, man := benchIndex(b, k, g1, snap, pop.present)
						sess, err := s.BeginSession(g1, snap)
						if err != nil {
							b.Fatal(err)
						}
						if err := sess.PutIndex(idx); err != nil {
							b.Fatal(err)
						}
						if err := sess.PutManifest(man); err != nil {
							b.Fatal(err)
						}
						ps = append(ps, prepared{sess})
					}
					cb.reset()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if _, err := ps[i].sess.Commit("client", time.Time{}); err != nil {
							b.Fatal(err)
						}
					}
					b.StopTimer()
					report(b, cb, px, n)
				})
			}
		}
	}
}

// maintenancePopulation: n chunks, referenced by snaps snapshots of n/snaps chunks each.
func maintenancePopulation(b *testing.B, n, snaps int) *population {
	pop := populate(b, n, 0)
	raw := s3BackendAt(b, pop.svc.Endpoint, pop.bucket, pop.svc)
	s, err := OpenBackend(raw)
	if err != nil {
		b.Fatal(err)
	}
	k := benchKeys(b)
	per := n / snaps
	for i := 0; i < snaps; i++ {
		snap := nextSnapID()
		idx, man := benchIndex(b, k, g1, snap, pop.present[i*per:(i+1)*per])
		sess, err := s.BeginSession(g1, snap)
		if err != nil {
			b.Fatal(err)
		}
		_ = sess.PutIndex(idx)
		_ = sess.PutManifest(man)
		if _, err := sess.Commit("client", time.Time{}); err != nil {
			b.Fatal(err)
		}
	}
	return pop
}

// GC, Verify and Usage over n chunks referenced by 20 snapshots.
func BenchmarkS3Maintenance(b *testing.B) {
	ns, rtts, pars := benchSizes()
	for _, n := range ns {
		pop := maintenancePopulation(b, n, 20)
		for _, rtt := range rtts {
			for _, par := range pars {
				name := fmt.Sprintf("n=%d/rtt=%s/par=%d", n, rtt, par)
				b.Run("gc/"+name, func(b *testing.B) {
					s, cb, px := pop.open(b, rtt)
					setBenchParallel(s, par)
					cb.reset()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						st, err := s.GC(context.Background(), time.Hour, false)
						if err != nil {
							b.Fatal(err)
						}
						if st.Live != n || st.Removed != 0 {
							b.Fatalf("gc: %+v", st)
						}
					}
					b.StopTimer()
					report(b, cb, px, n)
				})
				b.Run("verify/"+name, func(b *testing.B) {
					s, cb, px := pop.open(b, rtt)
					setBenchParallel(s, par)
					cb.reset()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						vs, err := s.VerifyWith(context.Background(), VerifyOpts{AllChunks: true})
						if err != nil {
							b.Fatal(err)
						}
						if len(vs.Problems) != 0 || vs.Chunks != n {
							b.Fatalf("verify: %d chunks, problems %v", vs.Chunks, vs.Problems)
						}
					}
					b.StopTimer()
					report(b, cb, px, n)
				})
				b.Run("usage/"+name, func(b *testing.B) {
					s, cb, px := pop.open(b, rtt)
					setBenchParallel(s, par)
					cb.reset()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						_, c, err := s.Usage()
						if err != nil {
							b.Fatal(err)
						}
						if c != int64(n) {
							b.Fatalf("usage: %d chunks", c)
						}
					}
					b.StopTimer()
					report(b, cb, px, n)
				})
			}
		}
	}
}
