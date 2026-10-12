package format

// Benchmarks of the client's per-chunk work: keyed id (HMAC-SHA256), zstd, XChaCha20-Poly1305 and
// the SHA-256 trailer, for one 4 MiB block chunk. See docs/performance.md.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	mrand "math/rand/v2"
	"testing"
)

const benchChunk = 4 << 20

// benchData returns 4 MiB of the given kind: "random" (incompressible), "text" (compresses
// about 3:1, like logs or source), "sparse" (mostly zero with scattered data, like a VM disk).
func benchData(kind string) []byte {
	b := make([]byte, benchChunk)
	r := mrand.New(mrand.NewPCG(1, 2))
	switch kind {
	case "random":
		fill(r, b)
	case "text":
		words := []string{"incus", "backup", "chunk", "the", "of", "server", "zstd", "lorem", "ipsum", "error", "warning", "2026-10-08"}
		p := 0
		for p < len(b) {
			w := words[r.IntN(len(words))]
			if r.IntN(8) == 0 {
				w = fmt.Sprintf("%x", r.Uint64())
			}
			p += copy(b[p:], w+" ")
		}
	case "sparse":
		for i := 0; i < len(b); i += 4096 {
			if r.IntN(10) == 0 {
				fill(r, b[i:i+4096])
			}
		}
	}
	return b
}

// fill writes deterministic pseudo-random (incompressible) bytes.
func fill(r *mrand.Rand, b []byte) {
	for i := 0; i+8 <= len(b); i += 8 {
		v := r.Uint64()
		for j := 0; j < 8; j++ {
			b[i+j] = byte(v >> (8 * j))
		}
	}
}

func benchKeys(b *testing.B) *Keys {
	k, err := DeriveKeys(fixedMaster())
	if err != nil {
		b.Fatal(err)
	}
	return k
}

func BenchmarkEncodeChunk(b *testing.B) {
	k := benchKeys(b)
	for _, kind := range []string{"random", "text", "sparse"} {
		data := benchData(kind)
		b.Run(kind+"/serial", func(b *testing.B) {
			b.SetBytes(benchChunk)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, err := k.EncodeChunk(data, EncodeOpts{}); err != nil {
					b.Fatal(err)
				}
			}
		})
		// one goroutine per core (GOMAXPROCS): the client's worker pool
		b.Run(kind+"/parallel", func(b *testing.B) {
			b.SetBytes(benchChunk)
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, _, err := k.EncodeChunk(data, EncodeOpts{}); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

func BenchmarkDecodeChunk(b *testing.B) {
	k := benchKeys(b)
	for _, kind := range []string{"random", "text"} {
		id, blob, err := k.EncodeChunk(benchData(kind), EncodeOpts{})
		if err != nil {
			b.Fatal(err)
		}
		b.Run(kind+"/serial", func(b *testing.B) {
			b.SetBytes(benchChunk)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := k.DecodeChunk(blob, id); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(kind+"/parallel", func(b *testing.B) {
			b.SetBytes(benchChunk)
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := k.DecodeChunk(blob, id); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// The parts of the per-chunk cost, one at a time.
func BenchmarkChunkParts(b *testing.B) {
	k := benchKeys(b)
	data := benchData("random")
	b.Run("hmac-id", func(b *testing.B) {
		b.SetBytes(benchChunk)
		for i := 0; i < b.N; i++ {
			k.ChunkID(data)
		}
	})
	b.Run("sha256", func(b *testing.B) {
		b.SetBytes(benchChunk)
		for i := 0; i < b.N; i++ {
			sha256.Sum256(data)
		}
	})
	// the capture's all-zero test (block.go, fs2.go): a zero chunk is never hashed or encoded
	zero := make([]byte, benchChunk)
	b.Run("zero-check", func(b *testing.B) {
		b.SetBytes(benchChunk)
		for i := 0; i < b.N; i++ {
			if bytes.Count(zero, []byte{0}) != len(zero) {
				b.Fatal("not zero")
			}
		}
	})
	text := benchData("text")
	b.Run("zstd-text", func(b *testing.B) {
		b.SetBytes(benchChunk)
		for i := 0; i < b.N; i++ {
			compress(text)
		}
	})
	b.Run("zstd-random", func(b *testing.B) {
		b.SetBytes(benchChunk)
		for i := 0; i < b.N; i++ {
			compress(data)
		}
	})
}

// goldenBlobs encodes fixed inputs with a fixed nonce and returns the SHA-256 of every blob, for
// pinning the compressed encoding (the format vectors only pin an uncompressed chunk).
func goldenBlobs(tb testing.TB, k *Keys) []string {
	var out []string
	nonce := fixedNonce(0x33)
	inputs := [][]byte{nil, []byte("x"), make([]byte, 100000)}
	for _, kind := range []string{"text", "sparse"} {
		d := benchData(kind)
		inputs = append(inputs, d, d[:12345], d[1<<20:3<<20+77])
	}
	r := mrand.New(mrand.NewPCG(7, 8))
	rnd := make([]byte, 300000)
	for i := range rnd {
		rnd[i] = byte(r.Uint32())
	}
	inputs = append(inputs, rnd)
	for _, in := range inputs {
		_, blob, err := k.EncodeChunk(in, EncodeOpts{Nonce: nonce})
		if err != nil {
			tb.Fatal(err)
		}
		s := sha256.Sum256(blob)
		out = append(out, hex.EncodeToString(s[:]))
	}
	return out
}
