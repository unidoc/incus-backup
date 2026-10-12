// Package webcore is the key-holding half of the browser library (web-crypto/): everything that
// needs a key or parses repository bytes, built on the real internal/format and internal/fscat
// code so the browser runs the same implementation as the host. cmd/ibwasm exposes it to
// JavaScript; this package itself is plain Go so it is tested on the host.
//
// Key material never leaves a Core: callers hold integer handles. Errors never contain key
// material (they are the format package's messages, which name no key bytes).
package webcore

import (
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"sort"
	"strconv"
	"sync"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/fscat"
)

// Manifest metadata keys of catalog snapshots. They mirror the constants in
// internal/client/engine/fs2.go (that package cannot be linked into the browser module: it pulls
// in Incus, NBD and the network client).
const (
	MetaFSFormat  = "fs.format"
	MetaFSPayload = "fs.payload_size"
	MetaFSCatalog = "fs.catalog_size"
)

// Core holds the open keys, catalogs and stream states of one page.
type Core struct {
	mu      sync.Mutex
	next    int
	keys    map[int]*format.Keys
	cats    map[int]*fscat.Tree
	walks   map[int]*walker
	hashes  map[int]hash.Hash
	unseals map[int]*unsealer
	seals   map[int]*ecdh.PrivateKey // one-time X25519 keys of sealed deliveries (never leave the Core)
}

// New returns an empty Core.
func New() *Core {
	return &Core{keys: map[int]*format.Keys{}, cats: map[int]*fscat.Tree{}, walks: map[int]*walker{},
		hashes: map[int]hash.Hash{}, unseals: map[int]*unsealer{}, seals: map[int]*ecdh.PrivateKey{}}
}

func (c *Core) id() int { c.next++; return c.next }

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func wipeKeys(k *format.Keys) {
	wipe(k.Enc[:])
	wipe(k.ID[:])
	wipe(k.Manifest[:])
	wipe(k.Delivery[:])
}

func (c *Core) addKeys(master []byte) (int, string, error) {
	defer wipe(master)
	k, err := format.DeriveKeys(master)
	if err != nil {
		return 0, "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.id()
	c.keys[h] = k
	return h, k.DomainID, nil
}

// LoadRecoveryKey decodes and checksum-verifies an IBR1 recovery key and keeps the derived
// sub-keys under a handle. The master key itself is wiped right after derivation.
func (c *Core) LoadRecoveryKey(s string) (int, string, error) {
	m, err := format.DecodeRecoveryKey(s)
	if err != nil {
		return 0, "", err
	}
	return c.addKeys(m)
}

// LoadEscrow opens an escrow blob (format.WrapMasterKey) with the printable IBE1 private key.
func (c *Core) LoadEscrow(wrapped []byte, priv string) (int, string, error) {
	m, _, err := format.UnwrapMasterKey(wrapped, priv)
	if err != nil {
		return 0, "", err
	}
	return c.addKeys(m)
}

func (c *Core) key(h int) (*format.Keys, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.keys[h]
	if !ok {
		return nil, errors.New("the key was closed")
	}
	return k, nil
}

// DropKey wipes and forgets a key.
func (c *Core) DropKey(h int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if k, ok := c.keys[h]; ok {
		wipeKeys(k)
		delete(c.keys, h)
	}
}

// Zeroize wipes every key and forgets every catalog, walk, hash and stream state.
func (c *Core) Zeroize() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for h, k := range c.keys {
		wipeKeys(k)
		delete(c.keys, h)
	}
	for h, u := range c.unseals {
		u.close()
		delete(c.unseals, h)
	}
	for h := range c.seals {
		delete(c.seals, h) // crypto/ecdh keeps its bytes private: dropping the last reference is all Go allows
	}
	c.cats = map[int]*fscat.Tree{}
	c.walks = map[int]*walker{}
	c.hashes = map[int]hash.Hash{}
}

// GroupID returns the opaque group id of an object (format.Keys.GroupID).
func (c *Core) GroupID(h int, project, kind, name string) (string, error) {
	k, err := c.key(h)
	if err != nil {
		return "", err
	}
	return k.GroupID(project, kind, name), nil
}

