// Package nbd is a small, strict NBD client (newstyle, structured replies) with
// the pieces incus-backup needs: reads that understand holes, BLOCK_STATUS for
// base:allocation and qemu:dirty-bitmap:<name>, and writes for restore.
//
// Every read of the wire checks its error and every length is bounded: a
// misbehaving server must produce an error, never a panic or an unbounded
// allocation.
package nbd

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	optGo            = 7
	optStructured    = 8
	optSetMetaCtx    = 10
	repAck           = 1
	repInfo          = 3
	repMetaContext   = 4
	infoExport       = 0
	infoBlockSize    = 3
	cmdRead          = 0
	cmdWrite         = 1
	cmdDisc          = 2
	cmdFlush         = 3
	cmdBlockStatus   = 7
	flagReqOne       = 1 << 3
	reqMagic         = 0x25609513
	simpleReplyMagic = 0x67446698
	structMagic      = 0x668e33ef
	optReplyMagic    = 0x3e889045565a9
	srDone           = 1
	srNone           = 0
	srOffsetData     = 1
	srOffsetHole     = 2
	srBlockStatus    = 5
	srErrorBit       = 1 << 15

	// MaxPayload bounds any single reply payload we accept.
	MaxPayload = 64 << 20
	// DefaultRequest is the largest read/write we issue in one request.
	DefaultRequest = 4 << 20
)

// Extent is a status extent.
type Extent struct {
	Offset, Length uint64
	Flags          uint32
}

// Conn is a negotiated NBD connection to one export.
type Conn struct {
	c       net.Conn
	r       *bufio.Reader
	mu      sync.Mutex
	handle  uint64
	size    uint64
	maxReq  uint32
	Context map[string]uint32 // granted metadata contexts name -> id
	// Depth is how many read requests may be in flight at once (0 = DefaultDepth). Set it before
	// the first read; it never changes what is read, only how many requests overlap.
	Depth  int
	closed atomic.Bool
	// broken is set when the reply stream can no longer be trusted (a protocol violation or a
	// transport error in the middle of a pipelined read): every later call fails with it.
	broken error
}

// Size returns the export size in bytes.
func (c *Conn) Size() uint64 { return c.size }

type reader struct {
	r   io.Reader
	err error
}

func (r *reader) u16() uint16 { var b [2]byte; r.full(b[:]); return binary.BigEndian.Uint16(b[:]) }
func (r *reader) u32() uint32 { var b [4]byte; r.full(b[:]); return binary.BigEndian.Uint32(b[:]) }
func (r *reader) u64() uint64 { var b [8]byte; r.full(b[:]); return binary.BigEndian.Uint64(b[:]) }
func (r *reader) full(p []byte) {
	if r.err != nil {
		return
	}
	_, r.err = io.ReadFull(r.r, p)
}
func (r *reader) bytes(n uint32) []byte {
	if r.err != nil {
		return nil
	}
	if n > MaxPayload {
		r.err = fmt.Errorf("nbd: server sent an oversized field (%d bytes)", n)
		return nil
	}
	b := make([]byte, n)
	r.full(b)
	return b
}

// Dial negotiates export on an established connection. contexts are metadata
// context names to request (e.g. "base:allocation", "qemu:dirty-bitmap:ib-x").
// The returned Conn owns nc.
func Dial(ctx context.Context, nc net.Conn, export string, contexts []string) (*Conn, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
		defer nc.SetDeadline(time.Time{})
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = nc.Close()
		case <-done:
		}
	}()
	c := &Conn{c: nc, r: bufio.NewReaderSize(nc, 1<<20), Context: map[string]uint32{}, maxReq: DefaultRequest}
	if err := c.negotiate(export, contexts); err != nil {
		_ = nc.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return c, nil
}

func (c *Conn) option(opt uint32, data []byte) error {
	b := make([]byte, 16+len(data))
	copy(b, "IHAVEOPT")
	binary.BigEndian.PutUint32(b[8:], opt)
	binary.BigEndian.PutUint32(b[12:], uint32(len(data)))
	copy(b[16:], data)
	_, err := c.c.Write(b)
	return err
}

func (c *Conn) optReply(want uint32) (typ uint32, data []byte, err error) {
	rd := &reader{r: c.r}
	if m := rd.u64(); rd.err == nil && m != optReplyMagic {
		return 0, nil, fmt.Errorf("nbd: bad option reply magic %#x", m)
	}
	opt := rd.u32()
	typ = rd.u32()
	n := rd.u32()
	data = rd.bytes(n)
	if rd.err != nil {
		return 0, nil, rd.err
	}
	if opt != want {
		return 0, nil, fmt.Errorf("nbd: reply for option %d, expected %d", opt, want)
	}
	if typ&0x80000000 != 0 {
		return typ, data, fmt.Errorf("nbd: server refused option %d (reply %#x: %s)", want, typ, string(data))
	}
	return typ, data, nil
}

