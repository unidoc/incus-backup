// incus-backup is the client of incus-backup: it discovers what an Incus host
// runs, captures it consistently, encrypts it locally and sends it to one or
// more incus-backup servers. It also restores, verifies and diagnoses.
package main

import (
	"fmt"
	"os"
	_ "time/tzdata" // IANA zones embedded: policy time zones must work on a minimal Alpine
)

var version = "dev"

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
