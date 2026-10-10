package datastore

import (
	"crypto/rand"
	"fmt"
	"os"
)

func randRead(b []byte) (int, error) { return rand.Read(b) }

func EnsureEmptyDir(root string) error {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty", root)
	}
	return nil
}
