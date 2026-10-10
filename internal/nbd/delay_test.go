package nbd

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// delayLine is a TCP proxy that adds a fixed one-way delay (rtt/2) to every byte in both
// directions without limiting bandwidth: each read is stamped and released at stamp+rtt/2 by a
// separate writer, so many requests can be in flight at once (sleeping per read would serialise
// them and model a much slower link). It models the latency of the real Incus NBD proxy path.
type delayLine struct {
	ln     net.Listener
	target string
	oneWay time.Duration
	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  []net.Conn
}

func newDelayLine(tb testing.TB, target string, rtt time.Duration) *delayLine {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	d := &delayLine{ln: ln, target: target, oneWay: rtt / 2}
	tb.Cleanup(func() {
		ln.Close()
		d.mu.Lock()
		for _, c := range d.conns {
			c.Close()
		}
		d.mu.Unlock()
		d.wg.Wait()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			d.mu.Lock()
			d.conns = append(d.conns, c, s)
			d.mu.Unlock()
			d.wg.Add(2)
			go d.pump(c, s)
			go d.pump(s, c)
		}
	}()
	return d
}

func (d *delayLine) addr() string { return d.ln.Addr().String() }

type stamped struct {
	at time.Time
	b  []byte
}

// pump copies src to dst, delaying every piece by oneWay.
func (d *delayLine) pump(src, dst net.Conn) {
	defer d.wg.Done()
	q := make(chan stamped, 1<<14)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for p := range q {
			if w := time.Until(p.at); w > 0 {
				time.Sleep(w)
			}
			if _, err := dst.Write(p.b); err != nil {
				for range q {
				}
				return
			}
		}
	}()
	for {
		b := make([]byte, 64<<10)
		n, err := src.Read(b)
		if n > 0 {
			q <- stamped{time.Now().Add(d.oneWay), b[:n]}
		}
		if err != nil {
			break
		}
	}
	close(q)
	<-done
	if tc, ok := dst.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	} else {
		dst.Close()
	}
	src.Close()
}

// tcpFake serves srv on a loopback TCP listener.
func tcpFake(tb testing.TB, srv *FakeServer) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.Serve(context.Background(), c)
		}
	}()
	return ln.Addr().String()
}

var _ = io.EOF

// BenchmarkDelayLineRTT calibrates the delay line: one-byte ping-pong round trips.
func BenchmarkDelayLineRTT(b *testing.B) {
	for _, rtt := range []time.Duration{0, time.Millisecond, 5 * time.Millisecond} {
		b.Run(rtt.String(), func(b *testing.B) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			defer ln.Close()
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				io.Copy(c, c)
			}()
			addr := ln.Addr().String()
			if rtt > 0 {
				addr = newDelayLine(b, addr, rtt).addr()
			}
			c, err := net.Dial("tcp", addr)
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close()
			p := []byte{1}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c.Write(p)
				io.ReadFull(c, p)
			}
		})
	}
}
