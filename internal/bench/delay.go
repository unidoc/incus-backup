// Package bench holds measurement helpers shared by the benchmarks of incus-backup (see
// docs/performance.md). Nothing in it is used by the production binaries.
package bench

import (
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DelayProxy is a TCP delay line: every byte that crosses it in either direction is released
// RTT/2 after it arrived. Bytes are stamped on arrival and released at stamp+RTT/2, so the
// line adds latency without limiting throughput (a sleep per read would also throttle a
// bulk transfer). It models a WAN link between a server and its S3 service, or between a
// client and its server, on one machine.
type DelayProxy struct {
	ln     net.Listener
	target string
	oneWay time.Duration
	// Conns counts accepted connections (connection reuse shows as a small number).
	Conns atomic.Int64
	// Bytes counts bytes forwarded in both directions.
	Bytes atomic.Int64
	wg    sync.WaitGroup
	mu    sync.Mutex
	open  map[net.Conn]struct{}
	done  chan struct{}
}

// NewDelayProxy listens on 127.0.0.1 and forwards to target (host:port) with the given round-trip time.
func NewDelayProxy(target string, rtt time.Duration) (*DelayProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &DelayProxy{ln: ln, target: target, oneWay: rtt / 2, open: map[net.Conn]struct{}{}, done: make(chan struct{})}
	go p.accept()
	return p, nil
}

// Addr is the host:port to connect to.
func (p *DelayProxy) Addr() string { return p.ln.Addr().String() }

// Close stops the proxy and breaks every connection.
func (p *DelayProxy) Close() {
	select {
	case <-p.done:
		return
	default:
	}
	close(p.done)
	_ = p.ln.Close()
	p.mu.Lock()
	for c := range p.open {
		_ = c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

func (p *DelayProxy) track(c net.Conn, add bool) {
	p.mu.Lock()
	if add {
		p.open[c] = struct{}{}
	} else {
		delete(p.open, c)
	}
	p.mu.Unlock()
}

func (p *DelayProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.Conns.Add(1)
		s, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.track(c, true)
		p.track(s, true)
		p.wg.Add(2)
		var once sync.Once
		closeBoth := func() {
			once.Do(func() {
				_ = c.Close()
				_ = s.Close()
				p.track(c, false)
				p.track(s, false)
			})
		}
		var half sync.WaitGroup
		half.Add(2)
		go func() { defer p.wg.Done(); defer half.Done(); p.line(s, c, closeBoth) }()
		go func() { defer p.wg.Done(); defer half.Done(); p.line(c, s, closeBoth) }()
		go func() { half.Wait(); closeBoth() }()
	}
}

type packet struct {
	b  []byte
	at time.Time
}

// line forwards src to dst, each read released oneWay after it arrived.
func (p *DelayProxy) line(dst, src net.Conn, abort func()) {
	ch := make(chan packet, 1<<14)
	go func() {
		defer close(ch)
		for {
			buf := make([]byte, 64<<10)
			n, err := src.Read(buf)
			if n > 0 {
				ch <- packet{buf[:n], time.Now().Add(p.oneWay)}
			}
			if err != nil {
				return
			}
		}
	}()
	for k := range ch {
		if d := time.Until(k.at); d > 0 {
			time.Sleep(d)
		}
		if _, err := dst.Write(k.b); err != nil {
			abort()
			for range ch {
			}
			return
		}
		p.Bytes.Add(int64(len(k.b)))
	}
	if tc, ok := dst.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	} else {
		abort()
	}
}

// Ints parses a comma separated list from the environment variable name, or returns def.
func Ints(name string, def ...int) []int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	var out []int
	for _, f := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err == nil {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// Durations parses a comma separated list of milliseconds from name, or returns def.
func Durations(name string, def ...time.Duration) []time.Duration {
	ms := Ints(name)
	if len(ms) == 0 {
		return def
	}
	out := make([]time.Duration, len(ms))
	for i, m := range ms {
		out[i] = time.Duration(m) * time.Millisecond
	}
	return out
}
