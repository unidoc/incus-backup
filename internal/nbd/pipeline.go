package nbd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// DefaultDepth is the default number of read requests kept in flight on one connection. QEMU's
// NBD server handles requests concurrently, so overlapping them hides the round trip of the
// Incus proxy path; 16 x 4 MiB keeps 64 MiB in flight, enough for ~30 GB/s*ms of bandwidth-delay.
const DefaultDepth = 16

// Segment is one range of a batched read: Buf is filled from offset Off.
type Segment struct {
	Off int64
	Buf []byte
}

// pending is one read request in flight.
type pending struct {
	off     uint64
	p       []byte
	seg     int // index of the segment it belongs to
	covered uint64
	done    bool
	failed  bool // an error chunk was reported; later chunks of this request are discarded
	spans   [][2]uint64
}

// claim records that [o, o+n) of the request was answered. Chunks of one reply must not overlap
// (NBD spec): an overlap could otherwise hide a range that was never sent and would read as zeros.
func (r *pending) claim(o, n uint64) error {
	for _, s := range r.spans {
		if o < s[1] && s[0] < o+n {
			return errors.New("nbd: overlapping chunks in a read reply")
		}
	}
	r.spans = append(r.spans, [2]uint64{o, o + n})
	r.covered += n
	return nil
}

// errHard marks failures after which the reply stream cannot be trusted.
type errHard struct{ error }

func (e errHard) Unwrap() error { return e.error }

func (c *Conn) depth() int {
	if c.Depth > 0 {
		return c.Depth
	}
	return DefaultDepth
}

// readSegments reads every segment, split into requests of at most maxReq bytes, with up to
// depth() requests in flight. Requests are written by a helper goroutine while this one reads and
// demultiplexes the replies by handle: structured reply chunks of different requests may arrive in
// any order and interleaved, as the NBD protocol allows. Writing and reading concurrently is also
// what keeps a synchronous transport (net.Pipe) from deadlocking when requests outrun replies.
//
// Errors the server reports for one request (an error chunk, an errno, a reply that does not cover
// the range) leave the stream in sync: the remaining in-flight replies are drained and the
// connection stays usable, as with one request at a time. A protocol violation or a transport
// error closes the connection and marks it broken. Callers hold c.mu. It returns how many bytes of
// segs[0] are complete from its start (ReadAt's count).
func (c *Conn) readSegments(segs []Segment) (int, error) {
	var reqs []*pending
	for si, sg := range segs {
		for done := 0; done < len(sg.Buf); {
			n := len(sg.Buf) - done
			if n > int(c.maxReq) {
				n = int(c.maxReq)
			}
			off := uint64(sg.Off) + uint64(done)
			if err := c.check(off, uint64(n)); err != nil {
				return 0, err
			}
			reqs = append(reqs, &pending{off: off, p: sg.Buf[done : done+n], seg: si})
			done += n
		}
	}
	if len(reqs) == 0 {
		return 0, nil
	}
	for _, r := range reqs {
		clear(r.p) // a hole leaves its range untouched: it must read as zeros
	}
	if len(reqs) == 1 {
		return c.readSingle(reqs[0])
	}
	byHandle := make(map[uint64]*pending, len(reqs))
	handles := make([]uint64, len(reqs))
	for i, r := range reqs {
		c.handle++
		handles[i] = c.handle
		byHandle[c.handle] = r
	}

	// writer: sends requests while a window slot is free
	window := make(chan struct{}, c.depth())
	stop := make(chan struct{})
	wdone := make(chan struct{})
	// startedCh carries "requests 1..n were (being) sent"; it never blocks the writer
	startedCh := make(chan int64, len(reqs)+1)
	var werr error
	writer := func() {
		defer close(wdone)
		var hdr [28]byte
		for i, r := range reqs {
			select {
			case window <- struct{}{}:
			case <-stop:
				return
			}
			select {
			case <-stop:
				return
			default:
			}
			binary.BigEndian.PutUint32(hdr[0:], reqMagic)
			binary.BigEndian.PutUint16(hdr[4:], 0)
			binary.BigEndian.PutUint16(hdr[6:], cmdRead)
			binary.BigEndian.PutUint64(hdr[8:], handles[i])
			binary.BigEndian.PutUint64(hdr[16:], r.off)
			binary.BigEndian.PutUint32(hdr[24:], uint32(len(r.p)))
			startedCh <- int64(i + 1)
			if _, err := c.c.Write(hdr[:]); err != nil {
				werr = err
				_ = c.c.Close() // the reader is (or will be) waiting for a reply that never comes
				return
			}
		}
	}
	go writer()

	var soft error
	stopped := false
	halt := func() {
		if !stopped {
			stopped = true
			close(stop)
		}
	}
	finished := 0
	hard := func(err error) (int, error) {
		halt()
		_ = c.c.Close()
		<-wdone
		if werr != nil {
			err = fmt.Errorf("nbd: sending a read request: %w (reply stream: %v)", werr, err)
		}
		c.broken = fmt.Errorf("nbd: connection unusable after an earlier error: %w", err)
		return prefix(reqs), err
	}
	var started int64
	writerDone := false
	for finished < len(reqs) {
		// only read a reply when a request is outstanding; otherwise wait until the writer sends
		// one, or gives up (after a soft error)
		for started <= int64(finished) && !writerDone {
			select {
			case n := <-startedCh:
				started = n
			case <-wdone:
				writerDone = true
			}
		}
		for drained := false; !drained; {
			select {
			case n := <-startedCh:
				started = n
			default:
				drained = true
			}
		}
		if started <= int64(finished) {
			break // the writer stopped and every request it sent is answered
		}
		r, err := c.replyChunk(byHandle)
		if err != nil {
			var h errHard
			if errors.As(err, &h) {
				return hard(h.error)
			}
			// a soft error that ends its request: keep draining, send nothing new
			if soft == nil {
				soft = err
			}
			halt()
		}
		if r != nil && r.done {
			finished++
			<-window
		}
	}
	halt()
	<-wdone
	if werr != nil {
		return hard(io.ErrUnexpectedEOF)
	}
	if soft != nil {
		return prefix(reqs), soft
	}
	return len(segs[0].Buf), nil
}