// SnapshotInfo is an opened, verified snapshot: its manifest and its chunk index.
type SnapshotInfo struct {
	Manifest  json.RawMessage `json:"manifest"`
	Kind      string          `json:"kind"`   // manifest object kind: block, filesystem, instance
	Format    string          `json:"format"` // "catalog", "archive", "block", "instance"
	IndexKind int             `json:"indexKind"`
	ChunkSize uint32          `json:"chunkSize"`
	Size      uint64          `json:"size"`
	Ends      []uint64        `json:"-"`
	IDs       []string        `json:"ids"` // "" = all-zero chunk (no blob)
	// catalog snapshots only
	PayloadSize    uint64 `json:"payloadSize"`
	CatalogSize    uint64 `json:"catalogSize"`
	PayloadEntries int    `json:"payloadEntries"` // entries [0, PayloadEntries) are the payload, the rest the catalog
}

// Identity names a backed up object: what the server's listing claims a group holds.
type Identity struct {
	Project string `json:"project"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
}

// OpenSnapshot decrypts the manifest, binds the plaintext index to it (the manifest records the
// index digest: a server that swaps in another snapshot's index is detected here) and checks the
// catalog layout.
//
// Identity: the group id is keyed (format.Keys.GroupID of project, kind and object name), so the
// object a manifest describes must hash to the group it was opened under; a manifest sealed under
// another object's group is refused. The server chooses which group it LABELS with which name in
// its listings, so a caller that shows a name passes it as expect: the group must then be that
// object's group, or the snapshot is refused (the server presented object B's data as object A).
func (c *Core) OpenSnapshot(h int, group, snap string, manifest, index []byte, expect *Identity) (*SnapshotInfo, error) {
	k, err := c.key(h)
	if err != nil {
		return nil, err
	}
	if !format.ValidSnapshotID(snap) {
		return nil, fmt.Errorf("%q is not a snapshot id", snap)
	}
	if expect != nil && k.GroupID(expect.Project, expect.Kind, expect.Name) != group {
		return nil, fmt.Errorf("snapshot identity does not match: the server presented group %s as %s/%s (%s), which is another object", group, expect.Project, expect.Name, expect.Kind)
	}
	m, err := k.OpenManifest(manifest, group, snap)
	if err != nil {
		return nil, err
	}
	if k.GroupID(m.Project, m.Object.Kind, m.Object.Name) != group {
		return nil, fmt.Errorf("snapshot identity does not match: the manifest describes %s/%s (%s), which is not stored under group %s", m.Project, m.Object.Name, m.Object.Kind, group)
	}
	if m.SnapshotID != "" && m.SnapshotID != snap {
		return nil, fmt.Errorf("snapshot identity does not match: the manifest is of snapshot %s, not %s", m.SnapshotID, snap)
	}
	if m.IndexDigest != format.IndexDigest(index) {
		return nil, fmt.Errorf("the index of %s does not match its manifest: the repository was modified", snap)
	}
	x, err := format.ParseIndex(index)
	if err != nil {
		return nil, err
	}
	mj, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	si := &SnapshotInfo{Manifest: mj, Kind: m.Object.Kind, IndexKind: x.Kind, ChunkSize: x.ChunkSize, Size: x.Size}
	si.Ends = make([]uint64, len(x.Entries))
	si.IDs = make([]string, len(x.Entries))
	for i, e := range x.Entries {
		si.Ends[i] = e.End
		if !e.ID.IsZero() {
			si.IDs[i] = e.ID.String()
		}
	}
	switch {
	case m.Object.Kind == "block":
		si.Format = "block"
	case m.Object.Kind == "instance":
		si.Format = "instance"
	case m.Meta[MetaFSFormat] == "2":
		si.Format = "catalog"
	default:
		si.Format = "archive"
	}
	if si.Format != "catalog" {
		return si, nil
	}
	pay, err := strconv.ParseUint(m.Meta[MetaFSPayload], 10, 64)
	if err != nil {
		return nil, errors.New("manifest lacks fs.payload_size")
	}
	cat, err := strconv.ParseUint(m.Meta[MetaFSCatalog], 10, 64)
	if err != nil || pay+cat != x.Size || cat == 0 {
		return nil, errors.New("manifest layout does not match the index")
	}
	n := sort.Search(len(x.Entries), func(i int) bool { return x.Entries[i].End > pay })
	if pay > 0 && (n == 0 || x.Entries[n-1].End != pay) {
		return nil, errors.New("the payload does not end on a chunk boundary")
	}
	si.PayloadSize, si.CatalogSize, si.PayloadEntries = pay, cat, n
	return si, nil
}

// DecodeChunk verifies and decrypts one chunk blob that the index names idHex.
func (c *Core) DecodeChunk(h int, blob []byte, idHex string) ([]byte, error) {
	k, err := c.key(h)
	if err != nil {
		return nil, err
	}
	id, err := format.ParseID(idHex)
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, errors.New("the zero chunk has no blob")
	}
	pt, err := k.DecodeChunk(blob, id)
	if err != nil {
		return nil, fmt.Errorf("chunk %s: %w", idHex, err)
	}
	return pt, nil
}

// ---- catalogs ---------------------------------------------------------------------------

// Xattr is an extended attribute; Value is base64 in JSON.
type Xattr struct {
	Name  string `json:"name"`
	Value []byte `json:"value"`
}

// EntryInfo is the JSON form of a catalog entry.
type EntryInfo struct {
	Path       string  `json:"path"`
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	Mode       uint32  `json:"mode"`
	ModeString string  `json:"modeString"`
	UID        uint32  `json:"uid"`
	GID        uint32  `json:"gid"`
	MtimeNs    string  `json:"mtimeNs"` // decimal: nanoseconds do not fit a JS number
	CtimeNs    string  `json:"ctimeNs"`
	Inode      string  `json:"inode"`
	Size       uint64  `json:"size"`
	SHA256     string  `json:"sha256,omitempty"`
	Link       string  `json:"link,omitempty"`
	Major      uint32  `json:"major,omitempty"`
	Minor      uint32  `json:"minor,omitempty"`
	Xattrs     []Xattr `json:"xattrs,omitempty"`
	Children   int     `json:"children,omitempty"`
}

func info(p string, e *fscat.Entry) EntryInfo {
	x := EntryInfo{Path: p, Name: e.Name, Kind: e.Kind.String(), Mode: e.Mode, ModeString: e.ModeString(), UID: e.UID, GID: e.GID,
		MtimeNs: strconv.FormatInt(e.MtimeNs, 10), CtimeNs: strconv.FormatInt(e.CtimeNs, 10), Inode: strconv.FormatUint(e.Inode, 10),
		Size: e.Size, Link: e.Link, Major: e.Major, Minor: e.Minor, Children: len(e.Children)}
	if e.Kind == fscat.KFile && e.Size > 0 {
		x.SHA256 = hex.EncodeToString(e.Sum[:])
	}
	for _, a := range e.Xattrs {
		x.Xattrs = append(x.Xattrs, Xattr{Name: a.Name, Value: a.Value})
	}
	return x
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// CatalogOpen decodes a catalog (fscat.Decode, which checks its digest).
func (c *Core) CatalogOpen(b []byte) (int, int, fscat.Stats, error) {
	t, err := fscat.Decode(b)
	if err != nil {
		return 0, 0, fscat.Stats{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.id()
	c.cats[h] = t
	return h, t.Count, t.Stats(), nil
}

// CatalogClose forgets a catalog and its walks.
func (c *Core) CatalogClose(h int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cats, h)
	for w, x := range c.walks {
		if x.cat == h {
			delete(c.walks, w)
		}
	}
}

func (c *Core) cat(h int) (*fscat.Tree, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.cats[h]
	if !ok {
		return nil, errors.New("the catalog was closed")
	}
	return t, nil
}

// List returns the children of a directory (or the entry itself).
func (c *Core) List(h int, p string) ([]EntryInfo, error) {
	t, err := c.cat(h)
	if err != nil {
		return nil, err
	}
	cp, err := fscat.CleanPath(p)
	if err != nil {
		return nil, err
	}
	e, err := t.Lookup(cp)
	if err != nil {
		return nil, err
	}
	if e.Kind != fscat.KDir {
		return []EntryInfo{info(cp, e)}, nil
	}
	out := make([]EntryInfo, 0, len(e.Children))
	for _, ch := range e.Children {
		out = append(out, info(join(cp, ch.Name), ch))
	}
	return out, nil
}

// Stat returns one entry.
func (c *Core) Stat(h int, p string) (EntryInfo, error) {
	t, err := c.cat(h)
	if err != nil {
		return EntryInfo{}, err
	}
	cp, err := fscat.CleanPath(p)
	if err != nil {
		return EntryInfo{}, err
	}
	e, err := t.Lookup(cp)
	if err != nil {
		return EntryInfo{}, err
	}
	return info(cp, e), nil
}

// Find returns the paths below root whose base name matches pattern (path.Match syntax).
func (c *Core) Find(h int, root, pattern string) ([]string, error) {
	t, err := c.cat(h)
	if err != nil {
		return nil, err
	}
	out, err := t.Find(root, pattern)
	if out == nil {
		out = []string{}
	}
	return out, err
}

// Subtree returns the number of entries and the total file bytes at and below p.
func (c *Core) Subtree(h int, p string) (int, uint64, error) {
	t, err := c.cat(h)
	if err != nil {
		return 0, 0, err
	}
	cp, err := fscat.CleanPath(p)
	if err != nil {
		return 0, 0, err
	}
	return t.Subtree(cp)
}

// FileRef locates the content of a regular file in the payload.
type FileRef struct {
	Path   string `json:"path"` // the path whose content this is (a hardlink's first link)
	Size   uint64 `json:"size"`
	PayOff uint64 `json:"payOff"`
	SHA256 string `json:"sha256"` // "" for an empty file
}

// ResolveFile finds the content of a regular file, following a hard link to its first link.
func (c *Core) ResolveFile(h int, p string) (FileRef, error) {
	t, err := c.cat(h)
	if err != nil {
		return FileRef{}, err
	}
	cp, err := fscat.CleanPath(p)
	if err != nil {
		return FileRef{}, err
	}
	e, err := t.Lookup(cp)
	if err != nil {
		return FileRef{}, err
	}
	if e.Kind == fscat.KHardlink {
		cp = e.Link
		if e, err = t.Lookup(e.Link); err != nil {
			return FileRef{}, fmt.Errorf("%s: hard link target: %w", p, err)
		}
	}
	if e.Kind != fscat.KFile {
		return FileRef{}, fmt.Errorf("%s is a %s, not a regular file", p, e.Kind)
	}
	r := FileRef{Path: cp, Size: e.Size, PayOff: e.PayOff}
	if e.Size > 0 {
		r.SHA256 = hex.EncodeToString(e.Sum[:])
	}
	return r, nil
}

// walker is a resumable pre-order walk (the order of the payload).
type walker struct {
	cat   int
	stack []frame
}

type frame struct {
	path string
	e    *fscat.Entry
	next int // -1: the entry itself has not been emitted yet
}

// WalkOpen starts a pre-order walk of the subtree at root (root itself included).
func (c *Core) WalkOpen(h int, root string) (int, error) {
	t, err := c.cat(h)
	if err != nil {
		return 0, err
	}
	cp, err := fscat.CleanPath(root)
	if err != nil {
		return 0, err
	}
	e, err := t.Lookup(cp)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.id()
	c.walks[w] = &walker{cat: h, stack: []frame{{path: cp, e: e, next: -1}}}
	return w, nil
}

// WalkNext returns up to max further entries; done is true when the walk is exhausted (the
// walk is then forgotten).
func (c *Core) WalkNext(w, max int) ([]EntryInfo, bool, error) {
	c.mu.Lock()
	x, ok := c.walks[w]
	c.mu.Unlock()
	if !ok {
		return nil, true, errors.New("the walk was closed")
	}
	if max <= 0 {
		max = 1000
	}
	out := []EntryInfo{}
	for len(x.stack) > 0 && len(out) < max {
		top := &x.stack[len(x.stack)-1]
		if top.next < 0 {
			out = append(out, info(top.path, top.e))
			top.next = 0
			continue
		}
		if top.next >= len(top.e.Children) {
			x.stack = x.stack[:len(x.stack)-1]
			continue
		}
		ch := top.e.Children[top.next]
		top.next++
		x.stack = append(x.stack, frame{path: join(top.path, ch.Name), e: ch, next: -1})
	}
	done := len(x.stack) == 0
	if done {
		c.WalkClose(w)
	}
	return out, done, nil
}

// WalkClose forgets a walk.
func (c *Core) WalkClose(w int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.walks, w)
}

// ---- streaming SHA-256 ------------------------------------------------------------------

// HashNew starts a SHA-256.
func (c *Core) HashNew() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.id()
	c.hashes[h] = sha256.New()
	return h
}

// HashUpdate adds data.
func (c *Core) HashUpdate(h int, b []byte) error {
	c.mu.Lock()
	x, ok := c.hashes[h]
	c.mu.Unlock()
	if !ok {
		return errors.New("unknown hash")
	}
	_, _ = x.Write(b)
	return nil
}

// HashZeros adds n zero bytes (all-zero chunks never cross into Go as data).
func (c *Core) HashZeros(h int, n uint64) error {
	c.mu.Lock()
	x, ok := c.hashes[h]
	c.mu.Unlock()
	if !ok {
		return errors.New("unknown hash")
	}
	var z [64 << 10]byte
	for n > 0 {
		k := uint64(len(z))
		if k > n {
			k = n
		}
		_, _ = x.Write(z[:k])
		n -= k
	}
	return nil
}

// HashSum returns the hex digest and forgets the hash.
func (c *Core) HashSum(h int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	x, ok := c.hashes[h]
	if !ok {
		return "", errors.New("unknown hash")
	}
	delete(c.hashes, h)
	return hex.EncodeToString(x.Sum(nil)), nil
}
