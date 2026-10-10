//go:build !js

package fscat

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Payload gives access to the bytes of the payload stream.
type Payload interface {
	// Open returns a reader for payload bytes [off, off+size).
	Open(off, size uint64) (io.Reader, error)
}

// FromReaderAt adapts a random-access reader (the chunk store) to Payload.
func FromReaderAt(r io.ReaderAt) Payload { return readerAt{r} }

type readerAt struct{ r io.ReaderAt }

func (p readerAt) Open(off, size uint64) (io.Reader, error) {
	return io.NewSectionReader(p.r, int64(off), int64(size)), nil
}

// Sequential adapts a forward-only stream that starts at payload offset 0. Entries must be
// opened in increasing offset order and read completely; gaps (files that are not extracted)
// are skipped by reading and discarding.
func Sequential(r io.Reader) Payload { return &seq{r: r} }

type seq struct {
	r   io.Reader
	pos uint64
	cur *io.LimitedReader
}

func (s *seq) Open(off, size uint64) (io.Reader, error) {
	if s.cur != nil && s.cur.N > 0 { // finish the previous entry (the counting reader advances pos)
		if _, err := io.Copy(io.Discard, s.cur); err != nil {
			return nil, err
		}
	}
	if off < s.pos {
		return nil, fmt.Errorf("payload stream is forward-only (at %d, asked for %d)", s.pos, off)
	}
	if off > s.pos {
		n, err := io.CopyN(io.Discard, s.r, int64(off-s.pos))
		s.pos += uint64(n)
		if err != nil {
			return nil, fmt.Errorf("payload ends early: %w", err)
		}
	}
	s.cur = &io.LimitedReader{R: &countReader{r: s.r, pos: &s.pos}, N: int64(size)}
	return s.cur, nil
}

type countReader struct {
	r   io.Reader
	pos *uint64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.pos += uint64(n)
	return n, err
}

// ExtractOptions control extraction.
type ExtractOptions struct {
	// NoChown skips changing ownership and tolerates xattrs/devices that need privileges.
	NoChown bool
	// Paths restricts extraction to these paths and everything below them (empty = the whole tree).
	// Parent directories are created with their catalogued metadata.
	Paths []string
	// SkipVerify turns off the SHA-256 check of extracted content (the default checks every file).
	SkipVerify bool
}

func safePath(root, name string) (string, error) {
	if name == "" {
		return root, nil
	}
	for _, c := range strings.Split(name, "/") {
		if c == "" || c == "." || c == ".." {
			return "", fmt.Errorf("unsafe path %q", name)
		}
	}
	cur := root
	parts := strings.Split(name, "/")
	for i, c := range parts {
		cur = filepath.Join(cur, c)
		if i == len(parts)-1 {
			break
		}
		fi, err := os.Lstat(cur)
		if err != nil {
			return "", fmt.Errorf("parent of %q does not exist: %w", name, err)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("parent %q of %q is not a directory (symlink traversal refused)", cur, name)
		}
	}
	return cur, nil
}

type item struct {
	rel   string
	e     *Entry
	whole bool // false: an ancestor directory created only to hold selected entries
}

