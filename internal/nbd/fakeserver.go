package nbd

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// Export is one disk served by a FakeServer. It is the test double for QEMU's
// NBD server: data is held in memory, and BLOCK_STATUS is answered for
// base:allocation and for any "qemu:dirty-bitmap:NAME" listed in Bitmaps.
type Export struct {
	Data    []byte
	Writes  bool
	Dirty   map[string]func(off uint64) bool // bitmap name -> is the 64 KiB block at off dirty
	Granule uint64
	// BreakAfter, if >0, closes the connection after that many read requests
	// (simulates a server dying mid-backup).
	BreakAfter int
	// Fault injection for the reply demultiplexer (tests only): FailRead answers a read with some
	// data and then an error chunk; ShortRead leaves the last piece of a read unanswered; Overlap
	// sends two chunks that cover the same bytes; BadHandle answers the first read with a handle
	// nobody asked for.
	FailRead, ShortRead, Overlap func(off uint64) bool
	BadHandle                    bool
	// Stall, when set, holds every read until the channel is closed (a hung server).
	Stall chan struct{}
	// Content, when set, replaces Data for test disks too large to hold in memory: it fills p with
	// the bytes at off, and Size is the export size. Zero, when set, answers base:allocation for a
	// granule without generating it. Such an export is read-only.
	Content func(p []byte, off uint64)
	Size    uint64
	Zero    func(off, n uint64) bool
	mu      sync.Mutex
	// read requests received and not yet fully answered (Interleave mode), and the peak of it
	inflight, maxInflight atomic.Int64
}

func (e *Export) size() uint64 {
	if e.Content != nil {
		return e.Size
	}
	return uint64(len(e.Data))
}

// read copies [off, off+len(p)) into p (the range is checked by the caller).
func (e *Export) read(p []byte, off uint64) {
	if e.Content != nil {
		e.Content(p, off)
		return
	}
	e.mu.Lock()
	copy(p, e.Data[off:])
	e.mu.Unlock()
}

// MaxInFlight is the largest number of read requests the server held at once (Interleave mode).
// The client had at least this many in flight.
func (e *Export) MaxInFlight() int64 { return e.maxInflight.Load() }

// FakeServer serves exports over any net.Conn.
type FakeServer struct {
	Exports map[string]*Export
	// MaxRequest is the maximum request size the server advertises (default 1 MiB, which exercises
	// the client's request splitting; QEMU advertises 32 MiB).
	MaxRequest uint32
	// Interleave makes the server behave like a concurrent one (QEMU): read requests that are
	// queued are answered together, last first, with the reply chunks of different requests
	// interleaved, as the NBD protocol allows.
	Interleave bool
}

