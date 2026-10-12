package datastore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FSBackend stores objects as files below a root directory. Writes are
// crash-safe: temp file in <root>/tmp, fsync, rename (or link, for ifAbsent), fsync
// of the directory.
type FSBackend struct {
	root string
}

// NewFSBackend returns a backend rooted at an existing directory.
func NewFSBackend(root string) (*FSBackend, error) {
	for _, d := range []string{root, filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	return &FSBackend{root: root}, nil
}

// Root returns the root directory.
func (b *FSBackend) Root() string { return b.root }

// Describe implements Backend.
func (b *FSBackend) Describe() string { return "filesystem " + b.root }

func (b *FSBackend) path(key string) (string, error) {
	if err := validKey(key); err != nil {
		return "", err
	}
	return filepath.Join(b.root, filepath.FromSlash(key)), nil
}

// SkipFsync turns off every fsync of the filesystem backend. Only the dev demo (cmd/ibdemo, a throw-away
// repository on a slow overlay filesystem) sets it; production code never does.
var SkipFsync bool

func syncDir(dir string) error {
	if SkipFsync {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func randName() string {
	var x [8]byte
	_, _ = rand.Read(x[:])
	return hex.EncodeToString(x[:])
}

// mkdirAllSync creates dir and fsyncs the parent of every directory it created.
func mkdirAllSync(dir string) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, d := range missing {
		if err := syncDir(filepath.Dir(d)); err != nil {
			return err
		}
	}
	return nil
}

// Put implements Backend.
func (b *FSBackend) Put(key string, data []byte, ifAbsent bool) error {
	dst, err := b.path(key)
	if err != nil {
		return err
	}
	if err := mkdirAllSync(filepath.Dir(dst)); err != nil {
		return err
	}
	tmp := filepath.Join(b.root, "tmp", ".w-"+randName())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := fsyncFile(f); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if ifAbsent {
		// link fails with EEXIST if dst exists: an atomic create-if-absent
		if err := os.Link(tmp, dst); err != nil {
			cleanup()
			if errors.Is(err, fs.ErrExist) {
				return ErrExists
			}
			return err
		}
		cleanup()
	} else if err := os.Rename(tmp, dst); err != nil {
		cleanup()
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func fsyncFile(f *os.File) error {
	if SkipFsync {
		return nil
	}
	return f.Sync()
}

// Get implements Backend.
func (b *FSBackend) Get(key string) ([]byte, error) {
	p, err := b.path(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}

// Stat implements Backend.
func (b *FSBackend) Stat(key string) (Info, error) {
	p, err := b.path(key)
	if err != nil {
		return Info{}, err
	}
	fi, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, ErrNotFound
	}
	if err != nil {
		return Info{}, err
	}
	if fi.IsDir() {
		return Info{}, ErrNotFound
	}
	return Info{Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// List implements Backend.
func (b *FSBackend) List(prefix string, fn func(string, Info) error) error {
	dir := strings.TrimSuffix(prefix, "/")
	if dir != "" {
		if err := validKey(dir); err != nil {
			return err
		}
	}
	start := filepath.Join(b.root, filepath.FromSlash(dir))
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(b.root, p)
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, "tmp/") || !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		return fn(key, Info{Size: info.Size(), ModTime: info.ModTime()})
	})
	return err
}

// ListPrefixes implements Backend.
func (b *FSBackend) ListPrefixes(prefix string) ([]string, error) {
	dir := strings.TrimSuffix(prefix, "/")
	if dir != "" {
		if err := validKey(dir); err != nil {
			return nil, err
		}
	}
	es, err := os.ReadDir(filepath.Join(b.root, filepath.FromSlash(dir)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Delete implements Backend.
func (b *FSBackend) Delete(key string) error {
	p, err := b.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// CleanTmp removes crash leftovers in tmp/ older than olderThan.
func (b *FSBackend) CleanTmp(olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	es, err := os.ReadDir(filepath.Join(b.root, "tmp"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range es {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.RemoveAll(filepath.Join(b.root, "tmp", e.Name())) == nil {
			n++
		}
	}
	return n, nil
}

var _ = fmt.Sprintf
