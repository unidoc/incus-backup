package fscat

import (
	"bytes"
	"fmt"
	"testing"
)

// writeCounter records the largest single write it receives.
type writeCounter struct {
	bytes.Buffer
	maxWrite int
}

func (w *writeCounter) Write(p []byte) (int, error) {
	w.maxWrite = max(w.maxWrite, len(p))
	return w.Buffer.Write(p)
}

// Encode streams the catalog in bounded pieces and produces exactly the bytes of Bytes.
func TestEncodeStreamsByteIdenticalToBytes(t *testing.T) {
	root := &Entry{Kind: KDir, Mode: 0o755}
	for d := 0; d < 40; d++ {
		dir := &Entry{Kind: KDir, Name: fmt.Sprintf("directory-%03d", d), Mode: 0o755}
		for f := 0; f < 1000; f++ {
			e := &Entry{Kind: KFile, Name: fmt.Sprintf("a-rather-long-file-name-%05d.txt", f), Mode: 0o644, Size: uint64(f + 1), PayOff: uint64(d*1000 + f), MtimeNs: int64(f)}
			e.Sum[0] = byte(f)
			dir.Children = append(dir.Children, e)
		}
		root.Children = append(root.Children, dir)
	}
	tr := &Tree{Root: root, Count: 1 + 40*1001}
	want, err := tr.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if len(want) < 3*flushAt {
		t.Fatalf("test catalog too small (%d bytes) to exercise flushing", len(want))
	}
	var w writeCounter
	if err := tr.Encode(&w); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Bytes(), want) {
		t.Fatal("streamed encoding differs from Bytes")
	}
	if w.maxWrite > flushAt+64<<10 {
		t.Fatalf("a single write of %d bytes: the encoding was not streamed", w.maxWrite)
	}
	if _, err := Decode(w.Bytes()); err != nil {
		t.Fatal(err)
	}
}
