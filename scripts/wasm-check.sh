#!/bin/sh
# The browser module must keep compiling for js/wasm: nothing it imports may need unix-only packages
# (golang.org/x/sys/unix, syscall.Stat_t, ...). CodeQL, the web-crypto tests and the Web UI build all run this.
set -eu
cd "$(dirname "$0")/.."
GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -o /dev/null ./cmd/ibwasm
echo "js/wasm build of cmd/ibwasm: ok"
