package demo

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// uiHandler serves a Vite build from a directory on disk (re-read on every request, so `npm run build`
// shows up without restarting the demo). Unknown paths that do not look like assets fall back to
// index.html so client-side routes survive a reload; a missing asset is a 404. Same rules as the
// embedded internal/server/webui handler.
func uiHandler(dir string) http.Handler {
	root := os.DirFS(dir)
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if st, err := fs.Stat(root, p); err != nil || st.IsDir() {
			if strings.Contains(path.Base(p), ".") && !strings.HasSuffix(p, ".html") {
				http.NotFound(w, r)
				return
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, r2)
			return
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// DefaultUIDir returns the first Vite build directory found relative to the working directory
// (web/dist, ../web/dist, ../../web/dist), or "".
func DefaultUIDir() string {
	for _, d := range []string{"web/dist", "../web/dist", "../../web/dist"} {
		if st, err := os.Stat(path.Join(d, "index.html")); err == nil && !st.IsDir() {
			return d
		}
	}
	return ""
}
