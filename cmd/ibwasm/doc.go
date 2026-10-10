// Command ibwasm is the WebAssembly module of the browser library web-crypto/. It is built only
// for GOOS=js GOARCH=wasm:
//
//	GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o web-crypto/wasm/ibwasm.wasm ./cmd/ibwasm
//
// The logic lives in internal/webcore (tested on the host); main.go only binds it to JavaScript.
package main
