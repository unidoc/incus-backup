package webui

import (
	"strings"
	"testing"
	"testing/fstest"
)

// The browser decryption library ships under assets/crypto-<hash>/: the module must be served as
// application/wasm (WebAssembly.compileStreaming refuses anything else), its ES modules as JavaScript
// (module workers check the type strictly), both cacheable for good (the path carries the hash).
func TestWebCryptoFilesTypesAndCaching(t *testing.T) {
	wasm := []byte("\x00asm\x01\x00\x00\x00")
	h := handlerFS(fstest.MapFS{
		"index.html": {Data: []byte("<html></html>")},
		"assets/crypto-0123456789ab/dist/index.js":     {Data: []byte("export {}")},
		"assets/crypto-0123456789ab/dist/worker.js":    {Data: []byte("export {}")},
		"assets/crypto-0123456789ab/wasm/ibwasm.wasm":  {Data: wasm},
		"assets/crypto-0123456789ab/wasm/wasm_exec.js": {Data: []byte("globalThis.Go = class {}")},
	})
	for p, want := range map[string]string{
		"/assets/crypto-0123456789ab/wasm/ibwasm.wasm":  "application/wasm",
		"/assets/crypto-0123456789ab/dist/index.js":     "text/javascript",
		"/assets/crypto-0123456789ab/dist/worker.js":    "text/javascript",
		"/assets/crypto-0123456789ab/wasm/wasm_exec.js": "text/javascript",
	} {
		w := get(t, h, p)
		if w.Code != 200 {
			t.Fatalf("%s: status %d", p, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, want) {
			t.Fatalf("%s: Content-Type %q, want %s", p, ct, want)
		}
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Fatalf("%s: Cache-Control %q, want immutable", p, cc)
		}
	}
	if w := get(t, h, "/assets/crypto-0123456789ab/wasm/ibwasm.wasm"); w.Body.String() != string(wasm) {
		t.Fatalf("module bytes changed in transit")
	}
	// a stale hash after an upgrade is a 404, never the HTML application under a .wasm name
	if w := get(t, h, "/assets/crypto-ffffffffffff/wasm/ibwasm.wasm"); w.Code != 404 {
		t.Fatalf("missing module: want 404, got %d", w.Code)
	}
}
