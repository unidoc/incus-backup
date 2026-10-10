package nbd

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"testing"
	"time"
)

// Benchmarks of the read path against the in-memory FakeServer. The fake server answers one
// request at a time, caps requests at 1 MiB (as advertised in its block-size info) and copies
// every reply, so these numbers bound the client from above only up to the fake server's own cost;
// they are not QEMU numbers.

const benchDisk = 256 << 20

func benchExport() *Export {
	d := make([]byte, benchDisk)
	rand.New(rand.NewSource(1)).Read(d)
	return &Export{Data: d}
}

type transport struct {
	name string
	dial func(tb testing.TB, srv *FakeServer) net.Conn
}

func transports() []transport {
	tcp := func(tb testing.TB, srv *FakeServer) net.Conn {
		c, err := net.Dial("tcp", tcpFake(tb, srv))
		if err != nil {
			tb.Fatal(err)
		}
		return c
	}
	delayed := func(rtt time.Duration) func(testing.TB, *FakeServer) net.Conn {
		return func(tb testing.TB, srv *FakeServer) net.Conn {
			dl := newDelayLine(tb, tcpFake(tb, srv), rtt)
			c, err := net.Dial("tcp", dl.addr())
			if err != nil {
				tb.Fatal(err)
			}
			return c
		}
	}
	return []transport{
		{"pipe", func(tb testing.TB, srv *FakeServer) net.Conn { return srv.Pipe(context.Background()) }},
		{"tcp", tcp},
		{"rtt1ms", delayed(time.Millisecond)},
		{"rtt5ms", delayed(5 * time.Millisecond)},
	}
}

// BenchmarkReadAt reads the whole export sequentially in 4 MiB ReadAt calls (what captureDisk did).
func BenchmarkReadAt(b *testing.B) {
	e := benchExport()
	for _, tr := range transports() {
		b.Run(tr.name, func(b *testing.B) {
			srv := &FakeServer{Exports: map[string]*Export{"root": e}}
			c, err := Dial(context.Background(), tr.dial(b, srv), "root", nil)
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close()
			buf := make([]byte, 4<<20)
			b.SetBytes(benchDisk)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for off := int64(0); off < benchDisk; off += int64(len(buf)) {
					if _, err := c.ReadAt(buf, off); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

// BenchmarkReadBatch reads the export as captureDisk does now: batches of 8 x 4 MiB segments with
// up to depth requests in flight, against servers advertising 1 MiB (the fake's default) and 4 MiB
// maximum requests.
func BenchmarkReadBatch(b *testing.B) {
	e := benchExport()
	for _, tr := range transports() {
		for _, maxReq := range []uint32{1 << 20, 4 << 20} {
			for _, depth := range []int{1, 4, 16} {
				b.Run(fmt.Sprintf("%s/maxreq%dMiB/depth%d", tr.name, maxReq>>20, depth), func(b *testing.B) {
					srv := &FakeServer{Exports: map[string]*Export{"root": e}, MaxRequest: maxReq}
					c, err := Dial(context.Background(), tr.dial(b, srv), "root", nil)
					if err != nil {
						b.Fatal(err)
					}
					defer c.Close()
					c.Depth = depth
					segs := make([]Segment, 8)
					for i := range segs {
						segs[i].Buf = make([]byte, 4<<20)
					}
					b.SetBytes(benchDisk)
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						for off := int64(0); off < benchDisk; off += 32 << 20 {
							for k := range segs {
								segs[k].Off = off + int64(k)<<22
							}
							if err := c.ReadBatch(segs); err != nil {
								b.Fatal(err)
							}
						}
					}
				})
			}
		}
	}
}

// BenchmarkReadAtSmall: 4 KiB random reads over a pipe (the guest-file browsing pattern); guards
// against per-call overhead of the pipelined path.
func BenchmarkReadAtSmall(b *testing.B) {
	e := benchExport()
	srv := &FakeServer{Exports: map[string]*Export{"root": e}}
	c, err := Dial(context.Background(), srv.Pipe(context.Background()), "root", nil)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, 4096)
	rng := rand.New(rand.NewSource(3))
	b.SetBytes(4096)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.ReadAt(buf, rng.Int63n(benchDisk/4096)*4096); err != nil {
			b.Fatal(err)
		}
	}
}
