package nbd

import (
	"bytes"
	"context"
	"math/rand"
	"testing"
	"time"
)

func testDisk(size int) []byte {
	d := make([]byte, size)
	rng := rand.New(rand.NewSource(7))
	// data in some 64 KiB blocks, holes elsewhere
	for blk := 0; blk < size/65536; blk++ {
		if blk%3 == 0 {
			rng.Read(d[blk*65536 : (blk+1)*65536])
		}
	}
	return d
}

func dial(t *testing.T, e *Export, ctxs ...string) *Conn {
	t.Helper()
	srv := &FakeServer{Exports: map[string]*Export{"root": e}}
	c, err := Dial(context.Background(), srv.Pipe(context.Background()), "root", ctxs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestReadAcrossRequestLimitWithHoles(t *testing.T) {
	disk := testDisk(8 << 20)
	c := dial(t, &Export{Data: disk})
	if c.Size() != uint64(len(disk)) {
		t.Fatal("size")
	}
	buf := make([]byte, 5<<20) // larger than the server's 1 MiB max request: must be split
	if _, err := c.ReadAt(buf, 1<<20); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, disk[1<<20:6<<20]) {
		t.Fatal("data mismatch")
	}
	// reading past the end is refused locally
	if _, err := c.ReadAt(make([]byte, 10), int64(len(disk))-5); err == nil {
		t.Fatal("read past the end succeeded")
	}
}

func TestStatusFollowsServerUntilCovered(t *testing.T) {
	disk := testDisk(64 << 20) // 1024 granules: forces several "256 extent" replies
	e := &Export{Data: disk, Dirty: map[string]func(uint64) bool{"b1": func(o uint64) bool { return (o/65536)%2 == 0 }}}
	c := dial(t, e, "base:allocation", "qemu:dirty-bitmap:b1")
	ext, err := c.Status("qemu:dirty-bitmap:b1", 0, uint64(len(disk)))
	if err != nil {
		t.Fatal(err)
	}
	var cover, dirty uint64
	prevEnd := uint64(0)
	for _, x := range ext {
		if x.Offset != prevEnd {
			t.Fatalf("gap or overlap at %d", x.Offset)
		}
		prevEnd = x.Offset + x.Length
		cover += x.Length
		if x.Flags&1 != 0 {
			dirty += x.Length
		}
	}
	if cover != uint64(len(disk)) || dirty != uint64(len(disk))/2 {
		t.Fatalf("cover=%d dirty=%d", cover, dirty)
	}
	alloc, err := c.Status("base:allocation", 0, uint64(len(disk)))
	if err != nil || len(alloc) == 0 {
		t.Fatal(err)
	}
	if _, err := c.Status("qemu:dirty-bitmap:missing", 0, 4096); err == nil {
		t.Fatal("ungranted context must be an error, never 'all clean'")
	}
	// a bitmap the server does not have is simply not granted
	c2 := dial(t, e, "qemu:dirty-bitmap:nope")
	if _, ok := c2.Context["qemu:dirty-bitmap:nope"]; ok {
		t.Fatal("unknown bitmap granted")
	}
}

func TestWriteAndFlush(t *testing.T) {
	e := &Export{Data: make([]byte, 4<<20), Writes: true}
	c := dial(t, e)
	payload := bytes.Repeat([]byte{0xab}, 3<<20)
	if _, err := c.WriteAt(payload, 4096); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(e.Data[4096:4096+len(payload)], payload) {
		t.Fatal("not written")
	}
	ro := dial(t, &Export{Data: make([]byte, 4096)})
	if _, err := ro.WriteAt([]byte("x"), 0); err == nil {
		t.Fatal("write to a read-only export succeeded")
	}
}

func TestServerDyingMidReadIsAnError(t *testing.T) {
	c := dial(t, &Export{Data: testDisk(16 << 20), BreakAfter: 2})
	buf := make([]byte, 8<<20)
	if _, err := c.ReadAt(buf, 0); err == nil {
		t.Fatal("truncated session returned success")
	}
}

func TestHandshakeFailures(t *testing.T) {
	srv := &FakeServer{Exports: map[string]*Export{}}
	if _, err := Dial(context.Background(), srv.Pipe(context.Background()), "nope", nil); err == nil {
		t.Fatal("unknown export accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	a, _ := netPipeSilent()
	if _, err := Dial(ctx, a, "root", nil); err == nil {
		t.Fatal("silent server must time out")
	}
}
