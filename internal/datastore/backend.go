package datastore

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Backend is the storage the repository lives on. It has OBJECT-STORE semantics on
// purpose, so a native S3-compatible backend can implement it without a POSIX
// filesystem underneath: no rename, no directories, no appends.
//
// What the repository relies on, and nothing more:
//
//   - Put writes a whole object and is durable when it returns. With ifAbsent it
//     fails with ErrExists if the key exists (S3: `If-None-Match: *`). Without it
//     an existing object is replaced (objects are immutable by convention).
//   - A snapshot becomes visible by ONE Put of its commit marker, written last
//     (`groups/<group>/<snapshot>/commit.json`, ifAbsent). Everything before it is
//     invisible garbage if a crash happens. This replaces "rename a directory".
//   - There is NO touch/utimes: object age is only ever the time of the Put, so an
//     object store never needs COPY-onto-self (which would create needless requests and
//     object versions). Liveness of chunks a running session relies on is tracked by
//     the Store itself (GC pinning), not by rewriting objects.
//   - ModTime of a written object is "about now", with the backend clock.
//   - List and ListPrefixes are consistent enough for GC: an object written before the
//     listing started may be missing from it only if it is younger than the GC grace.
type Backend interface {
	Put(key string, data []byte, ifAbsent bool) error
	Get(key string) ([]byte, error)
	Stat(key string) (Info, error)
	// List calls fn for every object whose key starts with prefix.
	List(prefix string, fn func(key string, info Info) error) error
	// ListPrefixes returns the immediate "directory" names below prefix (which ends in "/").
	ListPrefixes(prefix string) ([]string, error)
	// Delete removes an object; a missing object is not an error.
	Delete(key string) error
	// Describe names the backend for logs ("filesystem /srv/x", "s3 bucket/prefix").
	Describe() string
}

// Info describes a stored object.
type Info struct {
	Size    int64
	ModTime time.Time
}

// validKey rejects anything that could escape a filesystem backend's root or
// confuse an object store.
func validKey(k string) error {
	if k == "" || strings.HasPrefix(k, "/") || strings.Contains(k, "//") || strings.ContainsRune(k, 0) {
		return fmt.Errorf("invalid object key %q", k)
	}
	for _, c := range strings.Split(k, "/") {
		if c == ".." || c == "." {
			return fmt.Errorf("invalid object key %q", k)
		}
	}
	return nil
}

var errBackendNotFound = errors.New("object not found")
