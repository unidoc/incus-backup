// Package webui embeds the compiled management UI (Vue 3 + Vite, built from /web into dist/) and serves it.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler serves the single-page application: real files as they are (hashed assets cached for a year,
// everything else revalidated), any other path that is not an API call falls back to index.html so
// client-side routes survive a reload.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	return handlerFS(sub)
}

func handlerFS(sub fs.FS) http.Handler {
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if st, err := fs.Stat(sub, p); err != nil || st.IsDir() {
			if isAsset(p) { // a missing asset is a 404, not the app
				http.NotFound(w, r)
				return
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, r2)
			return
		}
		if strings.HasSuffix(p, ".wasm") { // WebAssembly.compileStreaming needs exactly this type
			w.Header().Set("Content-Type", "application/wasm")
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// staticExt are the file types the build emits. Only a missing file of one of these types (or anything
// under assets/) is a 404: client routes may end in dotted names (/hosts/web01.example.com) and must
// get the application.
var staticExt = map[string]bool{".js": true, ".mjs": true, ".css": true, ".map": true, ".png": true, ".svg": true, ".ico": true,
	".woff": true, ".woff2": true, ".wasm": true, ".json": true, ".txt": true, ".webmanifest": true, ".jpg": true, ".webp": true}

func isAsset(p string) bool {
	return strings.HasPrefix(p, "assets/") || staticExt[strings.ToLower(path.Ext(p))]
}