// Serve handles one client connection until it ends.
func (s *FakeServer) Serve(ctx context.Context, c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	_, _ = c.Write(append([]byte("NBDMAGICIHAVEOPT"), 0, 3)) // fixed newstyle + no zeroes
	var cf [4]byte
	if _, err := io.ReadFull(r, cf[:]); err != nil {
		return
	}
	var exp *Export
	var ctxIDs = map[string]uint32{}
	var structured bool
	var wantCtx []string
	for exp == nil {
		var h [16]byte
		if _, err := io.ReadFull(r, h[:]); err != nil || string(h[:8]) != "IHAVEOPT" {
			return
		}
		opt := binary.BigEndian.Uint32(h[8:])
		n := binary.BigEndian.Uint32(h[12:])
		data := make([]byte, n)
		if _, err := io.ReadFull(r, data); err != nil {
			return
		}
		reply := func(typ uint32, p []byte) {
			b := binary.BigEndian.AppendUint64(nil, 0x3e889045565a9)
			b = binary.BigEndian.AppendUint32(b, opt)
			b = binary.BigEndian.AppendUint32(b, typ)
			b = binary.BigEndian.AppendUint32(b, uint32(len(p)))
			_, _ = c.Write(append(b, p...))
		}
		switch opt {
		case optStructured:
			structured = true
			reply(repAck, nil)
		case optSetMetaCtx:
			nl := binary.BigEndian.Uint32(data)
			name := string(data[4 : 4+nl])
			e := s.Exports[name]
			q := binary.BigEndian.Uint32(data[4+nl:])
			p := data[8+nl:]
			for i := uint32(0); i < q; i++ {
				l := binary.BigEndian.Uint32(p)
				ctxName := string(p[4 : 4+l])
				p = p[4+l:]
				ok := ctxName == "base:allocation"
				if e != nil && len(ctxName) > 18 && ctxName[:18] == "qemu:dirty-bitmap:" {
					_, ok = e.Dirty[ctxName[18:]]
				}
				if ok {
					id := uint32(len(ctxIDs))
					ctxIDs[ctxName] = id
					wantCtx = append(wantCtx, ctxName)
					reply(repMetaContext, append(binary.BigEndian.AppendUint32(nil, id), ctxName...))
				}
			}
			reply(repAck, nil)
		case optGo:
			nl := binary.BigEndian.Uint32(data)
			name := string(data[:0])
			name = string(data[4 : 4+nl])
			e, ok := s.Exports[name]
			if !ok {
				reply(0x80000000|8, []byte("unknown export"))
				continue
			}
			info := binary.BigEndian.AppendUint16(nil, infoExport)
			info = binary.BigEndian.AppendUint64(info, e.size())
			info = binary.BigEndian.AppendUint16(info, 1)
			reply(repInfo, info)
			bs := binary.BigEndian.AppendUint16(nil, infoBlockSize)
			bs = binary.BigEndian.AppendUint32(bs, 1)
			bs = binary.BigEndian.AppendUint32(bs, 4096)
			maxReq := s.MaxRequest
			if maxReq == 0 {
				maxReq = 1 << 20 // 1 MiB max request: exercises request splitting
			}
			bs = binary.BigEndian.AppendUint32(bs, maxReq)
			reply(repInfo, bs)
			reply(repAck, nil)
			exp = e
		default:
			reply(0x80000000|1, []byte("unsupported"))
		}
	}
	_ = structured
	var wmu sync.Mutex
	write := func(b []byte) {
		wmu.Lock()
		_, _ = c.Write(b)
		wmu.Unlock()
	}
	var rq chan readReq
	responderDone := make(chan struct{})
	if s.Interleave {
		rq = make(chan readReq, 64)
		go respond(exp, rq, write, responderDone)
		defer func() { close(rq); <-responderDone }()
	} else {
		close(responderDone)
	}
	reads := 0
	badHandleUsed := false
	for {
		var q [28]byte
		if _, err := io.ReadFull(r, q[:]); err != nil {
			return
		}
		typ := binary.BigEndian.Uint16(q[6:])
		handle := binary.BigEndian.Uint64(q[8:])
		off := binary.BigEndian.Uint64(q[16:])
		n := binary.BigEndian.Uint32(q[24:])
		chunk := func(flags, ctype uint16, p []byte) {
			write(replyChunk(handle, flags, ctype, p))
		}
		simple := func(errno uint32) {
			b := binary.BigEndian.AppendUint32(nil, simpleReplyMagic)
			b = binary.BigEndian.AppendUint32(b, errno)
			b = binary.BigEndian.AppendUint64(b, handle)
			write(b)
		}
		switch typ {
		case cmdRead:
			reads++
			if exp.BreakAfter > 0 && reads > exp.BreakAfter {
				return
			}
			if exp.BadHandle && !badHandleUsed {
				badHandleUsed = true
				handle += 1 << 40
			}
			if exp.Stall != nil {
				<-exp.Stall
			}
			if s.Interleave {
				if v := exp.inflight.Add(1); v > exp.maxInflight.Load() {
					exp.maxInflight.Store(v)
				}
				select {
				case rq <- readReq{handle, off, n}:
				case <-responderDone:
					return
				}
				continue
			}
			for _, b := range readReply(exp, handle, off, n) {
				write(b)
			}
		case cmdWrite:
			buf := make([]byte, n)
			if _, err := io.ReadFull(r, buf); err != nil {
				return
			}
			if !exp.Writes || exp.Content != nil || off+uint64(n) > uint64(len(exp.Data)) {
				simple(1)
				continue
			}
			exp.mu.Lock()
			copy(exp.Data[off:], buf)
			exp.mu.Unlock()
			simple(0)
		case cmdFlush:
			simple(0)
		case cmdDisc:
			return
		case cmdBlockStatus:
			// find which context this is: the client asked for them in order
			var p []byte
			for _, name := range wantCtx {
				id := ctxIDs[name]
				ext := s.extents(exp, name, off, uint64(n))
				body := binary.BigEndian.AppendUint32(nil, id)
				for _, e := range ext {
					body = binary.BigEndian.AppendUint32(body, uint32(e.Length))
					body = binary.BigEndian.AppendUint32(body, e.Flags)
				}
				p = append(p, byte(len(body)>>24), byte(len(body)>>16), byte(len(body)>>8), byte(len(body)))
				p = append(p, body...)
			}
			// one chunk per context; REQ_ONE clients only look at theirs
			rest := p
			for len(rest) > 0 {
				l := binary.BigEndian.Uint32(rest)
				body := rest[4 : 4+l]
				rest = rest[4+l:]
				fl := uint16(0)
				if len(rest) == 0 {
					fl = srDone
				}
				chunk(fl, srBlockStatus, body)
			}
			if len(wantCtx) == 0 {
				chunk(srDone, srNone, nil)
			}
		default:
			simple(22)
		}
	}
}

type readReq struct {
	handle, off uint64
	n           uint32
}

func replyChunk(handle uint64, flags, ctype uint16, p []byte) []byte {
	b := make([]byte, 0, 20+len(p))
	b = binary.BigEndian.AppendUint32(b, structMagic)
	b = binary.BigEndian.AppendUint16(b, flags)
	b = binary.BigEndian.AppendUint16(b, ctype)
	b = binary.BigEndian.AppendUint64(b, handle)
	b = binary.BigEndian.AppendUint32(b, uint32(len(p)))
	return append(b, p...)
}

