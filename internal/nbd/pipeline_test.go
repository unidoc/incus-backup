package nbd

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func dialSrv(t *testing.T, srv *FakeServer, conn net.Conn, depth int, ctxs ...string) *Conn {
	t.Helper()
	c, err := Dial(context.Background(), conn, "root", ctxs)
	if err != nil {
		t.Fatal(err)
	}
	c.Depth = depth
	t.Cleanup(func() { c.Close() })
	return c
}

// modes runs fn against a sequential and an interleaving server, over a pipe and TCP, and with a
// 1 MiB request limit (reads split into several pipelined requests) and a 64 MiB one (every read
// in these tests is a single request: the non-pipelined path).
func modes(t *testing.T, e *Export, fn func(t *testing.T, srv *FakeServer, conn func() net.Conn)) {
	for _, maxReq := range []uint32{1 << 20, 64 << 20} {
		for _, inter := range []bool{false, true} {
			for _, tr := range []string{"pipe", "tcp"} {
				mode(t, e, maxReq, inter, tr, fn)
			}
		}
	}
}

// singleReq tells the fault predicates whether reads arrive as one request (offset 0) or split.
var singleReq atomic.Bool

func mode(t *testing.T, e *Export, maxReq uint32, inter bool, tr string, fn func(t *testing.T, srv *FakeServer, conn func() net.Conn)) {
	{
		{
			name := fmt.Sprintf("%s-max%dMiB", tr, maxReq>>20)
			if inter {
				name += "-interleaved"
			}
			t.Run(name, func(t *testing.T) {
				singleReq.Store(maxReq > 1<<20)
				srv := &FakeServer{Exports: map[string]*Export{"root": e}, Interleave: inter, MaxRequest: maxReq}
				conn := func() net.Conn {
					if tr == "pipe" {
						return srv.Pipe(context.Background())
					}
					c, err := net.Dial("tcp", tcpFake(t, srv))
					if err != nil {
						t.Fatal(err)
					}
					return c
				}
				fn(t, srv, conn)
			})
		}
	}
}

// Pipelined reads return exactly the export's bytes (data and holes) whatever the depth, the
// transport and the order in which a concurrent server answers.
func TestPipelinedReadsAreExact(t *testing.T) {
	disk := testDisk(24 << 20)
	e := &Export{Data: disk}
	modes(t, e, func(t *testing.T, srv *FakeServer, conn func() net.Conn) {
		for _, depth := range []int{1, 3, 16} {
			c := dialSrv(t, srv, conn(), depth)
			buf := make([]byte, 5<<20+12345)
			if n, err := c.ReadAt(buf, 1<<20+77); err != nil || n != len(buf) {
				t.Fatal(n, err)
			}
			if !bytes.Equal(buf, disk[1<<20+77:1<<20+77+len(buf)]) {
				t.Fatalf("depth %d: ReadAt data mismatch", depth)
			}
			// a batch of scattered 4 MiB chunks, some all-hole, reused buffers full of junk
			var segs []Segment
			for _, off := range []int64{0, 8 << 20, 4 << 20, 20 << 20, 12 << 20} {
				b := bytes.Repeat([]byte{0xee}, 4<<20)
				segs = append(segs, Segment{Off: off, Buf: b})
			}
			if err := c.ReadBatch(segs); err != nil {
				t.Fatal(err)
			}
			for _, sg := range segs {
				if !bytes.Equal(sg.Buf, disk[sg.Off:sg.Off+int64(len(sg.Buf))]) {
					t.Fatalf("depth %d: batch segment at %d differs", depth, sg.Off)
				}
			}
		}
	})
	if e.MaxInFlight() < 2 {
		t.Fatalf("the interleaving server never held two requests at once (%d): nothing was pipelined", e.MaxInFlight())
	}
}

