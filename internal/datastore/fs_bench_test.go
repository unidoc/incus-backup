package datastore

import (
	"fmt"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/unidoc/incus-backup/internal/format"
)

// Server-side cost of storing one chunk on a filesystem repository (the PUT handler's datastore
// part): trailer check, temp file write + fsync, link, directory fsync. Set
// INCUS_BACKUP_BENCH_FSDIR to measure another filesystem (default: the test temp dir).
func BenchmarkFSPutChunk(b *testing.B) {
	k := benchKeys(b)
	r := mrand.New(mrand.NewPCG(5, 6))
	for _, size := range []int{64 << 10, 4 << 20} {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(r.Uint32())
		}
		_, blob, err := k.EncodeChunk(data, format.EncodeOpts{NoCompress: true})
		if err != nil {
			b.Fatal(err)
		}
		for _, mode := range []string{"serial", "parallel"} {
			b.Run(fmt.Sprintf("%dKiB/%s", size>>10, mode), func(b *testing.B) {
				base := os.Getenv("INCUS_BACKUP_BENCH_FSDIR")
				if base == "" {
					base = b.TempDir()
				}
				root, err := os.MkdirTemp(base, "fsput-*")
				if err != nil {
					b.Fatal(err)
				}
				defer os.RemoveAll(root)
				if _, err := Init(filepath.Join(root, "ds")); err != nil {
					b.Fatal(err)
				}
				s, err := Open(filepath.Join(root, "ds"))
				if err != nil {
					b.Fatal(err)
				}
				// distinct ids: the blob is only checked by its trailer, so one blob can stand for all
				ids := make([]format.ID, b.N)
				for i := range ids {
					ids[i] = format.ID{1, 2, byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24)} // one existing leaf directory: the steady state of a repository
				}
				b.SetBytes(int64(len(blob)))
				b.ResetTimer()
				if mode == "serial" {
					for i := 0; i < b.N; i++ {
						if _, err := s.PutChunk(ids[i], blob); err != nil {
							b.Fatal(err)
						}
					}
					return
				}
				var next int64 = -1
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						i := int(atomicAdd(&next))
						if _, err := s.PutChunk(ids[i], blob); err != nil {
							b.Fatal(err)
						}
					}
				})
			})
		}
	}
}

func atomicAdd(p *int64) int64 { return atomic.AddInt64(p, 1) }
