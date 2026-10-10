// Package archive turns a directory tree into a deterministic PAX tar stream and
// back. The stream is what the filesystem engine chunks: it carries everything a
// container root needs to be restored exactly (numeric ids as stored in the
// snapshot, modes, nanosecond mtimes, symlinks, hard links, device nodes, fifos
// and extended attributes, which includes POSIX ACLs and file capabilities).
//
// Determinism matters: identical trees produce identical bytes (entries are
// sorted, atime/ctime and user/group names are not recorded), so content-defined
// chunking deduplicates unchanged regions across backups.
package archive

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const xattrPrefix = "SCHILY.xattr."

// Stats counts what was archived.
type Stats struct {
	Files, Dirs, Symlinks, Links, Special int
	Bytes                                 int64
}

func xattrs(path string) (map[string]string, error) {
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
	sz, err = unix.Llistxattr(path, buf)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:sz]), "\x00"), "\x00") {
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
		out[name] = string(val[:vsz])
	}
	return out, nil
}

// Write archives the tree at root into w.
func Write(w io.Writer, root string) (*Stats, error) {
	tw := tar.NewWriter(w)
	st := &Stats{}
	type inode struct{ dev, ino uint64 }
	seen := map[inode]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		var sys unix.Stat_t
		if err := unix.Lstat(p, &sys); err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:    name,
			Mode:    int64(fi.Mode().Perm()),
			Uid:     int(sys.Uid),
			Gid:     int(sys.Gid),
			ModTime: time.Unix(sys.Mtim.Sec, sys.Mtim.Nsec).UTC(),
			Format:  tar.FormatPAX,
		}
		if fi.Mode()&os.ModeSetuid != 0 {
			hdr.Mode |= 0o4000
		}
		if fi.Mode()&os.ModeSetgid != 0 {
			hdr.Mode |= 0o2000
		}
		if fi.Mode()&os.ModeSticky != 0 {
			hdr.Mode |= 0o1000
		}
		if x, err := xattrs(p); err != nil {
			return fmt.Errorf("reading xattrs of %s: %w", name, err)
		} else if len(x) > 0 {
			hdr.PAXRecords = map[string]string{}
			for k, v := range x {
				hdr.PAXRecords[xattrPrefix+k] = v
			}
		}
		switch {
		case fi.IsDir():
			if name == "." {
				hdr.Name = "./"
			} else {
				hdr.Name = name + "/"
			}
			hdr.Typeflag = tar.TypeDir
			st.Dirs++
		case fi.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, l
			st.Symlinks++
		case fi.Mode()&os.ModeSocket != 0:
			return nil // sockets cannot be archived (nor restored)
		case fi.Mode()&os.ModeNamedPipe != 0:
			hdr.Typeflag = tar.TypeFifo
			st.Special++
		case fi.Mode()&os.ModeCharDevice != 0:
			hdr.Typeflag, hdr.Devmajor, hdr.Devminor = tar.TypeChar, int64(unix.Major(sys.Rdev)), int64(unix.Minor(sys.Rdev))
			st.Special++
		case fi.Mode()&os.ModeDevice != 0:
			hdr.Typeflag, hdr.Devmajor, hdr.Devminor = tar.TypeBlock, int64(unix.Major(sys.Rdev)), int64(unix.Minor(sys.Rdev))
			st.Special++
		case fi.Mode().IsRegular():
			if sys.Nlink > 1 {
				k := inode{uint64(sys.Dev), sys.Ino}
				if first, ok := seen[k]; ok {
					hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeLink, first, 0
					st.Links++
					break
				}
				seen[k] = name
			}
			hdr.Typeflag, hdr.Size = tar.TypeReg, fi.Size()
			st.Files++
		default:
			return fmt.Errorf("%s: unsupported file type %v", name, fi.Mode())
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			n, err := io.CopyN(tw, f, hdr.Size)
			f.Close()
			if err != nil {
				return fmt.Errorf("%s: %w (file changed while archiving?)", name, err)
			}
			st.Bytes += n
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	return st, tw.Close()
}

// ExtractOptions control restore.
type ExtractOptions struct {
	// NoChown skips changing ownership (when not running as root).
	NoChown bool
	// Untrusted extracts a tar produced by something hostile (a guest filesystem read by the
	// recovery VM): only directories, regular files and symbolic links are created; devices, fifos
	// and hard links are skipped; set-id and sticky bits and all extended attributes are dropped;
	// ownership is never changed; MaxBytes bounds the total size.
	Untrusted bool
	MaxBytes  int64
	// Report, if set, receives what was skipped.
	Report *ExtractReport
}

// ExtractReport counts what an untrusted extraction did.
type ExtractReport struct {
	Files, Dirs, Symlinks int
	Bytes                 int64
	Skipped               []string
}

func hasDotDot(name string) bool {
	for _, c := range strings.Split(name, "/") {
		if c == ".." {
			return true
		}
	}
	return false
}

// safePath resolves name below root without ever following a symlink in any
// directory component (a malicious or damaged archive must not write outside).
func safePath(root, name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || hasDotDot(name) {
		return "", fmt.Errorf("unsafe path %q in archive", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." {
		return root, nil
	}
	cur := root
	parts := strings.Split(clean, string(filepath.Separator))
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

// ensureParents creates the missing parent directories of name below root (never following a
// symlink: an existing non-directory component is an error).
func ensureParents(root, name string) error {
	if name == "" || strings.HasPrefix(name, "/") || hasDotDot(name) {
		return fmt.Errorf("unsafe path %q in archive", name)
	}
	parts := strings.Split(filepath.Clean(filepath.FromSlash(name)), string(filepath.Separator))
	cur := root
	for _, c := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(cur, 0o755); err != nil {
				return err
			}
		case err != nil:
			return err
		case !fi.IsDir():
			return fmt.Errorf("%q is not a directory (symlink traversal refused)", cur)
		}
	}
	return nil
}

