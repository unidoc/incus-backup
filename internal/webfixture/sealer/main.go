// Command sealer seals stdin as a sealed delivery (format.NewSealWriter, IBS2): to an X25519 public
// key, authenticated with the repository key of -master for job -job and the request digest of
// -type/-params (jobspec.RequestDigest). The browser library's tests use it to prove that a key pair
// generated in the page receives real host output, and (with another -master) to play a malicious
// server that knows the public key but not the repository key.
//
//	go run ./internal/webfixture/sealer -pub <64 hex> -master <64 hex> -job <id> -type fs_extract -params '<json>' < plain > sealed
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/jobspec"
)

func main() {
	pub := flag.String("pub", "", "recipient X25519 public key, hex")
	master := flag.String("master", "", "repository master key, hex (32 bytes)")
	job := flag.String("job", "", "job id")
	typ := flag.String("type", "fs_extract", "job type")
	params := flag.String("params", "", "job parameters as the browser sent them (JSON)")
	flag.Parse()
	if err := run(*pub, *master, *job, *typ, *params); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(pubHex, masterHex, job, typ, params string) error {
	k, err := hex.DecodeString(pubHex)
	if err == nil && len(k) != 32 {
		err = fmt.Errorf("public key must be 32 bytes")
	}
	if err != nil {
		return err
	}
	m, err := hex.DecodeString(masterHex)
	if err != nil {
		return err
	}
	keys, err := format.DeriveKeys(m)
	if err != nil {
		return err
	}
	digest, err := jobspec.RequestDigest(typ, []byte(params))
	if err != nil {
		return err
	}
	w, err := format.NewSealWriter(os.Stdout, keys, k, format.SealBinding{JobID: job, Request: digest})
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, os.Stdin); err != nil {
		return err
	}
	return w.Close()
}
