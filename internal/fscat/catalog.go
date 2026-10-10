// Package fscat is the file catalog of filesystem snapshots (format "fs2").
//
// A filesystem snapshot is two byte ranges in ONE dynamic chunk index:
//
//	[ payload: the bytes of every regular file, concatenated in walk order ]
//	[ catalog: this package's encoding of the metadata tree                ]
//
// The catalog records, for every entry, its metadata (mode, uid/gid, nanosecond mtime and
// ctime, inode, size, xattrs and therefore ACLs and file capabilities), for files the
// position of their bytes in the payload and a SHA-256 of their content. It is sealed by
// the chunk encryption like any other data, so the server never learns a file name.
//
// The catalog serves four purposes:
//
//  1. change detection: the next backup compares the live tree with the previous catalog
//     and reuses whole chunks of unchanged files without reading them;
//  2. browsing (ls, find, stat) without downloading any file data;
//  3. single file and directory extraction: only the chunks that hold the requested bytes
//     are fetched;
//  4. end-to-end verification of extracted content (the SHA-256 per file).
package fscat

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
)

// Kind of catalog entry.
type Kind byte

// Entry kinds. The zero value is the end-of-directory marker in the encoding.
const (
	KDir Kind = iota + 1
	KFile
	KSymlink
	KHardlink
	KChar
	KBlock
	KFifo
)

func (k Kind) String() string {
	switch k {
	case KDir:
		return "dir"
	case KFile:
		return "file"
	case KSymlink:
		return "symlink"
	case KHardlink:
		return "hardlink"
	case KChar:
		return "char"
	case KBlock:
		return "block"
	case KFifo:
		return "fifo"
	}
	return "?"
}

// Xattr is one extended attribute (POSIX ACLs are the system.posix_acl_* attributes).
type Xattr struct {
	Name  string
	Value []byte
}

// Entry is one node of the tree.
type Entry struct {
	Kind    Kind
	Name    string // base name; "" for the root
	Mode    uint32 // permission bits and setuid/setgid/sticky
	UID     uint32
	GID     uint32
	MtimeNs int64
	CtimeNs int64
	Inode   uint64
	Size    uint64   // regular files
	PayOff  uint64   // regular files with Size > 0: offset of the content in the payload
	Sum     [32]byte // regular files with Size > 0: SHA-256 of the content
	Link    string   // symlink target; for a hardlink the path of the first link
	Major   uint32   // char and block devices
	Minor   uint32
	Xattrs  []Xattr // sorted by name

	Children []*Entry // directories, sorted by Name
}

// Tree is a whole catalog.
type Tree struct {
	Root *Entry
	// Count is the number of entries including the root.
	Count int
}

// Magic and version of the encoding.
const (
	magic   = "IBCT"
	version = 1
)

// ---- encoding --------------------------------------------------------------------

// encoder appends the encoding to one buffer. Bytes keeps the whole encoding in it; Encode
// flushes it to its writer (and the running digest) whenever it holds flushAt bytes, so streaming
// a catalog needs one small buffer instead of the whole encoding.
type encoder struct {
	b   []byte
	w   io.Writer
	h   hash.Hash
	err error
}

// flushAt is the buffer size at which Encode hands the encoding on.
const flushAt = 1 << 20

func (e *encoder) flush() {
	if e.err != nil || len(e.b) == 0 {
		return
	}
	e.h.Write(e.b)
	_, e.err = e.w.Write(e.b)
	e.b = e.b[:0]
}

func (e *encoder) uvarint(v uint64) { e.b = binary.AppendUvarint(e.b, v) }

func (e *encoder) varint(v int64) { e.b = binary.AppendVarint(e.b, v) }

func (e *encoder) str(s string) {
	e.uvarint(uint64(len(s)))
	e.b = append(e.b, s...)
}

func (e *encoder) xattrs(x []Xattr) {
	e.uvarint(uint64(len(x)))
	for _, a := range x {
		e.str(a.Name)
		e.uvarint(uint64(len(a.Value)))
		e.b = append(e.b, a.Value...)
	}
}

func (e *encoder) entry(x *Entry) {
	e.b = append(e.b, byte(x.Kind))
	e.str(x.Name)
	e.uvarint(uint64(x.Mode))
	e.uvarint(uint64(x.UID))
	e.uvarint(uint64(x.GID))
	e.varint(x.MtimeNs)
	e.varint(x.CtimeNs)
	e.uvarint(x.Inode)
	e.xattrs(x.Xattrs)
	switch x.Kind {
	case KDir:
		for _, c := range x.Children {
			e.entry(c)
		}
		e.b = append(e.b, 0)
	case KFile:
		e.uvarint(x.Size)
		if x.Size > 0 {
			e.uvarint(x.PayOff)
			e.b = append(e.b, x.Sum[:]...)
		}
	case KSymlink, KHardlink:
		e.str(x.Link)
	case KChar, KBlock:
		e.uvarint(uint64(x.Major))
		e.uvarint(uint64(x.Minor))
	}
	if e.w != nil && len(e.b) >= flushAt {
		e.flush()
	}
}

