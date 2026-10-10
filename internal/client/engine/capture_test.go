package engine_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/client/engine"
)

// A disk that spans several pipelined read batches, with all-zero chunks inside batches and a
// short tail chunk, is captured exactly, fully and then incrementally.
func TestCaptureAcrossReadBatches(t *testing.T) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	size := 77*MiB + 12345 // 20 chunks, the last one short
	w.inc.AddVM("default", "bosch", map[string]int{"root": size}, true)
	w.inc.SetConfig("default", "bosch", engine.KeyPolicy, "prod")
	disk := rnd(size, 11)
	for _, z := range []int{3, 8, 9, 15} { // zero chunks inside the first and second batch
		copy(disk[z*4*MiB:(z+1)*4*MiB], make([]byte, 4*MiB))
	}
	w.inc.GuestWrite("default", "bosch", "root", 0, disk)
	root := find(w.objects(), "instance/bosch/root")
	s1, rs := w.backupVM("bosch")
	mustOK(t, rs)
	if !rs[0].Full || rs[0].Dirty != int64(size) {
		t.Fatalf("first run must read everything: %+v", rs[0])
	}
	if !bytes.Equal(w.fetch("cikarang", root, s1), disk) {
		t.Fatal("full capture differs from the disk")
	}
	w.now = w.now.Add(time.Hour)
	for _, at := range []int{1*4*MiB + 7, 9 * 4 * MiB, 17*4*MiB + 100, size - 10} {
		p := rnd(9, int64(at))
		w.inc.GuestWrite("default", "bosch", "root", at, p)
		copy(disk[at:], p)
	}
	s2, rs := w.backupVM("bosch")
	mustOK(t, rs)
	if rs[0].Full || rs[0].Dirty != 3*4*MiB+int64(size-19*4*MiB) {
		t.Fatalf("incremental run must read exactly the dirty chunks: %+v", rs[0])
	}
	if !bytes.Equal(w.fetch("cikarang", root, s2), disk) {
		t.Fatal("incremental capture differs from the disk")
	}
}
