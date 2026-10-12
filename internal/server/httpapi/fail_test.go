package httpapi

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/unidoc/incus-backup/internal/datastore"
)

// A malformed id (datastore.ErrInvalid) is the caller's mistake: 400, never a retried 500.
func TestFailMapsInvalidTo400AndKeepsMaintenanceModesRetryable(t *testing.T) {
	s := &Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, c := range []struct {
		err  error
		code int
	}{
		{fmt.Errorf("reading index: %w", datastore.ErrInvalid), http.StatusBadRequest},
		{datastore.ErrReadOnly, http.StatusServiceUnavailable},
		{datastore.ErrOffline, http.StatusServiceUnavailable},
		{fmt.Errorf("disk on fire"), http.StatusInternalServerError},
	} {
		w := httptest.NewRecorder()
		s.fail(w, httptest.NewRequest("GET", "/v1/x", nil), c.err)
		if w.Code != c.code {
			t.Errorf("%v: status %d, want %d", c.err, w.Code, c.code)
		}
	}
}
