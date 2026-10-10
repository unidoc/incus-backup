// Package chunker implements content-defined chunking (FastCDC with
// normalised chunking) for filesystem archive streams. Block objects use fixed
// 4 MiB chunks and need no chunker.
//
// The gear table is derived from a fixed seed with a fixed generator, so chunk
// boundaries (and therefore deduplication) are stable across versions and hosts.
// Changing it would silently destroy deduplication against old backups: it is
// part of the repository format.
package chunker

import (
	"errors"
	"io"
)

// Params are the chunk size bounds in bytes.
type Params struct {
	Min, Avg, Max int
}

// Default is used for archive streams: average 2 MiB.
var Default = Params{Min: 256 << 10, Avg: 2 << 20, Max: 8 << 20}

var gear [256]uint64

func init() {
	// splitmix64 with a fixed seed.
	x := uint64(0x696e6375732d6261) // "incus-ba"
	for i := range gear {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		gear[i] = z ^ (z >> 31)
	}
}

func log2(n int) uint {
	b := uint(0)
	for n > 1 {
		n >>= 1
		b++
	}
	return b
}

func mask(bits uint) uint64 { return ((uint64(1) << bits) - 1) << (64 - bits) }

// Cut returns the length of the next chunk at the start of data. If atEOF is
// false and data is shorter than Max with no boundary found it returns 0, meaning
// "need more data".
func (p Params) Cut(data []byte, atEOF bool) int {
	n := len(data)
	if n <= p.Min {
		if atEOF {
			return n
		}
		return 0
	}
	max := p.Max
	if n < max {
		max = n
	}
	bits := log2(p.Avg)
	maskS, maskL := mask(bits+2), mask(bits-2)
	normal := p.Avg
	if normal > max {
		normal = max
	}
	var h uint64
	i := p.Min
	// warm the hash over the minimum window so it only depends on nearby bytes
	start := i - 64
	if start < 0 {
		start = 0
	}
	for j := start; j < i; j++ {
		h = (h << 1) + gear[data[j]]
	}
	for ; i < normal; i++ {
		h = (h << 1) + gear[data[i]]
		if h&maskS == 0 {
			return i + 1
		}
	}
	for ; i < max; i++ {
		h = (h << 1) + gear[data[i]]
		if h&maskL == 0 {
			return i + 1
		}
	}
	if max == p.Max || atEOF {
		return max
	}
	return 0
}

// Splitter cuts an io.Reader into chunks.
//
// buf[start:end] holds the bytes read but not yet returned. Unconsumed bytes are moved to the
// front only when a refill is needed, so each input byte is copied at most about once instead of
// once per chunk cut before it. Boundaries depend only on the stream: Cut is always called with
// at least Max bytes available or at end of input, exactly as before.
type Splitter struct {
	r          io.Reader
	p          Params
	buf        []byte
	start, end int
	eof        bool
}

// NewSplitter returns a Splitter. The params must satisfy 0 < Min < Avg < Max.
func NewSplitter(r io.Reader, p Params) (*Splitter, error) {
	if p.Min <= 0 || p.Avg <= p.Min || p.Max <= p.Avg || p.Avg < 1<<10 {
		return nil, errors.New("chunker: invalid parameters")
	}
	return &Splitter{r: r, p: p, buf: make([]byte, 2*p.Max)}, nil
}

// Next returns the next chunk (a fresh slice), or io.EOF when the input is done.
func (s *Splitter) Next() ([]byte, error) {
	for {
		if !s.eof && s.end-s.start < s.p.Max {
			if s.start > 0 {
				s.end = copy(s.buf, s.buf[s.start:s.end])
				s.start = 0
			}
			m, err := io.ReadFull(s.r, s.buf[s.end:])
			s.end += m
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				s.eof = true
			} else if err != nil {
				return nil, err
			}
		}
		if s.end == s.start {
			return nil, io.EOF
		}
		c := s.p.Cut(s.buf[s.start:s.end], s.eof)
		if c == 0 {
			if s.eof {
				c = s.end - s.start
			} else {
				continue
			}
		}
		out := append([]byte(nil), s.buf[s.start:s.start+c]...)
		s.start += c
		return out, nil
	}
}
