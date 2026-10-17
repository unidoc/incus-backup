package engine_test

// Upload throughput client -> server over real mutual TLS (the production HTTP server), through a
// TCP delay line, to a repository on the filesystem or on S3. One op uploads every chunk of a
// snapshot of incompressible data (nothing is present on the server) and commits it.
//
//	INCUS_BACKUP_BENCH_UPLOAD_MB   payload size in MiB          (default 256)
//	INCUS_BACKUP_BENCH_WORKERS     upload workers               (default 4,16)
//	INCUS_BACKUP_BENCH_RTT         client<->server RTT in ms    (default 0,20)
//	INCUS_BACKUP_BENCH_STORE       fs,s3                        (default fs)

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/bench"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func BenchmarkUpload(b *testing.B) {
	mb := bench.Ints("INCUS_BACKUP_BENCH_UPLOAD_MB", 256)[0]
	workers := bench.Ints("INCUS_BACKUP_BENCH_WORKERS", 4, 16)
	rtts := bench.Durations("INCUS_BACKUP_BENCH_RTT", 0, 20*time.Millisecond)
	stores := strings.Split(strings.TrimSpace(envOr("INCUS_BACKUP_BENCH_STORE", "fs")), ",")
	keys := masterKeys(9)
	sp, err := engine.NewSpool(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer sp.Close()
	size := uint64(mb) << 20
	idx := format.NewFixedIndex(size, format.FixedChunkSize)
	r := mrand.New(mrand.NewPCG(3, 4))
	buf := make([]byte, format.FixedChunkSize)
	for i := range idx.Entries {
		for j := 0; j+8 <= len(buf); j += 8 {
			v := r.Uint64()
			for k := 0; k < 8; k++ {
				buf[j+k] = byte(v >> (8 * k))
			}
		}
		id, blob, err := keys.EncodeChunk(buf, format.EncodeOpts{})
		if err != nil {
			b.Fatal(err)
		}
		if err := sp.Put(keys.DomainID, id, blob); err != nil {
			b.Fatal(err)
		}
		idx.Entries[i].ID = id
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, store := range stores {
		for _, rtt := range rtts {
			for _, w := range workers {
				b.Run(fmt.Sprintf("store=%s/rtt=%s/workers=%d/MiB=%d", store, rtt, w, mb), func(b *testing.B) {
					for i := 0; i < b.N; i++ {
						b.StopTimer()
						var n *testsupport.Node
						if store == "s3" {
							n = testsupport.NewS3Node(b)
						} else {
							n = testsupport.NewNode(b)
						}
						cr := n.Enrol("benfica", api.ScopeBackup)
						px, err := bench.NewDelayProxy(n.Addr(), rtt)
						if err != nil {
							b.Fatal(err)
						}
						cl, err := remote.New(px.Addr(), cr.CA, cr.Cert, cr.Key, keys.DomainID)
						if err != nil {
							b.Fatal(err)
						}
						e := &engine.Engine{Host: "benfica", Log: log, Now: time.Now}
						setUploadWorkers(e, w)
						t := &engine.Target{Name: "t", Client: cl, Keys: keys}
						created := time.Now().UTC()
						snap, _ := format.NewSnapshotID(created, nil)
						p := &engine.Prepared{Obj: &engine.Object{Project: "default", Name: "instance/bench/root", Kind: engine.KindBlock, Policy: "p"},
							SnapID: snap, Created: created, Size: size, Full: true,
							ByDomain: map[string]*engine.DomainSnapshot{keys.DomainID: {Index: idx, Keys: keys}}}
						b.StartTimer()
						sent, err := e.Upload(context.Background(), t, p, sp)
						b.StopTimer()
						if err != nil {
							b.Fatal(err)
						}
						if sent < int64(size) {
							b.Fatalf("sent %d of %d bytes", sent, size)
						}
						b.ReportMetric(float64(px.Conns.Load()), "conns")
						px.Close()
						b.StartTimer()
					}
					b.SetBytes(int64(size))
				})
			}
		}
	}
}

// An incremental upload: every chunk of a large index is already on the server, so the run is
// nothing but "which chunks are missing" questions and the commit (the existence check of every
// chunk). INCUS_BACKUP_BENCH_IDS sets the index size (default 50000 small chunks).
func BenchmarkUploadIncremental(b *testing.B) {
	nids := bench.Ints("INCUS_BACKUP_BENCH_IDS", 50000)[0]
	rtts := bench.Durations("INCUS_BACKUP_BENCH_RTT", 0, 20*time.Millisecond)
	stores := strings.Split(strings.TrimSpace(envOr("INCUS_BACKUP_BENCH_STORE", "fs")), ",")
	keys := masterKeys(10)
	sp, err := engine.NewSpool(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer sp.Close()
	idx := &format.Index{Kind: format.IndexDynamic}
	for i := 0; i < nids; i++ {
		data := []byte(fmt.Sprintf("incremental bench chunk %d", i))
		id, blob, err := keys.EncodeChunk(data, format.EncodeOpts{})
		if err != nil {
			b.Fatal(err)
		}
		if err := sp.Put(keys.DomainID, id, blob); err != nil {
			b.Fatal(err)
		}
		idx.Size += uint64(len(data))
		idx.Entries = append(idx.Entries, format.Entry{End: idx.Size, ID: id})
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	obj := &engine.Object{Project: "default", Name: "volume/incremental", Kind: engine.KindFilesystem, Policy: "p"}
	for _, store := range stores {
		var n *testsupport.Node
		if store == "s3" {
			n = testsupport.NewS3Node(b)
		} else {
			n = testsupport.NewNode(b)
		}
		cr := n.Enrol("benfica", api.ScopeBackup)
		direct, err := remote.New(n.Addr(), cr.CA, cr.Cert, cr.Key, keys.DomainID)
		if err != nil {
			b.Fatal(err)
		}
		e := &engine.Engine{Host: "benfica", Log: log, Now: time.Now}
		setUploadWorkers(e, 16)
		at := time.Now().UTC().Add(-time.Hour)
		upload := func(cl *remote.Client) int64 {
			at = at.Add(time.Second)
			snap, _ := format.NewSnapshotID(at, nil)
			p := &engine.Prepared{Obj: obj, SnapID: snap, Created: at, Size: idx.Size,
				ByDomain: map[string]*engine.DomainSnapshot{keys.DomainID: {Index: idx, Keys: keys}}}
			sent, err := e.Upload(context.Background(), &engine.Target{Name: "t", Client: cl, Keys: keys}, p, sp)
			if err != nil {
				b.Fatal(err)
			}
			return sent
		}
		upload(direct) // the first (full) upload, not measured
		for _, rtt := range rtts {
			b.Run(fmt.Sprintf("store=%s/rtt=%s/ids=%d", store, rtt, nids), func(b *testing.B) {
				px, err := bench.NewDelayProxy(n.Addr(), rtt)
				if err != nil {
					b.Fatal(err)
				}
				defer px.Close()
				cl, err := remote.New(px.Addr(), cr.CA, cr.Cert, cr.Key, keys.DomainID)
				if err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if sent := upload(cl); sent != 0 {
						b.Fatalf("an incremental upload sent %d bytes", sent)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(nids)*float64(b.N)/b.Elapsed().Seconds(), "ids/s")
			})
		}
	}
}
