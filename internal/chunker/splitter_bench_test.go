package chunker

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand"
	"testing"
)

// origSplitter is the Splitter as it was before the copy-avoidance change, kept verbatim as the
// reference: chunk boundaries are part of the repository format.
type origSplitter struct {
	r   io.Reader
	p   Params
	buf []byte
	n   int
	eof bool
}

func newOrigSplitter(r io.Reader, p Params) (*origSplitter, error) {
	if p.Min <= 0 || p.Avg <= p.Min || p.Max <= p.Avg || p.Avg < 1<<10 {
		return nil, errors.New("chunker: invalid parameters")
	}
	return &origSplitter{r: r, p: p, buf: make([]byte, 2*p.Max)}, nil
}

func (s *origSplitter) Next() ([]byte, error) {
	for {
		if !s.eof && s.n < s.p.Max {
			m, err := io.ReadFull(s.r, s.buf[s.n:s.p.Max+s.p.Max/2])
			s.n += m
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				s.eof = true
			} else if err != nil {
				return nil, err
			}
		}
		if s.n == 0 {
			return nil, io.EOF
		}
		c := s.p.Cut(s.buf[:s.n], s.eof)
		if c == 0 {
			if s.eof {
				c = s.n
			} else {
				continue
			}
		}
		out := append([]byte(nil), s.buf[:c]...)
		copy(s.buf, s.buf[c:s.n])
		s.n -= c
		return out, nil
	}
}

// trickleReader returns at most a few random bytes per Read, to exercise every refill path.
type trickleReader struct {
	r   io.Reader
	rng *rand.Rand
	max int
}

func (t *trickleReader) Read(p []byte) (int, error) {
	n := 1 + t.rng.Intn(t.max)
	if n < len(p) {
		p = p[:n]
	}
	return t.r.Read(p)
}

// The Splitter must cut exactly where the original implementation cut, for every input length,
// parameter set and read pattern.
func TestSplitterBoundariesMatchTheOriginal(t *testing.T) {
	params := []Params{Default, {Min: 2 << 10, Avg: 8 << 10, Max: 32 << 10}, {Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}}
	rng := rand.New(rand.NewSource(42))
	for pi, p := range params {
		for _, size := range []int{0, 1, p.Min - 1, p.Min, p.Min + 1, p.Avg, p.Max - 1, p.Max, p.Max + 1, 3*p.Max + 17, 10*p.Max + 5, 37 * p.Avg} {
			data := make([]byte, size)
			rng.Read(data)
			if size > 1000 {
				// a long run of zeros and a repeating pattern: boundaries at Max
				for i := size / 3; i < size/3+p.Max+100 && i < size; i++ {
					data[i] = 0
				}
			}
			for _, trickle := range []int{0, 7, 4096, 3 << 20} {
				mk := func() io.Reader {
					var r io.Reader = bytes.NewReader(data)
					if trickle > 0 {
						r = &trickleReader{r: r, rng: rand.New(rand.NewSource(int64(size))), max: trickle}
					}
					return r
				}
				o, _ := newOrigSplitter(mk(), p)
				n, _ := NewSplitter(mk(), p)
				for k := 0; ; k++ {
					a, ea := o.Next()
					b, eb := n.Next()
					if ea != eb || !bytes.Equal(a, b) {
						t.Fatalf("params %d size %d trickle %d chunk %d: original %d bytes (%v), new %d bytes (%v)", pi, size, trickle, k, len(a), ea, len(b), eb)
					}
					if ea == io.EOF {
						break
					}
				}
			}
		}
	}
}

// The returned chunk must not be overwritten by later calls (callers hand it to other goroutines).
func TestSplitterChunksStayValid(t *testing.T) {
	data := randBytes(40<<20, 9)
	s, _ := NewSplitter(bytes.NewReader(data), Default)
	var chunks [][]byte
	var sums [][32]byte
	for {
		c, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, c)
		sums = append(sums, sha256.Sum256(c))
	}
	var all []byte
	for i, c := range chunks {
		if sha256.Sum256(c) != sums[i] {
			t.Fatalf("chunk %d changed after later Next calls", i)
		}
		all = append(all, c...)
	}
	if !bytes.Equal(all, data) {
		t.Fatal("reassembly differs")
	}
}

func benchSplit(b *testing.B, next func(io.Reader) func() ([]byte, error)) {
	data := randBytes(64<<20, 1)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		nx := next(bytes.NewReader(data))
		for {
			if _, err := nx(); err == io.EOF {
				break
			} else if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkSplitter(b *testing.B) {
	benchSplit(b, func(r io.Reader) func() ([]byte, error) { s, _ := NewSplitter(r, Default); return s.Next })
}

func BenchmarkSplitterOriginal(b *testing.B) {
	benchSplit(b, func(r io.Reader) func() ([]byte, error) { s, _ := newOrigSplitter(r, Default); return s.Next })
}
