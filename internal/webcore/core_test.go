package webcore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/fscat"
)

func master(t *testing.T) ([]byte, string) {
	t.Helper()
	m, err := format.GenerateMaster()
	if err != nil {
		t.Fatal(err)
	}
	s, err := format.EncodeRecoveryKey(m)
	if err != nil {
		t.Fatal(err)
	}
	return m, s
}

func TestRecoveryKeyLoadAndBadChecksum(t *testing.T) {
	c := New()
	m, s := master(t)
	h, dom, err := c.LoadRecoveryKey(strings.ToLower(s))
	if err != nil {
		t.Fatal(err)
	}
	k, _ := format.DeriveKeys(m)
	if dom != k.DomainID {
		t.Fatalf("domain %s, want %s", dom, k.DomainID)
	}
	g, _ := c.GroupID(h, "default", "filesystem", "volume/docs")
	if g != k.GroupID("default", "filesystem", "volume/docs") {
		t.Fatal("group id differs from format.GroupID")
	}
	// flip one character of the key body
	b := []byte(s)
	i := len(b) - 3
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	_, _, err = c.LoadRecoveryKey(string(b))
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a typo must fail with a checksum message, got %v", err)
	}
	if strings.Contains(err.Error(), string(b)) || strings.Contains(err.Error(), s[5:15]) {
		t.Fatal("the error echoes the key")
	}
	held := c.keys[h]
	c.DropKey(h)
	if _, err := c.GroupID(h, "a", "b", "c"); err == nil {
		t.Fatal("a dropped key still works")
	}
	if held.Enc != [32]byte{} || held.ID != [32]byte{} || held.Manifest != [32]byte{} || held.Delivery != [32]byte{} {
		t.Fatal("a dropped key was not wiped")
	}
}

func TestZeroizeWipesKeyBytes(t *testing.T) {
	c := New()
	_, s := master(t)
	h, _, err := c.LoadRecoveryKey(s)
	if err != nil {
		t.Fatal(err)
	}
	k := c.keys[h]
	c.Zeroize()
	if k.Enc != [32]byte{} || k.ID != [32]byte{} || k.Manifest != [32]byte{} || k.Delivery != [32]byte{} {
		t.Fatal("sub-keys were not wiped")
	}
	if len(c.keys) != 0 {
		t.Fatal("keys remain")
	}
}

func TestEscrowLoad(t *testing.T) {
	c := New()
	m, _ := master(t)
	priv, pub, err := format.GenerateEscrowKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	w, err := format.WrapMasterKey(m, pub)
	if err != nil {
		t.Fatal(err)
	}
	_, dom, err := c.LoadEscrow(w, priv)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := format.DeriveKeys(m)
	if dom != k.DomainID {
		t.Fatal("escrow domain")
	}
	w[len(w)-1] ^= 1
	if _, _, err := c.LoadEscrow(w, priv); err == nil {
		t.Fatal("altered escrow blob accepted")
	}
}

// snapshot builds a small catalog snapshot with the real encoders.
func snapshot(t *testing.T, k *format.Keys, group, snap string, files map[string][]byte) (man, idx []byte, blobs map[string][]byte) {
	t.Helper()
	return snapshotOf(t, k, group, snap, "default", "volume/docs", files)
}

// snapshotOf is snapshot for any object; group may be another object's (a lying writer).
func snapshotOf(t *testing.T, k *format.Keys, group, snap, project, name string, files map[string][]byte) (man, idx []byte, blobs map[string][]byte) {
	t.Helper()
	root := &fscat.Entry{Kind: fscat.KDir, Mode: 0o755}
	var payload []byte
	var names []string
	for n := range files {
		names = append(names, n)
	}
	// sorted, as the catalog requires
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, n := range names {
		d := files[n]
		e := &fscat.Entry{Kind: fscat.KFile, Name: n, Mode: 0o644, Size: uint64(len(d)), PayOff: uint64(len(payload)), Sum: sha256.Sum256(d)}
		root.Children = append(root.Children, e)
		payload = append(payload, d...)
	}
	cat, err := (&fscat.Tree{Root: root}).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	blobs = map[string][]byte{}
	x := &format.Index{Kind: format.IndexDynamic}
	add := func(p []byte) {
		id, blob, err := k.EncodeChunk(p, format.EncodeOpts{})
		if err != nil {
			t.Fatal(err)
		}
		blobs[id.String()] = blob
		x.Size += uint64(len(p))
		x.Entries = append(x.Entries, format.Entry{End: x.Size, ID: id})
	}
	add(payload)
	add(cat)
	idx, err = x.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	m := &format.Manifest{SnapshotID: snap, Created: time.Now(), Project: project, Object: format.ObjectInfo{Name: name, Kind: "filesystem", Size: x.Size},
		IndexDigest: format.IndexDigest(idx), Meta: map[string]string{MetaFSFormat: "2", MetaFSPayload: strconv.Itoa(len(payload)), MetaFSCatalog: strconv.Itoa(len(cat))}}
	man, err = k.SealManifest(m, group, nil)
	if err != nil {
		t.Fatal(err)
	}
	return man, idx, blobs
}