// Extract writes the selected part of the tree below dest (which must exist).
func (t *Tree) Extract(dest string, pay Payload, o ExtractOptions) error {
	selected := map[string]bool{}
	for _, p := range o.Paths {
		c, err := CleanPath(p)
		if err != nil {
			return err
		}
		if _, err := t.Lookup(c); err != nil {
			return err
		}
		selected[c] = true
	}
	inSelection := func(rel string) bool {
		if len(selected) == 0 {
			return true
		}
		for s := range selected {
			if s == "" || rel == s || strings.HasPrefix(rel, s+"/") {
				return true
			}
		}
		return false
	}
	isAncestor := func(rel string) bool {
		for s := range selected {
			if rel == "" || strings.HasPrefix(s, rel+"/") {
				return true
			}
		}
		return false
	}
	var items []item
	extracted := map[string]bool{}
	t.Walk(func(rel string, e *Entry) bool {
		switch {
		case inSelection(rel):
			items = append(items, item{rel, e, true})
			extracted[rel] = true
			return true
		case e.Kind == KDir && isAncestor(rel):
			items = append(items, item{rel, e, false})
			return true
		}
		return false
	})
	var dirs []item
	for _, it := range items {
		if rel := it.rel; rel == "" {
			dirs = append(dirs, it) // the destination itself: metadata applied last
			continue
		}
		p, err := safePath(dest, it.rel)
		if err != nil {
			return err
		}
		e := it.e
		switch e.Kind {
		case KDir:
			if err := os.Mkdir(p, 0o700); err != nil {
				if !errors.Is(err, fs.ErrExist) {
					return err
				}
				if fi, lerr := os.Lstat(p); lerr != nil || !fi.IsDir() {
					return fmt.Errorf("%s exists and is not a directory (refusing to follow it)", it.rel)
				}
			}
			dirs = append(dirs, it)
		case KFile:
			if err := extractFile(p, it.rel, e, pay, o); err != nil {
				return err
			}
			if err := applyMeta(p, e, false, o); err != nil {
				return err
			}
		case KSymlink:
			if err := os.Symlink(e.Link, p); err != nil {
				return err
			}
			if err := applyMeta(p, e, true, o); err != nil {
				return err
			}
		case KHardlink:
			if extracted[e.Link] {
				target, err := safePath(dest, e.Link)
				if err != nil {
					return err
				}
				if err := os.Link(target, p); err != nil {
					return err
				}
				break
			}
			// the first link was not selected: materialise this one as an ordinary file
			first, err := t.Lookup(e.Link)
			if err != nil || first.Kind != KFile {
				return fmt.Errorf("hard link %s points at %q which is not in the catalog", it.rel, e.Link)
			}
			cp := *first
			cp.Name = e.Name
			if err := extractFile(p, it.rel, &cp, pay, o); err != nil {
				return err
			}
			if err := applyMeta(p, &cp, false, o); err != nil {
				return err
			}
		case KChar, KBlock, KFifo:
			mode := e.Mode
			switch e.Kind {
			case KChar:
				mode |= syscall.S_IFCHR
			case KBlock:
				mode |= syscall.S_IFBLK
			default:
				mode |= syscall.S_IFIFO
			}
			if err := unix.Mknod(p, mode, int(unix.Mkdev(e.Major, e.Minor))); err != nil {
				if o.NoChown && errors.Is(err, unix.EPERM) {
					break // not root: devices and fifos cannot be created; the rest is restored
				}
				return fmt.Errorf("mknod %s: %w", it.rel, err)
			}
			if err := applyMeta(p, e, false, o); err != nil {
				return err
			}
		}
	}
	// directories last, children first, so their mtimes survive the writes above
	for i := len(dirs) - 1; i >= 0; i-- {
		p, err := safePath(dest, dirs[i].rel)
		if err != nil {
			return err
		}
		if dirs[i].whole || dirs[i].rel != "" {
			if err := applyMeta(p, dirs[i].e, false, o); err != nil {
				return err
			}
		}
	}
	return nil
}

func extractFile(p, rel string, e *Entry, pay Payload, o ExtractOptions) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if e.Size > 0 {
		r, err := pay.Open(e.PayOff, e.Size)
		if err != nil {
			f.Close()
			return fmt.Errorf("%s: %w", rel, err)
		}
		h := sha256.New()
		var w io.Writer = f
		if !o.SkipVerify {
			w = io.MultiWriter(f, h)
		}
		n, err := io.Copy(w, r)
		if err != nil {
			f.Close()
			return fmt.Errorf("%s: %w", rel, err)
		}
		if uint64(n) != e.Size {
			f.Close()
			return fmt.Errorf("%s: payload delivered %d bytes, the catalog says %d", rel, n, e.Size)
		}
		if !o.SkipVerify {
			var got [32]byte
			copy(got[:], h.Sum(nil))
			if got != e.Sum {
				f.Close()
				return fmt.Errorf("%s: content does not match its recorded SHA-256 (the repository returned wrong data)", rel)
			}
		}
	}
	return f.Close()
}

func applyMeta(p string, e *Entry, link bool, o ExtractOptions) error {
	if fi, err := os.Lstat(p); err != nil || (!link && fi.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("%s is a symlink or vanished: refusing to apply metadata through it", p)
	}
	if !o.NoChown {
		if err := os.Lchown(p, int(e.UID), int(e.GID)); err != nil {
			return fmt.Errorf("chown %s: %w", p, err)
		}
	}
	for _, x := range e.Xattrs {
		if err := unix.Lsetxattr(p, x.Name, x.Value, 0); err != nil {
			if errors.Is(err, unix.ENOTSUP) || (errors.Is(err, unix.EPERM) && o.NoChown) {
				continue
			}
			return fmt.Errorf("setxattr %s on %s: %w", x.Name, p, err)
		}
	}
	ts := []unix.Timespec{unix.NsecToTimespec(e.MtimeNs), unix.NsecToTimespec(e.MtimeNs)}
	if link {
		return unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, unix.AT_SYMLINK_NOFOLLOW)
	}
	if err := os.Chmod(p, fileMode(e.Mode)); err != nil {
		return err
	}
	return unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, 0)
}

func fileMode(m uint32) os.FileMode {
	mode := os.FileMode(m & 0o777)
	if m&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if m&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if m&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	return mode
}