// readReply builds the structured reply chunks of one read: all-zero ranges as a hole, data in
// two pieces (so multi-chunk replies are exercised), plus the injected faults.
func readReply(exp *Export, handle, off uint64, n uint32) [][]byte {
	if off+uint64(n) > exp.size() {
		return [][]byte{replyChunk(handle, srDone, srErrorBit|1, binary.BigEndian.AppendUint32(append(binary.BigEndian.AppendUint32(nil, 22), 0, 0), 0)[:6])}
	}
	d := make([]byte, n)
	exp.read(d, off)
	data := func(flags uint16, o uint64, p []byte) []byte {
		return replyChunk(handle, flags, srOffsetData, append(binary.BigEndian.AppendUint64(nil, o), p...))
	}
	half := uint32(len(d) / 2)
	switch {
	case exp.FailRead != nil && exp.FailRead(off):
		return [][]byte{data(0, off, d[:half]), replyChunk(handle, srDone, srErrorBit|1, binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 5), 0))}
	case exp.ShortRead != nil && exp.ShortRead(off):
		return [][]byte{data(srDone, off, d[:half])}
	case exp.Overlap != nil && exp.Overlap(off):
		// two chunks that both cover the first half: the byte count adds up, the second half is never sent
		return [][]byte{data(0, off, d[:half]), data(srDone, off, d[:len(d)-int(half)])}
	}
	// serve all-zero regions as holes so the client's hole handling is exercised
	zero := true
	for _, b := range d {
		if b != 0 {
			zero = false
			break
		}
	}
	if zero {
		p := binary.BigEndian.AppendUint64(nil, off)
		p = binary.BigEndian.AppendUint32(p, n)
		return [][]byte{replyChunk(handle, srDone, srOffsetHole, p)}
	}
	return [][]byte{data(0, off, d[:half]), data(srDone, off+uint64(half), d[half:])}
}

// respond answers queued reads like a concurrent server: everything queued is answered together,
// newest request first, one chunk of each in turn.
func respond(exp *Export, rq <-chan readReq, write func([]byte), done chan<- struct{}) {
	defer close(done)
	for first := range rq {
		batch := []readReq{first}
	more:
		for len(batch) < 8 {
			select {
			case q, ok := <-rq:
				if !ok {
					break more
				}
				batch = append(batch, q)
			default:
				break more
			}
		}
		var lists [][][]byte
		for i := len(batch) - 1; i >= 0; i-- {
			lists = append(lists, readReply(exp, batch[i].handle, batch[i].off, batch[i].n))
		}
		for k := 0; ; k++ {
			any := false
			for _, l := range lists {
				if k < len(l) {
					if k == len(l)-1 {
						exp.inflight.Add(-1) // before the last chunk: the client cannot have seen it end yet
					}
					write(l[k])
					any = true
				}
			}
			if !any {
				break
			}
		}
	}
}

// extents computes status extents for [off, off+n): for allocation, non-zero
// blocks are data (flags 0) and zero blocks are hole+zero (flags 3); for a dirty
// bitmap, flags 1 marks dirty granules. At most 256 extents are returned so the
// client's "keep asking until covered" loop is exercised.
func (s *FakeServer) extents(e *Export, name string, off, n uint64) []Extent {
	g := e.Granule
	if g == 0 {
		g = 65536
	}
	size := e.size()
	var out []Extent
	var blk []byte
	flagAt := func(o uint64) uint32 {
		start, stop := o-o%g, min64(o-o%g+g, size)
		if name == "base:allocation" {
			if e.Zero != nil {
				if e.Zero(start, stop-start) {
					return 3
				}
				return 0
			}
			if cap(blk) < int(stop-start) {
				blk = make([]byte, stop-start)
			}
			blk = blk[:stop-start]
			e.read(blk, start)
			for _, b := range blk {
				if b != 0 {
					return 0
				}
			}
			return 3
		}
		if f := e.Dirty[name[18:]]; f != nil && f(o-o%g) {
			return 1
		}
		return 0
	}
	pos := off
	end := off + n
	for pos < end && len(out) < 256 {
		next := pos - pos%g + g
		if next > end {
			next = end
		}
		if next > size {
			next = size
		}
		fl := flagAt(pos)
		if k := len(out); k > 0 && out[k-1].Flags == fl {
			out[k-1].Length += next - pos
		} else {
			out = append(out, Extent{Offset: pos, Length: next - pos, Flags: fl})
		}
		pos = next
	}
	return out
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

// Pipe returns a client-side connection served by this FakeServer.
func (s *FakeServer) Pipe(ctx context.Context) net.Conn {
	a, b := net.Pipe()
	go s.Serve(ctx, b)
	return a
}

var _ = fmt.Sprintf
