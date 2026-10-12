package fscat

// The scanner exactly as it was before the parallel walk (kept verbatim, renamed), as the reference
// the parallel scanner must reproduce byte for byte.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

func origListXattrs(path string) ([]Xattr, error) {
	sz, err := unix.Llistxattr(path, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOSYS) {
			return nil, nil
		}
		return nil, err
	}
	if sz == 0 {
		return nil, nil
	}
	buf := make([]byte, sz)
	if sz, err = unix.Llistxattr(path, buf); err != nil {
		return nil, err
	}
	var out []Xattr
	start := 0
	for i := 0; i < sz; i++ {
		if buf[i] != 0 {
			continue
		}
		name := string(buf[start:i])
		start = i + 1
		if name == "" {
			continue
		}
		vsz, err := unix.Lgetxattr(path, name, nil)
		if err != nil {
			if errors.Is(err, unix.ENODATA) {
				continue
			}
			return nil, err
		}
		val := make([]byte, vsz)
		if vsz > 0 {
			if vsz, err = unix.Lgetxattr(path, name, val); err != nil {
				return nil, err
			}
		}
		out = append(out, Xattr{Name: name, Value: val[:vsz]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type origScanner struct {
	o    ScanOptions
	root string
	res  *Result
	seen map[[2]uint64]string // (dev, ino) -> first path, for hard links
	off  uint64
	pack *Run
	// prevLimit is the end of the previous payload: a catalog that points outside its own
	// payload is not trusted for reuse.
	prevLimit uint64
}

// Scan walks root (a read-only snapshot mount) and builds the catalog and the run layout. It
// reads no file content.
func origScan(root string, o ScanOptions) (*Result, error) {
	o.norm()
	s := &origScanner{o: o, root: root, res: &Result{}, seen: map[[2]uint64]string{}}
	s.prevLimit = prevPayloadLimit(o.Prev)
	var prevRoot *Entry
	if o.Prev != nil && !o.NoReuse {
		prevRoot = o.Prev.Root
	}
	rootEntry, err := s.entryFor(root, "", "", prevRoot)
	if err != nil {
		return nil, err
	}
	if rootEntry.Kind != KDir {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	if err := s.dir(root, "", rootEntry, prevRoot); err != nil {
		return nil, err
	}
	s.closePack()
	s.res.Tree = &Tree{Root: rootEntry}
	n := 0
	s.res.Tree.Walk(func(string, *Entry) bool { n++; return true })
	s.res.Tree.Count = n
	s.res.PayloadSize = s.off
	return s.res, nil
}

func (s *origScanner) entryFor(path, name, rel string, prev *Entry) (*Entry, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	e := &Entry{
		Name: name, Mode: st.Mode & 0o7777, UID: st.Uid, GID: st.Gid,
		MtimeNs: unix.TimespecToNsec(st.Mtim), CtimeNs: unix.TimespecToNsec(st.Ctim), Inode: st.Ino,
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		e.Kind = KDir
	case unix.S_IFLNK:
		e.Kind = KSymlink
		l, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		e.Link = l
	case unix.S_IFIFO:
		e.Kind = KFifo
	case unix.S_IFCHR:
		e.Kind, e.Major, e.Minor = KChar, unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev))
	case unix.S_IFBLK:
		e.Kind, e.Major, e.Minor = KBlock, unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev))
	case unix.S_IFSOCK:
		return nil, errSocket
	case unix.S_IFREG:
		e.Kind, e.Size = KFile, uint64(st.Size)
		if st.Nlink > 1 {
			k := [2]uint64{uint64(st.Dev), st.Ino}
			if first, ok := s.seen[k]; ok {
				e.Kind, e.Size, e.Link = KHardlink, 0, first
				break
			}
			s.seen[k] = rel
		}
	default:
		return nil, fmt.Errorf("%s: unsupported file type %#o", path, st.Mode&unix.S_IFMT)
	}
	x, err := origListXattrs(path)
	if err != nil {
		return nil, fmt.Errorf("reading xattrs of %s: %w", path, err)
	}
	e.Xattrs = x
	return e, nil
}

func (s *origScanner) dir(path, rel string, e *Entry, prev *Entry) error {
	ents, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	names := make([]string, 0, len(ents))
	for _, d := range ents {
		names = append(names, d.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		cp := filepath.Join(path, name)
		crel := name
		if rel != "" {
			crel = rel + "/" + name
		}
		var pc *Entry
		if prev != nil && prev.Kind == KDir {
			pc = prev.child(name)
		}
		c, err := s.entryFor(cp, name, crel, pc)
		if err == errSocket {
			continue // sockets cannot be archived or restored
		}
		if err != nil {
			return err
		}
		e.Children = append(e.Children, c)
		switch c.Kind {
		case KDir:
			s.res.Dirs++
			if err := s.dir(cp, crel, c, pc); err != nil {
				return err
			}
		case KFile:
			s.res.Files++
			if c.Size > 0 {
				s.addFile(RunFile{Rel: crel, Path: cp, Entry: c}, pc)
			}
		default:
			s.res.Other++
		}
	}
	return nil
}

// addFile places a file with content into the run layout.
func (s *origScanner) addFile(f RunFile, prev *Entry) {
	size := int64(f.Entry.Size)
	reusable := prev != nil && !s.o.NoReuse && prev.Size > 0 && same(prev, f.Entry, s.o.TrustCtimeInode) && prev.PayOff+prev.Size <= s.prevLimit
	if reusable {
		f.Entry.Sum = prev.Sum
	}
	if size >= s.o.SmallFile {
		s.closePack()
		r := Run{Start: s.off, End: s.off + f.Entry.Size, Large: true, Files: []RunFile{f}}
		f.Entry.PayOff = s.off
		if reusable {
			r.Reusable, r.OldStart, r.OldEnd = true, prev.PayOff, prev.PayOff+prev.Size
		}
		s.off = r.End
		s.res.Runs = append(s.res.Runs, r)
		return
	}
	if s.pack == nil {
		s.pack = &Run{Start: s.off, End: s.off, Reusable: true}
	}
	p := s.pack
	f.Entry.PayOff = s.off
	if p.Reusable {
		switch {
		case !reusable:
			p.Reusable = false
		case len(p.Files) == 0:
			p.OldStart, p.OldEnd = prev.PayOff, prev.PayOff+prev.Size
		case prev.PayOff == p.OldEnd:
			p.OldEnd = prev.PayOff + prev.Size
		default:
			p.Reusable = false
		}
	}
	p.Files = append(p.Files, f)
	s.off += f.Entry.Size
	p.End = s.off
	cum := int64(p.End - p.Start)
	if cum >= s.o.PackMax || (cum >= s.o.PackMin && nameHash(f.Rel)%s.o.PackDiv == 0) {
		s.closePack()
	}
}

func (s *origScanner) closePack() {
	if s.pack == nil {
		return
	}
	s.res.Runs = append(s.res.Runs, *s.pack)
	s.pack = nil
}