// Extract restores a stream into dest (which must exist).
func Extract(r io.Reader, dest string, o ExtractOptions) error {
	return ExtractTar(tar.NewReader(r), dest, o)
}

// ExtractTar restores an already opened tar reader into dest (which must exist).
func ExtractTar(tr *tar.Reader, dest string, o ExtractOptions) error {
	type dirFix struct {
		path string
		hdr  *tar.Header
	}
	var dirs []dirFix
	if o.Untrusted {
		o.NoChown = true
	}
	rep := o.Report
	if rep == nil {
		rep = &ExtractReport{}
	}
	apply := func(path string, hdr *tar.Header, link bool) error {
		if fi, lerr := os.Lstat(path); lerr != nil || (!link && fi.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("%s is a symlink or vanished: refusing to apply metadata through it", path)
		}
		if o.Untrusted {
			hdr.Mode &= 0o777 // no setuid, setgid or sticky from a hostile source
			hdr.PAXRecords = nil
		}
		if !o.NoChown {
			if err := os.Lchown(path, hdr.Uid, hdr.Gid); err != nil {
				return fmt.Errorf("chown %s: %w", path, err)
			}
		}
		for k, v := range hdr.PAXRecords {
			if !strings.HasPrefix(k, xattrPrefix) {
				continue
			}
			if err := unix.Lsetxattr(path, strings.TrimPrefix(k, xattrPrefix), []byte(v), 0); err != nil {
				if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) && o.NoChown {
					continue
				}
				return fmt.Errorf("setxattr %s on %s: %w", k, path, err)
			}
		}
		if link {
			ts := []unix.Timespec{unix.NsecToTimespec(hdr.ModTime.UnixNano()), unix.NsecToTimespec(hdr.ModTime.UnixNano())}
			return unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW)
		}
		mode := os.FileMode(hdr.Mode & 0o777)
		if hdr.Mode&0o4000 != 0 {
			mode |= os.ModeSetuid
		}
		if hdr.Mode&0o2000 != 0 {
			mode |= os.ModeSetgid
		}
		if hdr.Mode&0o1000 != 0 {
			mode |= os.ModeSticky
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
		return os.Chtimes(path, hdr.ModTime, hdr.ModTime)
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}
		if o.Untrusted {
			if n := strings.Trim(hdr.Name, "/"); n == "" || n == "." {
				return fmt.Errorf("hostile archive entry %q (it would change the destination itself)", hdr.Name)
			}
			switch hdr.Typeflag {
			case tar.TypeDir, tar.TypeReg, tar.TypeSymlink:
			default:
				rep.Skipped = append(rep.Skipped, hdr.Name)
				continue
			}
		}
		if o.Untrusted {
			// `tar -cf - etc/nginx` carries no entry for etc/: create missing parents as plain directories
			if err := ensureParents(dest, strings.TrimSuffix(hdr.Name, "/")); err != nil {
				return err
			}
		}
		p, err := safePath(dest, strings.TrimSuffix(hdr.Name, "/"))
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(p, 0o700); err != nil {
				if !errors.Is(err, fs.ErrExist) {
					return err
				}
				// an existing entry must be a REAL directory: a symlink of that name (planted by a
				// hostile archive) would make the final chmod/chtimes act on whatever it points at
				if fi, lerr := os.Lstat(p); lerr != nil || !fi.IsDir() {
					return fmt.Errorf("%q exists and is not a directory (refusing to follow it)", hdr.Name)
				}
			}
			dirs = append(dirs, dirFix{p, hdr})
			rep.Dirs++
		case tar.TypeReg:
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			var src io.Reader = tr
			if o.Untrusted && o.MaxBytes > 0 {
				if rep.Bytes+hdr.Size > o.MaxBytes {
					f.Close()
					return fmt.Errorf("the recovered data exceeds the limit of %d bytes", o.MaxBytes)
				}
				src = io.LimitReader(tr, hdr.Size)
			}
			n, err := io.Copy(f, src)
			rep.Bytes += n
			rep.Files++
			if err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			if err := apply(p, hdr, false); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(hdr.Linkname, p); err != nil {
				return err
			}
			rep.Symlinks++
			if err := apply(p, hdr, true); err != nil {
				return err
			}
		case tar.TypeLink:
			target, err := safePath(dest, hdr.Linkname)
			if err != nil {
				return err
			}
			if err := os.Link(target, p); err != nil {
				return err
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			mode := uint32(hdr.Mode & 0o7777)
			switch hdr.Typeflag {
			case tar.TypeChar:
				mode |= syscall.S_IFCHR
			case tar.TypeBlock:
				mode |= syscall.S_IFBLK
			default:
				mode |= syscall.S_IFIFO
			}
			if err := unix.Mknod(p, mode, int(unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor)))); err != nil {
				return fmt.Errorf("mknod %s: %w", p, err)
			}
			if err := apply(p, hdr, false); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type %c for %s", hdr.Typeflag, hdr.Name)
		}
	}
	// directories last, children first, so mtimes survive the writes above
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := apply(dirs[i].path, dirs[i].hdr, false); err != nil {
			return err
		}
	}
	return nil
}
