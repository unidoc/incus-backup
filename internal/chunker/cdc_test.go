package chunker

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

func split(t *testing.T, data []byte, p Params) [][]byte {
	t.Helper()
	s, err := NewSplitter(bytes.NewReader(data), p)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for {
		c, err := s.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
}

func randBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func TestReassemblyBoundsAndDeterminism(t *testing.T) {
	p := Params{Min: 16 << 10, Avg: 64 << 10, Max: 256 << 10}
	data := randBytes(20<<20, 1)
	a := split(t, data, p)
	var re []byte
	for i, c := range a {
		re = append(re, c...)
		if len(c) > p.Max || (len(c) < p.Min && i != len(a)-1) {
			t.Fatalf("chunk %d has size %d outside [%d,%d]", i, len(c), p.Min, p.Max)
		}
	}
	if !bytes.Equal(re, data) {
		t.Fatal("chunks do not reassemble to the input")
	}
	b := split(t, data, p)
	if len(a) != len(b) {
		t.Fatal("not deterministic")
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatal("not deterministic")
		}
	}
	avg := len(data) / len(a)
	if avg < p.Avg/2 || avg > p.Avg*2 {
		t.Fatalf("average chunk %d is far from the target %d", avg, p.Avg)
	}
}

// The point of content-defined chunking: inserting bytes at the start must not
// change the chunks after the next boundary.
func TestShiftResilience(t *testing.T) {
	p := Params{Min: 16 << 10, Avg: 64 << 10, Max: 256 << 10}
	data := randBytes(16<<20, 2)
	shifted := append([]byte("inserted-prefix-bytes"), data...)
	a, b := split(t, data, p), split(t, shifted, p)
	seen := map[string]bool{}
	for _, c := range a {
		seen[string(c)] = true
	}
	same := 0
	for _, c := range b {
		if seen[string(c)] {
			same++
		}
	}
	if frac := float64(same) / float64(len(b)); frac < 0.95 {
		t.Fatalf("only %.0f%% of chunks survive a prefix insertion (fixed-size chunking would give ~0%%)", frac*100)
	}
}

func TestEdgeCases(t *testing.T) {
	p := Default
	if len(split(t, nil, p)) != 0 {
		t.Fatal("empty input must give no chunks")
	}
	if c := split(t, []byte("x"), p); len(c) != 1 || string(c[0]) != "x" {
		t.Fatal("tiny input")
	}
	zeros := make([]byte, 40<<20)
	cs := split(t, zeros, p)
	for _, c := range cs[:len(cs)-1] {
		if len(c) != p.Max {
			t.Fatalf("all-zero input must cut at Max, got %d", len(c))
		}
	}
	if _, err := NewSplitter(bytes.NewReader(nil), Params{Min: 10, Avg: 5, Max: 20}); err == nil {
		t.Fatal("bad parameters accepted")
	}
}

// The gear table is part of the repository format: pin its first values.
func TestGearTableIsPinned(t *testing.T) {
	want := [4]uint64{gear[0], gear[1], gear[2], gear[255]}
	if want[0] != 0x1b5a6bdf5f8f5ba8 && want[0] == 0 {
		t.Fatal("gear table not initialised")
	}
	// recompute independently
	x := uint64(0x696e6375732d6261)
	var g [256]uint64
	for i := range g {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		g[i] = z ^ (z >> 31)
	}
	if g != gear {
		t.Fatal("gear table changed: this breaks deduplication against existing backups")
	}
}
