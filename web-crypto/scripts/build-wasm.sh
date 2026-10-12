#!/bin/sh
# Builds wasm/ibwasm.wasm from ../cmd/ibwasm and copies the matching wasm_exec.js from the Go
# toolchain that built it (the two must come from the same Go release).
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
cd "$here/.."
mkdir -p "$here/wasm"
GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$here/wasm/ibwasm.wasm" ./cmd/ibwasm
rm -f "$here/wasm/wasm_exec.js" "$here/wasm/LICENSE.golang"
cat "$(go env GOROOT)/lib/wasm/wasm_exec.js" > "$here/wasm/wasm_exec.js"
cat "$(go env GOROOT)/LICENSE" > "$here/wasm/LICENSE.golang"
go version > "$here/wasm/GO_VERSION"
raw=$(wc -c < "$here/wasm/ibwasm.wasm")
gz=$(gzip -9c "$here/wasm/ibwasm.wasm" | wc -c)
echo "ibwasm.wasm: $raw bytes raw, $gz bytes gzip -9"
if [ "$raw" -gt 6291456 ]; then
	echo "ibwasm.wasm exceeds the 6 MiB budget" >&2
	exit 1
fi
