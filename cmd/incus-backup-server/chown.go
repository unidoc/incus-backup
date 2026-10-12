package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// serviceUser is the account the OpenRC service runs as.
const serviceUser = "incus-backup"

// chownTree hands paths over to the service account when init runs as root, so
// the daemon (which does not run as root) can read its configuration and write
// its state and datastore. It does nothing when not root or when the account
// does not exist (a hand-managed install).
func chownTree(name string, paths ...string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return chownIDs(uid, gid, paths...)
}

func chownIDs(uid, gid int, paths ...string) error {
	for _, p := range paths {
		err := filepath.Walk(p, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			return os.Lchown(path, uid, gid)
		})
		if err != nil {
			return fmt.Errorf("handing %s to the service account: %w", p, err)
		}
	}
	return nil
}
