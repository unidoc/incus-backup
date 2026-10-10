package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, p string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
	return w
}

func TestSPAFallbackAndAssets(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/", "/hosts", "/hosts/web01.example.com", "/snapshots/abc/2026-10-08T03:00:00.123Z", "/profile"} {
		w := get(t, h, p)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "<html") {
			t.Fatalf("%s: want the app (200 html), got %d", p, w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s: Cache-Control %q, want no-cache", p, cc)
		}
	}
	for _, p := range []string{"/assets/missing-abc123.js", "/missing.css", "/favicon-old.ico", "/assets/x"} {
		if w := get(t, h, p); w.Code != 404 {
			t.Fatalf("%s: want 404 for a missing asset, got %d", p, w.Code)
		}
	}
}
