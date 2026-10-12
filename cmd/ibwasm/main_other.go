//go:build !(js && wasm)

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "ibwasm is a WebAssembly module: build it with GOOS=js GOARCH=wasm")
	os.Exit(2)
}