// Encode serialises the tree. The stream ends with a SHA-256 of everything before it, so a
// truncated or damaged catalog is rejected even before the chunk layer's own checks.
//
// The encoding is written in pieces of about flushAt bytes, never held whole (byte-identical to
// Bytes).
func (t *Tree) Encode(w io.Writer) error {
	e := &encoder{b: make([]byte, 0, flushAt+64<<10), w: w, h: sha256.New()}
	e.b = append(e.b, magic...)
	e.b = append(e.b, version, 0, 0, 0)
	e.entry(t.Root)
	e.flush()
	if e.err != nil {
		return e.err
	}
	// the digest itself is not hashed
	_, err := w.Write(e.h.Sum(nil))
	return err
}

// Bytes returns the encoded catalog.
func (t *Tree) Bytes() ([]byte, error) {
	// about 80 bytes per entry for typical names: one allocation instead of repeated growth
	e := &encoder{b: make([]byte, 0, 64+96*max(t.Count, 1))}
	e.b = append(e.b, magic...)
	e.b = append(e.b, version, 0, 0, 0)
	e.entry(t.Root)
	// the digest itself is not hashed
	sum := sha256.Sum256(e.b)
	return append(e.b, sum[:]...), nil
}

// ---- decoding --------------------------------------------------------------------

type decoder struct {
	b     []byte
	pos   int
	depth int
	count int
	// slab hands out entries from blocks (one allocation per block instead of per entry); stack
	// collects the children of the directories being decoded, copied out once at an exact size.
	slab  []Entry
	stack []*Entry
}

func (d *decoder) newEntry() *Entry {
	if len(d.slab) == 0 {
		d.slab = make([]Entry, 512)
	}
	x := &d.slab[0]
	d.slab = d.slab[1:]
	return x
}

var errShort = errors.New("catalog is truncated")

func (d *decoder) byte() (byte, error) {
	if d.pos >= len(d.b) {
		return 0, errShort
	}
	c := d.b[d.pos]
	d.pos++
	return c, nil
}

func (d *decoder) uvarint() (uint64, error) {
	v, n := binary.Uvarint(d.b[d.pos:])
	if n <= 0 {
		return 0, errShort
	}
	d.pos += n
	return v, nil
}

func (d *decoder) varint() (int64, error) {
	v, n := binary.Varint(d.b[d.pos:])
	if n <= 0 {
		return 0, errShort
	}
	d.pos += n
	return v, nil
}

func (d *decoder) bytes(n uint64) ([]byte, error) {
	if n > uint64(len(d.b)-d.pos) {
		return nil, errShort
	}
	out := d.b[d.pos : d.pos+int(n)]
	d.pos += int(n)
	return out, nil
}

func (d *decoder) str() (string, error) {
	n, err := d.uvarint()
	if err != nil {
		return "", err
	}
	b, err := d.bytes(n)
	return string(b), err
}

const maxDepth = 4096

// decode reads one entry whose kind byte has already been read.
func (d *decoder) entry(kind Kind) (*Entry, error) {
	if d.depth > maxDepth {
		return nil, errors.New("catalog nests too deeply")
	}
	d.count++
	x := d.newEntry()
	x.Kind = kind
	var err error
	if x.Name, err = d.str(); err != nil {
		return nil, err
	}
	// a name is one path component: no separator, no NUL (no Linux file name has either), never
	// "", "." or ".." below the root (a hostile catalog must not steer extraction or ZIP names)
	if strings.ContainsAny(x.Name, "/\x00") || x.Name == "." || x.Name == ".." || (x.Name == "" && d.depth > 0) {
		return nil, fmt.Errorf("catalog holds an unsafe name %q", x.Name)
	}
	var v uint64
	if v, err = d.uvarint(); err != nil {
		return nil, err
	}
	x.Mode = uint32(v)
	if v, err = d.uvarint(); err != nil {
		return nil, err
	}
	x.UID = uint32(v)
	if v, err = d.uvarint(); err != nil {
		return nil, err
	}
	x.GID = uint32(v)
	if x.MtimeNs, err = d.varint(); err != nil {
		return nil, err
	}
	if x.CtimeNs, err = d.varint(); err != nil {
		return nil, err
	}
	if x.Inode, err = d.uvarint(); err != nil {
		return nil, err
	}
	nx, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	if nx > uint64(len(d.b)) {
		return nil, errShort
	}
	for i := uint64(0); i < nx; i++ {
		name, err := d.str()
		if err != nil {
			return nil, err
		}
		vl, err := d.uvarint()
		if err != nil {
			return nil, err
		}
		val, err := d.bytes(vl)
		if err != nil {
			return nil, err
		}
		x.Xattrs = append(x.Xattrs, Xattr{Name: name, Value: append([]byte(nil), val...)})
	}
	switch kind {
	case KDir:
		d.depth++
		mark := len(d.stack)
		for {
			k, err := d.byte()
			if err != nil {
				return nil, err
			}
			if k == 0 {
				break
			}
			c, err := d.entry(Kind(k))
			if err != nil {
				return nil, err
			}
			d.stack = append(d.stack, c)
		}
		x.Children = append([]*Entry(nil), d.stack[mark:]...)
		clear(d.stack[mark:])
		d.stack = d.stack[:mark]
		d.depth--
		for i := 1; i < len(x.Children); i++ {
			if x.Children[i-1].Name >= x.Children[i].Name {
				return nil, fmt.Errorf("catalog directory entries are not sorted or repeat (%q)", x.Children[i].Name)
			}
		}
	case KFile:
		if x.Size, err = d.uvarint(); err != nil {
			return nil, err
		}
		if x.Size > 0 {
			if x.PayOff, err = d.uvarint(); err != nil {
				return nil, err
			}
			b, err := d.bytes(32)
			if err != nil {
				return nil, err
			}
			copy(x.Sum[:], b)
		}
	case KSymlink, KHardlink:
		if x.Link, err = d.str(); err != nil {
			return nil, err
		}
	case KChar, KBlock:
		if v, err = d.uvarint(); err != nil {
			return nil, err
		}
		x.Major = uint32(v)
		if v, err = d.uvarint(); err != nil {
			return nil, err
		}
		x.Minor = uint32(v)
	case KFifo:
	default:
		return nil, fmt.Errorf("catalog holds an unknown entry kind %d", kind)
	}
	return x, nil
}

