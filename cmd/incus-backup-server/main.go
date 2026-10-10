// incus-backup-server is the repository server of incus-backup: a zero-knowledge
// store for encrypted chunks with a PostgreSQL control plane.
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
