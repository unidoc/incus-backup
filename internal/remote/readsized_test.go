package remote

import (
	"io"
	"runtime"
	"strings"
	"testing"
)

// A response that announces more than it sends must neither be accepted nor make the client
// allocate what it announced (in a sync run the peer is another server).
func TestReadSizedDoesNotTrustTheAnnouncedLength(t *testing.T) {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := readSized(strings.NewReader("tiny"), MaxResponse, MaxResponse)
	runtime.ReadMemStats(&after)
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("short body: %v", err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 2*presizeMax {
		t.Fatalf("a 4-byte body announcing %d bytes allocated %d", MaxResponse, got)
	}
	body := strings.Repeat("y", 4<<20)
	runtime.ReadMemStats(&before)
	b, err := readSized(strings.NewReader(body), int64(len(body)), MaxResponse)
	runtime.ReadMemStats(&after)
	if err != nil || string(b) != body {
		t.Fatalf("honest body: %v", err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > uint64(len(body))+1<<20 {
		t.Fatalf("a 4 MiB body allocated %d bytes", got)
	}
	// longer than announced: only the announced bytes are taken (the caller's parser decides)
	b, err = readSized(strings.NewReader("abcdef"), 3, MaxResponse)
	if err != nil || string(b) != "abc" {
		t.Fatalf("%q %v", b, err)
	}
}