// readSingle is readSegments for one request: nothing to overlap, so no goroutine and no window
// (the small random reads of guest-file browsing).
func (c *Conn) readSingle(r *pending) (int, error) {
	h, err := c.request(cmdRead, 0, r.off, uint32(len(r.p)), nil)
	if err != nil {
		_ = c.c.Close()
		c.broken = fmt.Errorf("nbd: connection unusable after an earlier error: %w", err)
		return 0, err
	}
	byHandle := map[uint64]*pending{h: r}
	var soft error
	for !r.done {
		if _, err := c.replyChunk(byHandle); err != nil {
			var hd errHard
			if errors.As(err, &hd) {
				_ = c.c.Close()
				c.broken = fmt.Errorf("nbd: connection unusable after an earlier error: %w", hd.error)
				return 0, hd.error
			}
			if soft == nil {
				soft = err
			}
		}
	}
	if soft != nil {
		return 0, soft
	}
	return len(r.p), nil
}

// prefix is how many bytes of the first segment are complete from its start.
func prefix(reqs []*pending) int {
	n := 0
	for _, r := range reqs {
		if r.seg != 0 || !r.done || r.covered != uint64(len(r.p)) {
			break
		}
		n += len(r.p)
	}
	return n
}

// replyChunk reads one reply chunk (structured) or one simple reply and applies it to its request.
// It returns the request it belonged to. Errors wrapped in errHard leave the stream unusable; others
// end only their request (which is then marked done).
func (c *Conn) replyChunk(byHandle map[uint64]*pending) (*pending, error) {
	rd := &reader{r: c.r}
	magic := rd.u32()
	if rd.err != nil {
		return nil, errHard{rd.err}
	}
	switch magic {
	case simpleReplyMagic:
		errno := rd.u32()
		h := rd.u64()
		if rd.err != nil {
			return nil, errHard{rd.err}
		}
		r := byHandle[h]
		if r == nil || r.done {
			return nil, errHard{fmt.Errorf("nbd: reply for handle %d, which is not in flight", h)}
		}
		r.done = true
		if errno != 0 {
			return r, fmt.Errorf("nbd: server error %d", errno)
		}
		// a simple reply to a read in structured mode carries no data: the coverage check fails
		// unless the read was empty
		return r, c.covered(r)
	case structMagic:
		fl := rd.u16()
		typ := rd.u16()
		h := rd.u64()
		n := rd.u32()
		if rd.err != nil {
			return nil, errHard{rd.err}
		}
		r := byHandle[h]
		if r == nil || r.done {
			return nil, errHard{fmt.Errorf("nbd: reply chunk for handle %d, which is not in flight", h)}
		}
		if n > MaxPayload+16 {
			return nil, errHard{fmt.Errorf("nbd: oversized reply chunk (%d)", n)}
		}
		last := fl&srDone != 0
		if typ&srErrorBit != 0 {
			body := rd.bytes(n)
			if rd.err != nil {
				return nil, errHard{rd.err}
			}
			var code uint32
			if len(body) >= 4 {
				code = binary.BigEndian.Uint32(body[:4])
			}
			err := fmt.Errorf("nbd: server error chunk %#x code %d", typ, code)
			if r.failed {
				err = nil // already reported
			}
			r.failed = true
			r.done = last
			return r, err
		}
		if r.failed {
			if _, err := io.CopyN(io.Discard, c.r, int64(n)); err != nil {
				return nil, errHard{err}
			}
			r.done = last
			return r, nil
		}
		lr := &reader{r: io.LimitReader(c.r, int64(n))}
		switch typ {
		case srOffsetData:
			if n < 8 {
				return nil, errHard{errors.New("nbd: short data chunk")}
			}
			o := lr.u64()
			if lr.err != nil {
				return nil, errHard{lr.err}
			}
			dl := uint64(n) - 8
			if o < r.off || o+dl > r.off+uint64(len(r.p)) {
				return nil, errHard{errors.New("nbd: data chunk outside the requested range")}
			}
			if err := r.claim(o, dl); err != nil {
				return nil, errHard{err}
			}
			lr.full(r.p[o-r.off : o-r.off+dl])
		case srOffsetHole:
			if n != 12 {
				return nil, errHard{errors.New("nbd: malformed hole chunk")}
			}
			o := lr.u64()
			l := lr.u32()
			if lr.err != nil {
				return nil, errHard{lr.err}
			}
			if o < r.off || o+uint64(l) > r.off+uint64(len(r.p)) {
				return nil, errHard{errors.New("nbd: hole chunk outside the requested range")}
			}
			if err := r.claim(o, uint64(l)); err != nil {
				return nil, errHard{err}
			}
			clear(r.p[o-r.off : o-r.off+uint64(l)])
		case srNone:
		default:
			return nil, errHard{fmt.Errorf("nbd: unexpected chunk type %d in read reply", typ)}
		}
		if lr.err != nil {
			return nil, errHard{lr.err}
		}
		if _, err := io.Copy(io.Discard, lr.r); err != nil {
			return nil, errHard{err}
		}
		if !last {
			return r, nil
		}
		r.done = true
		return r, c.covered(r)
	default:
		return nil, errHard{fmt.Errorf("nbd: bad reply magic %#x", magic)}
	}
}

func (c *Conn) covered(r *pending) error {
	if r.covered != uint64(len(r.p)) {
		return fmt.Errorf("nbd: read reply covered %d of %d bytes", r.covered, len(r.p))
	}
	return nil
}