// Decode parses an encoded catalog and checks its digest.
func Decode(b []byte) (*Tree, error) {
	if len(b) < 8+32 || string(b[:4]) != magic {
		return nil, errors.New("not a file catalog")
	}
	if b[4] != version {
		return nil, fmt.Errorf("unsupported catalog version %d", b[4])
	}
	body, sum := b[:len(b)-32], b[len(b)-32:]
	if h := sha256.Sum256(body); !bytes.Equal(h[:], sum) {
		return nil, errors.New("catalog digest mismatch (damaged or truncated)")
	}
	d := &decoder{b: body, pos: 8}
	k, err := d.byte()
	if err != nil {
		return nil, err
	}
	if Kind(k) != KDir {
		return nil, errors.New("catalog root is not a directory")
	}
	root, err := d.entry(KDir)
	if err != nil {
		return nil, err
	}
	if d.pos != len(body) {
		return nil, errors.New("catalog has trailing data")
	}
	return &Tree{Root: root, Count: d.count}, nil
}

// ---- lookup ----------------------------------------------------------------------

// CleanPath normalises a user supplied path ("/a//b/../c", "a/b/") to "a/b" form relative to
// the root; "" and "/" mean the root. It rejects escapes above the root.
func CleanPath(p string) (string, error) {
	var parts []string
	for _, c := range strings.Split(p, "/") {
		switch c {
		case "", ".":
		case "..":
			if len(parts) == 0 {
				return "", fmt.Errorf("path %q escapes the root", p)
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, "/"), nil
}

func (x *Entry) child(name string) *Entry {
	i := sort.Search(len(x.Children), func(i int) bool { return x.Children[i].Name >= name })
	if i < len(x.Children) && x.Children[i].Name == name {
		return x.Children[i]
	}
	return nil
}

// Lookup finds an entry by path ("" = root). Symlinks are not followed.
func (t *Tree) Lookup(p string) (*Entry, error) {
	c, err := CleanPath(p)
	if err != nil {
		return nil, err
	}
	cur := t.Root
	if c == "" {
		return cur, nil
	}
	for _, part := range strings.Split(c, "/") {
		if cur.Kind != KDir {
			return nil, fmt.Errorf("%s: not a directory", p)
		}
		if cur = cur.child(part); cur == nil {
			return nil, fmt.Errorf("%s: no such file or directory", p)
		}
	}
	return cur, nil
}

// Walk visits every entry in pre-order (the order of the payload) with its full path
// ("" for the root). Returning false from fn skips the entry's children.
func (t *Tree) Walk(fn func(path string, e *Entry) bool) {
	var rec func(p string, e *Entry)
	rec = func(p string, e *Entry) {
		if !fn(p, e) {
			return
		}
		for _, c := range e.Children {
			cp := c.Name
			if p != "" {
				cp = p + "/" + c.Name
			}
			rec(cp, c)
		}
	}
	rec("", t.Root)
}

// Stats summarises a tree.
type Stats struct {
	Files, Dirs, Symlinks, Hardlinks, Special int
	Bytes                                     uint64
}

// Stats counts the entries.
func (t *Tree) Stats() Stats {
	var s Stats
	t.Walk(func(_ string, e *Entry) bool {
		switch e.Kind {
		case KDir:
			s.Dirs++
		case KFile:
			s.Files++
			s.Bytes += e.Size
		case KSymlink:
			s.Symlinks++
		case KHardlink:
			s.Hardlinks++
		default:
			s.Special++
		}
		return true
	})
	return s
}

// Equal reports whether two xattr lists are identical (both sorted by name).
func xattrsEqual(a, b []Xattr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || !bytes.Equal(a[i].Value, b[i].Value) {
			return false
		}
	}
	return true
}
