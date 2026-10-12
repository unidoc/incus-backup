package format

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/vectors.json")

func fixedMaster() []byte {
	m := make([]byte, 32)
	for i := range m {
		m[i] = byte(i)
	}
	return m
}

func fixedNonce(b byte) []byte { return bytes.Repeat([]byte{b}, NonceSize) }

type vectors struct {
	Master        string `json:"master_hex"`
	Enc           string `json:"k_enc_hex"`
	ID            string `json:"k_id_hex"`
	Manifest      string `json:"k_manifest_hex"`
	Domain        string `json:"domain_id"`
	ChunkPlain    string `json:"chunk_plaintext_hex"`
	ChunkNonce    string `json:"chunk_nonce_hex"`
	ChunkID       string `json:"chunk_id_hex"`
	ChunkBlob     string `json:"chunk_blob_hex"`
	Group         string `json:"group_id"`
	IndexFile     string `json:"index_file_hex"`
	ManifestNonce string `json:"manifest_nonce_hex"`
	ManifestJSON  string `json:"manifest_json"`
	ManifestBlob  string `json:"manifest_blob_hex"`
	Recovery      string `json:"recovery_key"`
}

func buildVectors(t *testing.T) vectors {
	t.Helper()
	master := fixedMaster()
	k, err := DeriveKeys(master)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("incus-backup test vector chunk\n")
	nonce := fixedNonce(0x11)
	id, blob, err := k.EncodeChunk(plain, EncodeOpts{NoCompress: true, Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	idx := &Index{Kind: IndexFixed, ChunkSize: 16, Size: 31, Entries: []Entry{{End: 16, ID: id}, {End: 31, ID: ZeroID}}}
	idxFile, err := idx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	group := k.GroupID("custody", "block", "custody-benfica-bosch/root")
	m := &Manifest{
		SnapshotID:  "20261007230000-0a1b2c3d",
		Created:     time.Date(2026, 10, 7, 23, 0, 0, 0, time.UTC),
		Host:        "benfica",
		Project:     "custody",
		Policy:      "production",
		Object:      ObjectInfo{Name: "custody-benfica-bosch/root", Kind: "block", Size: 31},
		IndexDigest: IndexDigest(idxFile),
	}
	mn := fixedNonce(0x22)
	mb, err := k.SealManifest(m, group, mn)
	if err != nil {
		t.Fatal(err)
	}
	mj, _ := json.Marshal(m)
	rk, _ := EncodeRecoveryKey(master)
	return vectors{
		Master: hex.EncodeToString(master), Enc: hex.EncodeToString(k.Enc[:]), ID: hex.EncodeToString(k.ID[:]),
		Manifest: hex.EncodeToString(k.Manifest[:]), Domain: k.DomainID,
		ChunkPlain: hex.EncodeToString(plain), ChunkNonce: hex.EncodeToString(nonce),
		ChunkID: id.String(), ChunkBlob: hex.EncodeToString(blob), Group: group,
		IndexFile: hex.EncodeToString(idxFile), ManifestNonce: hex.EncodeToString(mn),
		ManifestJSON: string(mj), ManifestBlob: hex.EncodeToString(mb), Recovery: rk,
	}
}

// TestVectors pins every byte of the format. If this fails the on-disk format
// changed: bump the format version and update docs/format.md, never just -update.
func TestVectors(t *testing.T) {
	got := buildVectors(t)
	if *update {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile("testdata/vectors.json", append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var want vectors
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if got != want {
		gb, _ := json.MarshalIndent(got, "", " ")
		wb, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("format vectors changed\n got: %s\nwant: %s", gb, wb)
	}
}

func TestChunkRoundTripAndCompression(t *testing.T) {
	k, _ := DeriveKeys(fixedMaster())
	for _, name := range []string{"small", "compressible", "random-ish"} {
		var pt []byte
		switch name {
		case "small":
			pt = []byte("x")
		case "compressible":
			pt = bytes.Repeat([]byte("abcdefgh"), 100000)
		default:
			pt = make([]byte, 50000)
			s := uint32(12345)
			for i := range pt {
				s = s*1664525 + 1013904223
				pt[i] = byte(s >> 24)
			}
		}
		id, blob, err := k.EncodeChunk(pt, EncodeOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if name == "compressible" && len(blob) > len(pt)/10 {
			t.Fatalf("compressible data not compressed: %d -> %d", len(pt), len(blob))
		}
		got, err := k.DecodeChunk(blob, id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("%s: round trip mismatch", name)
		}
	}
}

func TestChunkEmptyIsRepresentable(t *testing.T) {
	k, _ := DeriveKeys(fixedMaster())
	id, blob, err := k.EncodeChunk(nil, EncodeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	pt, err := k.DecodeChunk(blob, id)
	if err != nil || len(pt) != 0 {
		t.Fatalf("empty chunk: %v len=%d", err, len(pt))
	}
}

// Every single byte of a blob is covered by either the trailer or the AEAD.
func TestChunkTamperEveryByte(t *testing.T) {
	k, _ := DeriveKeys(fixedMaster())
	id, blob, _ := k.EncodeChunk([]byte("hello world, hello chunk"), EncodeOpts{NoCompress: true, Nonce: fixedNonce(7)})
	for i := range blob {
		c := append([]byte{}, blob...)
		c[i] ^= 0x01
		if _, err := k.DecodeChunk(c, id); err == nil {
			t.Fatalf("flipping byte %d went undetected", i)
		}
		if i >= len(blob)-trailerSize {
			continue
		}
		if VerifyBlob(c) == nil {
			t.Fatalf("keyless trailer check missed flip at byte %d", i)
		}
	}
	if err := VerifyBlob(blob); err != nil {
		t.Fatal(err)
	}
}

// Negative control for the trailer: an attacker who recomputes the trailer still
// cannot pass AEAD authentication.
func TestChunkRecomputedTrailerStillRejected(t *testing.T) {
	k, _ := DeriveKeys(fixedMaster())
	id, blob, _ := k.EncodeChunk([]byte("secret payload"), EncodeOpts{NoCompress: true, Nonce: fixedNonce(3)})
	c := append([]byte{}, blob[:len(blob)-trailerSize]...)
	c[blobHeader+NonceSize] ^= 1
	c = appendTrailer(c)
	if VerifyBlob(c) != nil {
		t.Fatal("trailer should verify after recompute")
	}
	if _, err := k.DecodeChunk(c, id); err == nil {
		t.Fatal("AEAD accepted modified ciphertext")
	}
}

func TestChunkWrongKeyAndWrongID(t *testing.T) {
	k1, _ := DeriveKeys(fixedMaster())
	other := fixedMaster()
	other[0] ^= 0xff
	k2, _ := DeriveKeys(other)
	id, blob, _ := k1.EncodeChunk([]byte("data"), EncodeOpts{})
	if _, err := k2.DecodeChunk(blob, id); err == nil {
		t.Fatal("wrong key accepted")
	}
	id2, _, _ := k1.EncodeChunk([]byte("other"), EncodeOpts{})
	if _, err := k1.DecodeChunk(blob, id2); err == nil {
		t.Fatal("blob accepted under a different chunk id (swap attack)")
	}
	if _, err := k1.DecodeChunk(blob, ZeroID); err == nil {
		t.Fatal("zero id accepted")
	}
	if k1.DomainID == k2.DomainID {
		t.Fatal("domain ids collide")
	}
}

func TestChunkIDIsKeyedAndDeterministic(t *testing.T) {
	k1, _ := DeriveKeys(fixedMaster())
	other := fixedMaster()
	other[5] ^= 1
	k2, _ := DeriveKeys(other)
	a, b := k1.ChunkID([]byte("same")), k1.ChunkID([]byte("same"))
	if a != b {
		t.Fatal("id not deterministic")
	}
	if a == k2.ChunkID([]byte("same")) {
		t.Fatal("id not keyed")
	}
}

func TestDecompressionBombRejected(t *testing.T) {
	k, _ := DeriveKeys(fixedMaster())
	big := make([]byte, MaxChunkSize+1)
	id, blob, err := k.EncodeChunk(big, EncodeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.DecodeChunk(blob, id); err == nil {
		t.Fatal("oversized plaintext accepted")
	}
}

func TestIndexRoundTripAndValidation(t *testing.T) {
	x := NewFixedIndex(10*1024*1024+5, FixedChunkSize)
	if len(x.Entries) != 3 {
		t.Fatalf("entries=%d", len(x.Entries))
	}
	k, _ := DeriveKeys(fixedMaster())
	x.Entries[1].ID = k.ChunkID([]byte("a"))
	b, err := x.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	y, err := ParseIndex(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(y.Entries) != 3 || y.Entries[1].ID != x.Entries[1].ID || y.Size != x.Size {
		t.Fatal("round trip mismatch")
	}
	if off, n := y.Span(2); off != 8*1024*1024 || n != 2*1024*1024+5 {
		t.Fatalf("span %d %d", off, n)
	}
	if ids := y.ChunkIDs(); len(ids) != 1 {
		t.Fatalf("ChunkIDs=%v", ids)
	}
	// every byte flip must be rejected
	for i := range b {
		c := append([]byte{}, b...)
		c[i] ^= 0x80
		if _, err := ParseIndex(c); err == nil {
			t.Fatalf("flip at %d accepted", i)
		}
	}
	bad := &Index{Kind: IndexFixed, ChunkSize: 16, Size: 32, Entries: []Entry{{End: 20}, {End: 32}}}
	if _, err := bad.Marshal(); err == nil {
		t.Fatal("misaligned fixed index accepted")
	}
	bad = &Index{Kind: IndexDynamic, Size: 10, Entries: []Entry{{End: 6}, {End: 5}}}
	if _, err := bad.Marshal(); err == nil {
		t.Fatal("non-monotonic dynamic index accepted")
	}
	if _, err := (&Index{Kind: IndexFixed, ChunkSize: 4}).Marshal(); err != nil {
		t.Fatalf("empty object index should be valid: %v", err)
	}
}

func FuzzParseIndex(f *testing.F) {
	x := NewFixedIndex(9<<20, FixedChunkSize)
	b, _ := x.Marshal()
	f.Add(b)
	f.Add([]byte("IBIX"))
	f.Fuzz(func(t *testing.T, in []byte) {
		x, err := ParseIndex(in)
		if err != nil {
			return
		}
		out, err := x.Marshal()
		if err != nil || !bytes.Equal(out, in) {
			t.Fatalf("accepted input does not re-marshal identically: %v", err)
		}
	})
}

func FuzzDecodeChunk(f *testing.F) {
	k, _ := DeriveKeys(fixedMaster())
	id, blob, _ := k.EncodeChunk([]byte("fuzz seed"), EncodeOpts{})
	f.Add(blob)
	f.Fuzz(func(t *testing.T, in []byte) {
		_, _ = k.DecodeChunk(in, id) // must never panic
		_ = VerifyBlob(in)
	})
}

func TestManifestSealOpen(t *testing.T) {
	k, _ := DeriveKeys(fixedMaster())
	g := k.GroupID("p", "block", "vm/root")
	m := &Manifest{SnapshotID: "20261007230000-0a1b2c3d", Created: time.Now().UTC().Truncate(time.Second), Host: "h", Object: ObjectInfo{Name: "vm/root", Kind: "block", Size: 5}}
	b, err := k.SealManifest(m, g, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := k.OpenManifest(b, g, m.SnapshotID)
	if err != nil || got.Host != "h" || got.Object.Name != "vm/root" {
		t.Fatalf("open: %v %+v", err, got)
	}
	if _, err := k.OpenManifest(b, k.GroupID("p", "block", "vm/other"), m.SnapshotID); err == nil {
		t.Fatal("manifest accepted under a different group (relocation attack)")
	}
	if _, err := k.OpenManifest(b, g, "20261007230001-0a1b2c3d"); err == nil {
		t.Fatal("manifest accepted under a different snapshot id")
	}
	for i := range b {
		c := append([]byte{}, b...)
		c[i] ^= 1
		if _, err := k.OpenManifest(c, g, m.SnapshotID); err == nil {
			t.Fatalf("tamper at %d accepted", i)
		}
	}
	if strings.Contains(string(b), "vm/root") {
		t.Fatal("manifest leaks the object name in plaintext")
	}
}

func TestRecoveryKey(t *testing.T) {
	m := fixedMaster()
	s, err := EncodeRecoveryKey(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, "IBR1-") {
		t.Fatal(s)
	}
	got, err := DecodeRecoveryKey(strings.ToLower(strings.ReplaceAll(s, "-", " ")))
	if err != nil || !bytes.Equal(got, m) {
		t.Fatalf("decode: %v", err)
	}
	// any single character change must be caught by the checksum or the alphabet
	for i := 5; i < len(s); i++ {
		if s[i] == '-' {
			continue
		}
		c := []byte(s)
		if c[i] == 'A' {
			c[i] = 'B'
		} else {
			c[i] = 'A'
		}
		if _, err := DecodeRecoveryKey(string(c)); err == nil {
			t.Fatalf("typo at %d accepted", i)
		}
	}
	if _, err := DecodeRecoveryKey("garbage"); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestKeyFile(t *testing.T) {
	m := fixedMaster()
	p := Argon2Params{Time: 1, MemKiB: 1024, Threads: 1}
	kf, err := NewKeyFile(m, "correct horse", p, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := kf.Marshal()
	if bytes.Contains(b, []byte(hex.EncodeToString(m))) {
		t.Fatal("master key present in clear")
	}
	kf2, err := ParseKeyFile(b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := kf2.Unwrap("correct horse")
	if err != nil || !bytes.Equal(got, m) {
		t.Fatalf("unwrap: %v", err)
	}
	if _, err := kf2.Unwrap("wrong"); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	if _, err := kf2.Unwrap(""); err == nil {
		t.Fatal("empty passphrase accepted for protected file")
	}
	// moving the wrapped key into another domain must fail (AAD binds domain id)
	kf2.DomainID = "0000000000000000"
	if _, err := kf2.Unwrap("correct horse"); err == nil {
		t.Fatal("domain id tamper accepted")
	}
	none, _ := NewKeyFile(m, "", p, nil, nil)
	got, err = none.Unwrap("")
	if err != nil || !bytes.Equal(got, m) {
		t.Fatalf("unprotected: %v", err)
	}
}

// A damaged or hostile key file is an error, never a panic or a huge allocation (review F11).
func TestKeyFileUnwrapRejectsBadStoredFields(t *testing.T) {
	m := fixedMaster()
	p := Argon2Params{Time: 1, MemKiB: 1024, Threads: 1}
	fresh := func() *KeyFile {
		kf, err := NewKeyFile(m, "correct horse", p, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return kf
	}
	cases := map[string]func(kf *KeyFile){
		"short nonce":  func(kf *KeyFile) { kf.Nonce = base64.StdEncoding.EncodeToString(make([]byte, NonceSize-1)) },
		"long nonce":   func(kf *KeyFile) { kf.Nonce = base64.StdEncoding.EncodeToString(make([]byte, NonceSize+1)) },
		"empty nonce":  func(kf *KeyFile) { kf.Nonce = "" },
		"zero threads": func(kf *KeyFile) { kf.Argon2.Threads = 0 },
		"zero time":    func(kf *KeyFile) { kf.Argon2.Time = 0 },
		"huge time":    func(kf *KeyFile) { kf.Argon2.Time = 1 << 30 },
		"huge memory":  func(kf *KeyFile) { kf.Argon2.MemKiB = 1 << 31 },
		"tiny memory":  func(kf *KeyFile) { kf.Argon2.MemKiB = 1 },
		"many threads": func(kf *KeyFile) { kf.Argon2.Threads = 255 },
	}
	for name, mutate := range cases {
		kf := fresh()
		mutate(kf)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic: %v", name, r)
				}
			}()
			if _, err := kf.Unwrap("correct horse"); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}()
	}
	if _, err := NewKeyFile(m, "x", Argon2Params{}, nil, nil); err == nil {
		t.Error("NewKeyFile accepted zero Argon2 parameters")
	}
	if got, err := fresh().Unwrap("correct horse"); err != nil || !bytes.Equal(got, m) {
		t.Fatalf("a valid file still opens: %v", err)
	}
}

func TestSnapshotID(t *testing.T) {
	id, err := NewSnapshotID(time.Date(2026, 10, 7, 23, 0, 0, 0, time.UTC), bytes.NewReader([]byte{0xde, 0xad, 0xbe, 0xef}))
	if err != nil || id != "20261007230000-deadbeef" || !ValidSnapshotID(id) {
		t.Fatalf("%q %v", id, err)
	}
	for _, bad := range []string{"", "x", "20261007230000_deadbeef", "20261007230000-DEADBEEF", "../../etc/passwd", "20261007230000-deadbee"} {
		if ValidSnapshotID(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// "latest snapshot" is decided by sorting ids, so ids made in the same second
// must sort in creation order.
func TestSnapshotIDsSortInCreationOrderWithinASecond(t *testing.T) {
	base := time.Date(2026, 10, 7, 23, 0, 0, 0, time.UTC)
	prev := ""
	for i := 0; i < 2000; i++ {
		id, err := NewSnapshotID(base.Add(time.Duration(i)*time.Microsecond*3), nil)
		if err != nil || !ValidSnapshotID(id) {
			t.Fatalf("%q %v", id, err)
		}
		if id <= prev {
			t.Fatalf("id %q does not sort after %q", id, prev)
		}
		prev = id
	}
}