func TestOpenSnapshotBindsIndexAndBrowses(t *testing.T) {
	c := New()
	m, s := master(t)
	k, _ := format.DeriveKeys(m)
	h, _, _ := c.LoadRecoveryKey(s)
	g := k.GroupID("default", "filesystem", "volume/docs")
	const snapA, snapB = "20261007120000-00000001", "20261007130000-00000002"
	manA, idxA, blobs := snapshot(t, k, g, snapA, map[string][]byte{"a.txt": []byte("hello"), "b.bin": bytes.Repeat([]byte{7}, 1000)})
	_, idxB, _ := snapshot(t, k, g, snapB, map[string][]byte{"other": []byte("x")})

	if _, err := c.OpenSnapshot(h, g, snapA, manA, idxB, nil); err == nil || !strings.Contains(err.Error(), "does not match its manifest") {
		t.Fatalf("another snapshot's index must be rejected, got %v", err)
	}
	if _, err := c.OpenSnapshot(h, g, snapB, manA, idxA, nil); err == nil {
		t.Fatal("a manifest served under another snapshot id must be rejected")
	}
	si, err := c.OpenSnapshot(h, g, snapA, manA, idxA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if si.Format != "catalog" || si.PayloadEntries != 1 || len(si.IDs) != 2 {
		t.Fatalf("%+v", si)
	}
	var mj map[string]any
	if err := json.Unmarshal(si.Manifest, &mj); err != nil || mj["snapshot_id"] != snapA {
		t.Fatalf("manifest json: %v %v", err, mj)
	}
	// chunk X served as chunk Y
	if _, err := c.DecodeChunk(h, blobs[si.IDs[1]], si.IDs[0]); err == nil {
		t.Fatal("a valid blob under the wrong id must fail")
	}
	cat, err := c.DecodeChunk(h, blobs[si.IDs[1]], si.IDs[1])
	if err != nil {
		t.Fatal(err)
	}
	ch, n, st, err := c.CatalogOpen(cat)
	if err != nil || n != 3 || st.Files != 2 {
		t.Fatalf("%v %d %+v", err, n, st)
	}
	l, err := c.List(ch, "/")
	if err != nil || len(l) != 2 || l[0].Path != "a.txt" || l[1].Size != 1000 {
		t.Fatalf("%v %+v", err, l)
	}
	r, err := c.ResolveFile(ch, "b.bin")
	if err != nil || r.PayOff != 5 || r.Size != 1000 {
		t.Fatalf("%v %+v", err, r)
	}
	if _, err := c.ResolveFile(ch, "/"); err == nil {
		t.Fatal("a directory is not a file")
	}
	if _, err := c.List(ch, "../x"); err == nil {
		t.Fatal("escape accepted")
	}
	f, _ := c.Find(ch, "", "*.txt")
	if len(f) != 1 || f[0] != "a.txt" {
		t.Fatal(f)
	}
	// the wrong key
	_, s2 := master(t)
	h2, _, _ := c.LoadRecoveryKey(s2)
	if _, err := c.OpenSnapshot(h2, g, snapA, manA, idxA, nil); err == nil {
		t.Fatal("wrong key opened the manifest")
	}
	if _, err := c.DecodeChunk(h2, blobs[si.IDs[0]], si.IDs[0]); err == nil {
		t.Fatal("wrong key decoded a chunk")
	}
	cat[10] ^= 1
	if _, _, _, err := c.CatalogOpen(cat); err == nil {
		t.Fatal("a damaged catalog was accepted")
	}
}

func TestWalkPagingMatchesTreeWalk(t *testing.T) {
	c := New()
	var mk func(depth int, name string) *fscat.Entry
	mk = func(depth int, name string) *fscat.Entry {
		e := &fscat.Entry{Kind: fscat.KDir, Name: name}
		if depth == 0 {
			return e
		}
		for i := 0; i < 4; i++ {
			e.Children = append(e.Children, mk(depth-1, "d"+strconv.Itoa(i)))
		}
		e.Children = append(e.Children, &fscat.Entry{Kind: fscat.KFile, Name: "f"})
		return e
	}
	tree := &fscat.Tree{Root: mk(4, "")}
	b, _ := tree.Bytes()
	ch, n, _, err := c.CatalogOpen(b)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	tree.Walk(func(p string, _ *fscat.Entry) bool { want = append(want, p); return true })
	if n != len(want) {
		t.Fatalf("count %d, walk %d", n, len(want))
	}
	w, _ := c.WalkOpen(ch, "")
	var got []string
	for {
		l, done, err := c.WalkNext(w, 7)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range l {
			got = append(got, e.Path)
		}
		if done {
			break
		}
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatal("paged walk differs from Tree.Walk")
	}
	w, _ = c.WalkOpen(ch, "d1/d2")
	l, done, _ := c.WalkNext(w, 1000)
	if !done || l[0].Path != "d1/d2" || l[1].Path != "d1/d2/d0" {
		t.Fatalf("%v %+v", done, l[:2])
	}
}

func TestHashMatchesSHA256(t *testing.T) {
	c := New()
	h := c.HashNew()
	_ = c.HashUpdate(h, []byte("abc"))
	_ = c.HashZeros(h, 200000)
	want := sha256.Sum256(append([]byte("abc"), make([]byte, 200000)...))
	if got, _ := c.HashSum(h); got != hex.EncodeToString(want[:]) {
		t.Fatal("hash differs")
	}
}