func (c *Conn) negotiate(export string, contexts []string) error {
	rd := &reader{r: c.r}
	hdr := rd.bytes(18)
	if rd.err != nil {
		return fmt.Errorf("nbd: reading handshake: %w", rd.err)
	}
	if string(hdr[:8]) != "NBDMAGIC" || string(hdr[8:16]) != "IHAVEOPT" {
		return errors.New("nbd: not a newstyle NBD server")
	}
	if binary.BigEndian.Uint16(hdr[16:])&1 == 0 {
		return errors.New("nbd: server lacks fixed newstyle negotiation")
	}
	var cf [4]byte
	binary.BigEndian.PutUint32(cf[:], 1|2) // FIXED_NEWSTYLE | NO_ZEROES
	if _, err := c.c.Write(cf[:]); err != nil {
		return err
	}
	if err := c.option(optStructured, nil); err != nil {
		return err
	}
	if typ, _, err := c.optReply(optStructured); err != nil || typ != repAck {
		return fmt.Errorf("nbd: server does not support structured replies: %v", err)
	}
	if len(contexts) > 0 {
		d := binary.BigEndian.AppendUint32(nil, uint32(len(export)))
		d = append(d, export...)
		d = binary.BigEndian.AppendUint32(d, uint32(len(contexts)))
		for _, q := range contexts {
			d = binary.BigEndian.AppendUint32(d, uint32(len(q)))
			d = append(d, q...)
		}
		if err := c.option(optSetMetaCtx, d); err != nil {
			return err
		}
		for {
			typ, data, err := c.optReply(optSetMetaCtx)
			if err != nil {
				return err
			}
			if typ == repAck {
				break
			}
			if typ == repMetaContext && len(data) >= 4 {
				c.Context[string(data[4:])] = binary.BigEndian.Uint32(data[:4])
			}
		}
	}
	d := binary.BigEndian.AppendUint32(nil, uint32(len(export)))
	d = append(d, export...)
	d = binary.BigEndian.AppendUint16(d, 2) // two info requests
	d = binary.BigEndian.AppendUint16(d, infoExport)
	d = binary.BigEndian.AppendUint16(d, infoBlockSize)
	if err := c.option(optGo, d); err != nil {
		return err
	}
	gotSize := false
	for {
		typ, data, err := c.optReply(optGo)
		if err != nil {
			return err
		}
		if typ == repAck {
			break
		}
		if typ != repInfo || len(data) < 2 {
			continue
		}
		switch binary.BigEndian.Uint16(data[:2]) {
		case infoExport:
			if len(data) < 12 {
				return errors.New("nbd: short export info")
			}
			c.size = binary.BigEndian.Uint64(data[2:10])
			gotSize = true
		case infoBlockSize:
			if len(data) >= 14 {
				if max := binary.BigEndian.Uint32(data[10:14]); max >= 4096 && max < c.maxReq {
					c.maxReq = max &^ 4095
				}
			}
		}
	}
	if !gotSize {
		return errors.New("nbd: server did not report the export size")
	}
	return nil
}

func (c *Conn) request(typ uint16, flags uint16, off uint64, n uint32, payload []byte) (uint64, error) {
	c.handle++
	var b [28]byte
	binary.BigEndian.PutUint32(b[0:], reqMagic)
	binary.BigEndian.PutUint16(b[4:], flags)
	binary.BigEndian.PutUint16(b[6:], typ)
	binary.BigEndian.PutUint64(b[8:], c.handle)
	binary.BigEndian.PutUint64(b[16:], off)
	binary.BigEndian.PutUint32(b[24:], n)
	if _, err := c.c.Write(b[:]); err != nil {
		return 0, err
	}
	if len(payload) > 0 {
		if _, err := c.c.Write(payload); err != nil {
			return 0, err
		}
	}
	return c.handle, nil
}

type chunk struct {
	typ    uint16
	flags  uint16
	length uint32
}

// replies reads the reply (simple or structured) to handle, calling fn for
// every structured chunk's payload reader.
func (c *Conn) replies(handle uint64, fn func(typ uint16, n uint32, r *reader) error) error {
	rd := &reader{r: c.r}
	first := true
	for {
		magic := rd.u32()
		if rd.err != nil {
			return rd.err
		}
		switch magic {
		case simpleReplyMagic:
			errno := rd.u32()
			h := rd.u64()
			if rd.err != nil {
				return rd.err
			}
			if h != handle {
				return fmt.Errorf("nbd: reply handle %d, expected %d", h, handle)
			}
			if errno != 0 {
				return fmt.Errorf("nbd: server error %d", errno)
			}
			if first && fn != nil {
				// a simple reply to a read carries raw data; we never ask reads in simple mode
			}
			return nil
		case structMagic:
			fl := rd.u16()
			typ := rd.u16()
			h := rd.u64()
			n := rd.u32()
			if rd.err != nil {
				return rd.err
			}
			if h != handle {
				return fmt.Errorf("nbd: reply handle %d, expected %d", h, handle)
			}
			if n > MaxPayload+16 {
				return fmt.Errorf("nbd: oversized reply chunk (%d)", n)
			}
			first = false
			if typ&srErrorBit != 0 {
				body := rd.bytes(n)
				if rd.err != nil {
					return rd.err
				}
				var code uint32
				if len(body) >= 4 {
					code = binary.BigEndian.Uint32(body[:4])
				}
				return fmt.Errorf("nbd: server error chunk %#x code %d", typ, code)
			}
			lr := &reader{r: io.LimitReader(c.r, int64(n))}
			if err := fn(typ, n, lr); err != nil {
				return err
			}
			if lr.err != nil {
				return lr.err
			}
			// drain whatever fn did not consume
			if _, err := io.Copy(io.Discard, lr.r); err != nil {
				return err
			}
			if fl&srDone != 0 {
				return nil
			}
		default:
			return fmt.Errorf("nbd: bad reply magic %#x", magic)
		}
	}
}

