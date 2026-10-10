package format

import (
	"bytes"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// goldenCompressed are the SHA-256 digests of the blobs goldenBlobs produces, computed with the
// encoder as it was before the encoder pool was introduced (one shared zstd encoder). The format
// vectors pin an uncompressed chunk only; this pins the compressed path byte for byte, so a change
// of the compression (level, options, library behaviour) is noticed.
var goldenCompressed = []string{
	"86e7350e2c1b3cf622487bd765b4d165c54083f54fcd91025c928d652804bc23",
	"7ed96f43391f221c0ea031ea8aa00691c7f0c28eaf302ee97b13e60b7497d54b",
	"b7154e56d5b219a6bb2506a478a5c0b38e1b51621026f899488f6caeeec296fb",
	"94fe6211ec1f5a8f8baec52dbf588599b04fee45f93ba60c936d15251e883c06",
	"2028d47cfc57a4cc291b2c236416e80ab39f547fb8247e71a87523518fe51a99",
	"63a9d9211ff65a41a0db56e2e39ade16d7d575f751a31168b23ca7c871ad0c98",
	"f3374e40f19e54d2f40d1247d5f49244761c76c3eb82b4d59146727190d98391",
	"c91a1bb41b8dfa6db9c3d983e55894cc68dd55c5c91b656d26e698be38871fbd",
	"3d92a6de521648debd95c850b1a270fe05d125db26ef601df01492888bf79e9e",
	"71acabba876ae3b4c5dafffe7e70138f14f5088b131bec8040e800fe32244c7f",
}

func TestCompressedEncodingIsPinned(t *testing.T) {
	k, err := DeriveKeys(fixedMaster())
	if err != nil {
		t.Fatal(err)
	}
	// serially, and from many goroutines at once (each pooled encoder must give the same bytes)
	got := goldenBlobs(t, k)
	for i := range goldenCompressed {
		if got[i] != goldenCompressed[i] {
			t.Fatalf("compressed blob %d changed: %s", i, got[i])
		}
	}
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < 1; r++ {
				for i, h := range goldenBlobs(t, k) {
					if h != goldenCompressed[i] {
						errs <- h
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for h := range errs {
		t.Fatalf("a concurrent encode produced different bytes: %s", h)
	}
}

// The pooled encoders compress exactly like one encoder built with the original options, for
// every input size around the block and window boundaries.
func TestPooledZstdMatchesReferenceEncoder(t *testing.T) {
	ref, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	data := benchData("text")
	sizes := []int{0, 1, 100, 4095, 4096, 65535, 65536, 128<<10 - 1, 128 << 10, 128<<10 + 1, 1 << 20, 4<<20 - 3, 4 << 20}
	for _, n := range sizes {
		want := ref.EncodeAll(data[:n], nil)
		got := compress(data[:n])
		if !bytes.Equal(got, want) {
			t.Fatalf("size %d: pooled encoder output differs", n)
		}
	}
}
