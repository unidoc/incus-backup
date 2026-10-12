package format

import (
	"crypto/rand"
	"fmt"
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/memprobe"
)

// TestIndexMemoryPerEntry measures what an index of a 4 TiB disk (a million 4 MiB entries, all
// distinct) costs in memory: the file, the parsed entries and the distinct-id list every upload,
// sync and verification builds. These are the unavoidable linear costs of block objects; the
// numbers go into docs/memory.md.
func TestIndexMemoryPerEntry(t *testing.T) {
	sizes := memprobe.Sizes("IB_MEMBENCH_INDEX_ENTRIES", []float64{1 << 18})
	var rows []string
	for _, sz := range sizes {
		n := int(sz)
		x := NewFixedIndex(uint64(n)*FixedChunkSize, FixedChunkSize)
		for i := range x.Entries {
			_, _ = rand.Read(x.Entries[i].ID[:])
		}
		b, err := x.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		x = nil
		label := fmt.Sprintf("%d entries (%d GiB disk)", n, n*4/1024)

		p := memprobe.StartTight("ParseIndex")
		idx, err := ParseIndex(b)
		r1 := p.Stop()
		if err != nil {
			t.Fatal(err)
		}
		p = memprobe.StartTight("ChunkIDs")
		ids := idx.ChunkIDs()
		r2 := p.Stop()
		if len(ids) != n {
			t.Fatalf("%d ids", len(ids))
		}
		for _, r := range []memprobe.Result{r1, r2} {
			per := float64(r.HeapDelta) / float64(n)
			rows = append(rows, r.Row("index", label)+fmt.Sprintf(" %.0f B/entry |", per))
			if per > 200 { // measured 72-112 at suite size, 110 at 1M entries: a gross regression (a second set or list) fails
				t.Errorf("%s: %.0f bytes per entry", r.Phase, per)
			}
		}
		rows = append(rows, fmt.Sprintf("| index | %s | index file | | %.1f MiB | | | | | %d B/entry |", label, float64(len(b))/(1<<20), len(b)/n))
	}
	t.Log("\n" + memprobe.Header + "\n" + strings.Join(rows, "\n"))
	memprobe.Report(rows...)
}

// ChunkIDs is exact although its seen-set keys on an 8-byte prefix: ids that share the prefix
// are all returned, once each, in index order.
func TestChunkIDsIsExactForSharedPrefixes(t *testing.T) {
	mk := func(prefix, tail byte) ID {
		var id ID
		for i := 0; i < 8; i++ {
			id[i] = prefix
		}
		id[31] = tail
		return id
	}
	a, b, c, d := mk(1, 1), mk(1, 2), mk(1, 3), mk(2, 1)
	x := &Index{Kind: IndexDynamic}
	for i, id := range []ID{a, b, a, {}, c, b, d, c, a} {
		x.Entries = append(x.Entries, Entry{End: uint64(i + 1), ID: id})
	}
	got := x.ChunkIDs()
	want := []ID{a, b, c, d}
	if len(got) != len(want) {
		t.Fatalf("got %d ids, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("id %d differs", i)
		}
	}
}