func (c *Conn) check(off, n uint64) error {
	if c.closed.Load() {
		return errors.New("nbd: connection closed")
	}
	if c.broken != nil {
		return c.broken
	}
	if off+n < off || off+n > c.size {
		return fmt.Errorf("nbd: range %d+%d is outside the export (%d bytes)", off, n, c.size)
	}
	return nil
}

// ReadAt fills p from offset off. Holes are returned as zeros.
func (c *Conn) ReadAt(p []byte, off int64) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if off < 0 {
		return 0, errors.New("nbd: negative offset")
	}
	return c.readSegments([]Segment{{Off: off, Buf: p}})
}

// ReadBatch fills every segment (holes read as zeros), keeping up to Depth requests in flight
// across all of them. It is ReadAt for several ranges at once: the whole batch costs about one
// round trip instead of one per request. On error it reports the first failure; the buffers of
// segments before the failing one are complete.
func (c *Conn) ReadBatch(segs []Segment) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, sg := range segs {
		if sg.Off < 0 {
			return errors.New("nbd: negative offset")
		}
	}
	_, err := c.readSegments(segs)
	return err
}

// WriteAt writes p at offset off.
func (c *Conn) WriteAt(p []byte, off int64) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	done := 0
	for done < len(p) {
		n := len(p) - done
		if n > int(c.maxReq) {
			n = int(c.maxReq)
		}
		if err := c.check(uint64(off)+uint64(done), uint64(n)); err != nil {
			return done, err
		}
		h, err := c.request(cmdWrite, 0, uint64(off)+uint64(done), uint32(n), p[done:done+n])
		if err != nil {
			return done, err
		}
		if err := c.replies(h, func(typ uint16, n uint32, r *reader) error { return nil }); err != nil {
			return done, err
		}
		done += n
	}
	return done, nil
}

// Flush asks the server to persist writes.
func (c *Conn) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, err := c.request(cmdFlush, 0, 0, 0, nil)
	if err != nil {
		return err
	}
	return c.replies(h, func(uint16, uint32, *reader) error { return nil })
}

// Status returns the extents of metadata context name covering [off, off+n),
// following the server until the whole range is covered.
func (c *Conn) Status(name string, off, n uint64) ([]Extent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.Context[name]
	if !ok {
		return nil, fmt.Errorf("nbd: metadata context %q was not granted", name)
	}
	if err := c.check(off, n); err != nil {
		return nil, err
	}
	var out []Extent
	pos, end := off, off+n
	for pos < end {
		l := end - pos
		if l > 1<<30 {
			l = 1 << 30
		}
		h, err := c.request(cmdBlockStatus, flagReqOne, pos, uint32(l), nil)
		if err != nil {
			return nil, err
		}
		var got uint64
		err = c.replies(h, func(typ uint16, ln uint32, r *reader) error {
			if typ != srBlockStatus {
				return nil
			}
			cid := r.u32()
			if r.err != nil {
				return r.err
			}
			if cid != id {
				return nil
			}
			for rem := ln - 4; rem >= 8; rem -= 8 {
				el := r.u32()
				fl := r.u32()
				if r.err != nil {
					return r.err
				}
				if el == 0 {
					return errors.New("nbd: zero-length extent")
				}
				if pos+got+uint64(el) > end {
					el = uint32(end - pos - got) // servers may round up past the request
				}
				if k := len(out); k > 0 && out[k-1].Flags == fl && out[k-1].Offset+out[k-1].Length == pos+got {
					out[k-1].Length += uint64(el)
				} else {
					out = append(out, Extent{Offset: pos + got, Length: uint64(el), Flags: fl})
				}
				got += uint64(el)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if got == 0 {
			return nil, fmt.Errorf("nbd: no status returned at offset %d", pos)
		}
		pos += got
	}
	return out, nil
}

// Close ends the session. When no call is in progress it is polite (NBD_CMD_DISC); when one is
// (a read blocked on a stalled server, cancelled through context.AfterFunc), it closes the
// transport at once, which makes the blocked call return an error instead of hanging.
func (c *Conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	if c.mu.TryLock() {
		defer c.mu.Unlock()
		if c.broken == nil {
			_, _ = c.request(cmdDisc, 0, 0, 0, nil)
		}
	}
	return c.c.Close()
}