// The window is a hard bound: the server never holds more requests than Depth.
func TestPipelineDepthIsBounded(t *testing.T) {
	for _, depth := range []int{1, 4} {
		e := &Export{Data: testDisk(32 << 20)}
		srv := &FakeServer{Exports: map[string]*Export{"root": e}, Interleave: true}
		c := dialSrv(t, srv, srv.Pipe(context.Background()), depth)
		var segs []Segment
		for off := int64(0); off < 32<<20; off += 4 << 20 {
			segs = append(segs, Segment{Off: off, Buf: make([]byte, 4<<20)})
		}
		for i := 0; i < 3; i++ {
			if err := c.ReadBatch(segs); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.MaxInFlight(); got > int64(depth) || (depth > 1 && got < 2) {
			t.Fatalf("depth %d: server saw %d requests in flight", depth, got)
		}
	}
}

// A server error in the middle of a read fails the call, drains what is in flight, and leaves the
// connection usable; ReadAt reports only the complete prefix.
func TestServerErrorMidStreamIsAnErrorAndTheConnectionSurvives(t *testing.T) {
	disk := testDisk(16 << 20)
	bad := uint64(3 << 20)
	e := &Export{Data: disk, FailRead: func(off uint64) bool { return off == bad || off == 0 && singleReq.Load() }}
	modes(t, e, func(t *testing.T, srv *FakeServer, conn func() net.Conn) {
		c := dialSrv(t, srv, conn(), 8)
		buf := make([]byte, 8<<20)
		n, err := c.ReadAt(buf, 0)
		if err == nil || !strings.Contains(err.Error(), "server error") {
			t.Fatalf("mid-stream error not reported: %v", err)
		}
		if n > int(bad) {
			t.Fatalf("ReadAt claims %d bytes complete, but the read at %d failed", n, bad)
		}
		if !bytes.Equal(buf[:n], disk[:n]) {
			t.Fatal("the reported prefix is wrong")
		}
		// still in sync: a later read is exact
		ok := make([]byte, 2<<20)
		if _, err := c.ReadAt(ok, 8<<20); err != nil || !bytes.Equal(ok, disk[8<<20:10<<20]) {
			t.Fatalf("connection unusable after a server error: %v", err)
		}
	})
}

func TestShortReplyIsAnError(t *testing.T) {
	disk := testDisk(8 << 20)
	e := &Export{Data: disk, ShortRead: func(off uint64) bool { return off == 1<<20 || off == 0 && singleReq.Load() }}
	modes(t, e, func(t *testing.T, srv *FakeServer, conn func() net.Conn) {
		c := dialSrv(t, srv, conn(), 8)
		if err := c.ReadBatch([]Segment{{Off: 0, Buf: make([]byte, 4<<20)}}); err == nil || !strings.Contains(err.Error(), "covered") {
			t.Fatalf("a reply that does not cover the range must fail: %v", err)
		}
	})
}

// Protocol violations (overlapping chunks that add up to the right length, a reply for a handle
// nobody asked for) fail and make the connection refuse further use.
func TestProtocolViolationsBreakTheConnection(t *testing.T) {
	disk := testDisk(8 << 20)
	cases := map[string]*Export{
		"overlap":    {Data: disk, Overlap: func(off uint64) bool { return off == 2<<20 || off == 0 && singleReq.Load() }},
		"bad-handle": {Data: disk, BadHandle: true},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			modes(t, e, func(t *testing.T, srv *FakeServer, conn func() net.Conn) {
				c := dialSrv(t, srv, conn(), 8)
				if _, err := c.ReadAt(make([]byte, 4<<20), 0); err == nil {
					t.Fatal("protocol violation accepted")
				}
				if _, err := c.ReadAt(make([]byte, 10), 0); err == nil {
					t.Fatal("a broken connection was used again")
				}
			})
		})
	}
}

func TestConnectionDroppedWhileRequestsAreInFlight(t *testing.T) {
	e := &Export{Data: testDisk(32 << 20), BreakAfter: 5}
	modes(t, e, func(t *testing.T, srv *FakeServer, conn func() net.Conn) {
		c := dialSrv(t, srv, conn(), 16)
		done := make(chan error, 1)
		var segs []Segment
		for off := int64(0); off < 16<<20; off += 2 << 20 {
			segs = append(segs, Segment{Off: off, Buf: make([]byte, 2<<20)})
		}
		go func() { done <- c.ReadBatch(segs) }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("truncated session returned success")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("read hung after the server went away")
		}
	})
}

// Close (what context.AfterFunc does on max_freeze) unblocks a read stuck on a hung server.
func TestCloseUnblocksAStalledRead(t *testing.T) {
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	e := &Export{Data: testDisk(8 << 20), Stall: stall}
	modes(t, e, func(t *testing.T, srv *FakeServer, conn func() net.Conn) {
		c := dialSrv(t, srv, conn(), 4)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		stop := context.AfterFunc(ctx, func() { _ = c.Close() })
		defer stop()
		done := make(chan error, 1)
		go func() { done <- c.ReadBatch([]Segment{{Off: 0, Buf: make([]byte, 4<<20)}}) }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a read on a hung, closed connection succeeded")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Close did not unblock the read")
		}
		if _, err := c.ReadAt(make([]byte, 1), 0); err == nil {
			t.Fatal("read after Close succeeded")
		}
	})
}
