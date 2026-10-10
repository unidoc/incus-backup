package bench

import (
	"io"
	"net"
	"testing"
	"time"
)

// The delay line really adds the round-trip time to a request/response exchange, and it does
// not throttle a bulk transfer to one buffer per RTT (negative control for a sleep-per-read).
func TestDelayProxyAddsLatencyNotThrottling(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	const rtt = 20 * time.Millisecond
	p, err := NewDelayProxy(ln.Addr().String(), rtt)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, 1)
	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := c.Write([]byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c, buf); err != nil || buf[0] != byte(i) {
			t.Fatalf("echo %d: %v %v", i, err, buf)
		}
	}
	per := time.Since(start) / 5
	if per < rtt || per > rtt+15*time.Millisecond {
		t.Fatalf("round trip through the line took %s, want about %s", per, rtt)
	}
	// 32 MiB in flight at once: with a sleep per 64 KiB read this would take 512*RTT (10 s).
	big := make([]byte, 32<<20)
	start = time.Now()
	go func() { _, _ = c.Write(big) }()
	if _, err := io.ReadFull(c, make([]byte, len(big))); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("a 32 MiB transfer took %s: the line throttles", el)
	}
	if p.Conns.Load() != 1 {
		t.Fatalf("connections: %d", p.Conns.Load())
	}
}
