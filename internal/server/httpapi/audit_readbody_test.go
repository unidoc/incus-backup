package httpapi

import (
	"bytes"
	"io"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/unidoc/incus-backup/internal/datastore"
)

// stalled is a body that delivers a few bytes and then fails, like a peer that announced a large
// Content-Length and never sends it.
type stalled struct{ n int }

func (s *stalled) Read(p []byte) (int, error) {
	if s.n == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	k := min(len(p), s.n)
	s.n -= k
	return k, nil
}

// The memory readBody allocates must follow the bytes that actually arrived, not the length the
// peer announced: otherwise a peer can make the server allocate MaxBlobSize per HTTP/2 stream with
// nothing but headers.
func TestAuditReadBodyDoesNotTrustTheAnnouncedLength(t *testing.T) {
	var before, after runtime.MemStats
	const streams = 4
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < streams; i++ {
		r := httptest.NewRequest("PUT", "/v1/sessions/x/chunks/y", &stalled{n: 100})
		r.ContentLength = datastore.MaxBlobSize
		if _, err := readBody(httptest.NewRecorder(), r, datastore.MaxBlobSize+1); err == nil {
			t.Fatal("a short body was accepted")
		}
	}
	runtime.ReadMemStats(&after)
	per := (after.TotalAlloc - before.TotalAlloc) / streams
	t.Logf("allocated per stalled request: %d bytes", per)
	if per > 4<<20 {
		t.Fatalf("100 bytes received, %d bytes allocated per request", per)
	}
	// control: a complete body of the announced length is read exactly
	want := bytes.Repeat([]byte{7}, 5<<20+3)
	r := httptest.NewRequest("PUT", "/x", bytes.NewReader(want))
	r.ContentLength = int64(len(want))
	got, err := readBody(httptest.NewRecorder(), r, datastore.MaxBlobSize+1)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("complete body: %d bytes, %v", len(got), err)
	}
	// and a body longer than announced is refused (the server enforces Content-Length; here the
	// reader would just stop at the announced length)
}
