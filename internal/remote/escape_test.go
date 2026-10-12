package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/unidoc/incus-backup/internal/api"
)

// group, snapshot and chunk ids are escaped into the request path, never concatenated raw.
func TestPathSegmentsAreEscapedAndSyncWritesCarryATransferID(t *testing.T) {
	var paths, xfer []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		xfer = append(xfer, r.Header.Get(api.HeaderSyncTransfer))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	ctx := context.Background()
	_, _ = c.Index(ctx, "g/../x", "s?y#z")
	_, _ = c.Chunk(ctx, "a/b c")
	_ = c.SyncPutChunk(ctx, "1/2", []byte("x"))
	_ = c.SyncPutChunk(ctx, "3", []byte("x"))
	want := []string{
		"/v1/groups/g%2F..%2Fx/snapshots/s%3Fy%23z/index",
		"/v1/chunks/a%2Fb%20c",
		"/v1/sync/chunks/1%2F2",
		"/v1/sync/chunks/3",
	}
	if len(paths) != len(want) {
		t.Fatalf("paths %q", paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("request %d: path %q, want %q", i, paths[i], want[i])
		}
	}
	if xfer[0] != "" || xfer[1] != "" {
		t.Errorf("reads must not carry a transfer id: %q", xfer)
	}
	if xfer[2] == "" || xfer[2] != xfer[3] {
		t.Errorf("one client = one transfer id on its sync writes: %q", xfer)
	}
	other := &Client{Base: srv.URL, HTTP: srv.Client()}
	_ = other.SyncPutChunk(ctx, "4", []byte("x"))
	if xfer[4] == "" || xfer[4] == xfer[2] {
		t.Errorf("a second client must use its own transfer id: %q", xfer)
	}
}
