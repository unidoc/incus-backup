package webcore

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/jobspec"
)

// delivery is what a browser asked for and what the host answers.
type delivery struct {
	job    string
	typ    string
	params []byte
	digest []byte
}

func newDelivery(t *testing.T, job string, pub []byte, paths string) delivery {
	t.Helper()
	p := []byte(`{"project":"default","object":"volume/docs","paths":[` + paths + `],"recipient_public_key":"` + base64.StdEncoding.EncodeToString(pub) + `"}`)
	d, err := RequestDigest("fs_extract", p)
	if err != nil {
		t.Fatal(err)
	}
	return delivery{job: job, typ: "fs_extract", params: p, digest: d}
}

func sealFor(t *testing.T, k *format.Keys, pub []byte, d delivery, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := format.NewSealWriter(&b, k, pub, format.SealBinding{JobID: d.job, Request: d.digest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// unsealAll pushes sealed in pieces of step bytes; it also records the plaintext released BEFORE
// the stream ended, to prove the newest frame is held back.
func unsealAll(c *Core, key, seal int, d delivery, sealed []byte, step int) (out []byte, early int, err error) {
	u, err := c.UnsealOpen(key, seal, d.job, d.digest)
	if err != nil {
		return nil, 0, err
	}
	for i := 0; i < len(sealed); i += step {
		j := min(i+step, len(sealed))
		p, err := c.UnsealPush(u, sealed[i:j])
		if err != nil {
			c.UnsealClose(u)
			return nil, 0, err
		}
		if j < len(sealed) {
			early += len(p)
		}
		out = append(out, p...)
	}
	return out, early, c.UnsealEnd(u)
}

func TestUnsealPushedDeliveries(t *testing.T) {
	c := New()
	m, s := master(t)
	k, _ := format.DeriveKeys(m)
	kh, _, err := c.LoadRecoveryKey(s)
	if err != nil {
		t.Fatal(err)
	}
	sh, pub, err := c.SealNew()
	if err != nil || len(pub) != 32 {
		t.Fatal(err)
	}
	d := newDelivery(t, "job-1", pub, `"docs"`)
	for _, size := range []int{0, 1, 1 << 20, 1<<20 + 1, 2<<20 + 12345} {
		data := make([]byte, size)
		_, _ = rand.Read(data)
		sealed := sealFor(t, k, pub, d, data)
		for _, step := range []int{1 << 30, 65536, 4093, 3} {
			if step == 3 && size > 1<<20+1 {
				continue // byte-dribbling a large stream is slow and adds nothing beyond 4093
			}
			got, early, err := unsealAll(c, kh, sh, d, sealed, step)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("size %d step %d: %v (got %d bytes)", size, step, err, len(got))
			}
			// the last frame (up to 1 MiB) is released only with the trailer
			if size > 0 && early >= size {
				t.Fatalf("size %d step %d: all %d bytes were released before the trailer arrived", size, step, early)
			}
		}
		for _, cut := range []int{0, 10, 36, 40, len(sealed) / 2, len(sealed) - 1} {
			if cut >= len(sealed) {
				continue
			}
			if _, _, err := unsealAll(c, kh, sh, d, sealed[:cut], 4096); err == nil {
				t.Fatalf("size %d: truncation at %d accepted", size, cut)
			}
		}
		if _, _, err := unsealAll(c, kh, sh, d, append(append([]byte{}, sealed...), 0, 0, 0, 0), 4096); err == nil {
			t.Fatalf("size %d: trailing data accepted", size)
		}
		bad := append([]byte{}, sealed...)
		bad[len(bad)-5] ^= 1
		if _, _, err := unsealAll(c, kh, sh, d, bad, 4096); err == nil {
			t.Fatalf("size %d: altered trailer accepted", size)
		}
	}
	if len(c.unseals) != 0 {
		t.Fatalf("%d stream states leaked", len(c.unseals))
	}
}

// What a malicious server can do with everything it knows: the recipient public key, the job
// ids, other jobs' deliveries; but not the repository key.
func TestUnsealRefusesMaliciousServer(t *testing.T) {
	c := New()
	m, s := master(t)
	k, _ := format.DeriveKeys(m)
	kh, _, _ := c.LoadRecoveryKey(s)
	sh, pub, _ := c.SealNew()
	d := newDelivery(t, "job-1", pub, `"docs"`)
	real := sealFor(t, k, pub, d, bytes.Repeat([]byte("genuine "), 300000))
	if _, _, err := unsealAll(c, kh, sh, d, real, 65536); err != nil {
		t.Fatal(err)
	}
	attacker, _ := format.DeriveKeys(bytes.Repeat([]byte{0xee}, 32))
	forged := sealFor(t, attacker, pub, d, []byte("trojan"))
	if got, _, err := unsealAll(c, kh, sh, d, forged, 65536); err == nil || len(got) > 0 {
		t.Fatalf("a forged delivery was accepted or released plaintext: %v %q", err, got)
	}
	// the same request under another job id (replay of job-1's delivery as job-2)
	d2 := d
	d2.job = "job-2"
	if _, _, err := unsealAll(c, kh, sh, d2, real, 65536); err == nil {
		t.Fatal("job-1's delivery accepted as job-2")
	}
	// the server ran ANOTHER request (other paths) with this public key: the browser's digest differs
	other := newDelivery(t, "job-1", pub, `"etc"`)
	forOther := sealFor(t, k, pub, other, []byte("files the user did not ask for"))
	if _, _, err := unsealAll(c, kh, sh, d, forOther, 65536); err == nil {
		t.Fatal("a delivery of other parameters was accepted")
	}
	// an IBS1 stream is refused with a clear message
	v1 := append([]byte("IBS1"), make([]byte, 60)...)
	if _, _, err := unsealAll(c, kh, sh, d, v1, 65536); err == nil || !strings.Contains(err.Error(), "IBS1") {
		t.Fatalf("IBS1: %v", err)
	}
	// a key of another repository (the browser holds the wrong recovery key)
	_, s2 := master(t)
	kh2, _, _ := c.LoadRecoveryKey(s2)
	if _, _, err := unsealAll(c, kh2, sh, d, real, 65536); err == nil {
		t.Fatal("opened with another repository's key")
	}
	// a discarded seal key or a dropped repository key cannot open anything
	c.SealDrop(sh)
	if _, err := c.UnsealOpen(kh, sh, d.job, d.digest); err == nil {
		t.Fatal("a dropped seal key still works")
	}
	c.DropKey(kh)
	sh2, _, _ := c.SealNew()
	if _, err := c.UnsealOpen(kh, sh2, d.job, d.digest); err == nil {
		t.Fatal("a dropped repository key still works")
	}
}

// The digest is the same whether the host computes it from the server's canonical copy or the
// browser from what it sent (Validate is idempotent).
func TestRequestDigestSurvivesServerCanonicalisation(t *testing.T) {
	_, pub, _ := New().SealNew()
	sent := newDelivery(t, "j", pub, `"/docs//2025/./a.txt","Q1/"`)
	_, canon, err := jobspec.Validate("fs_extract", sent.params)
	if err != nil {
		t.Fatal(err)
	}
	host, err := jobspec.RequestDigest("fs_extract", canon)
	if err != nil || !bytes.Equal(host, sent.digest) {
		t.Fatalf("digest of the canonical params differs: %v", err)
	}
	if _, err := RequestDigest("fs_extract", []byte("{")); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

func TestSealImportAndZeroize(t *testing.T) {
	c := New()
	priv := bytes.Repeat([]byte{9}, 32)
	h, pub, err := c.SealImport(priv)
	if err != nil || len(pub) != 32 {
		t.Fatal(err)
	}
	if !bytes.Equal(priv, make([]byte, 32)) {
		t.Fatal("the imported private key bytes were not wiped")
	}
	c.Zeroize()
	if len(c.seals) != 0 {
		t.Fatal("seal keys survive zeroize")
	}
	if _, _, err := c.SealImport(make([]byte, 5)); err == nil {
		t.Fatal("bad key accepted")
	}
	_ = h
}

// W4: a snapshot is accepted only as the object it is. A manifest sealed under another object's
// group (a writer lying about the group) and a group presented under another object's name (a
// server labelling B's snapshots as A's) are both refused.
func TestOpenSnapshotChecksIdentity(t *testing.T) {
	c := New()
	m, s := master(t)
	k, _ := format.DeriveKeys(m)
	h, _, _ := c.LoadRecoveryKey(s)
	gA := k.GroupID("default", "filesystem", "volume/docs")
	gB := k.GroupID("default", "filesystem", "volume/payroll")
	const snap = "20261007120000-00000001"
	manB, idxB, _ := snapshotOf(t, k, gB, snap, "default", "volume/payroll", map[string][]byte{"salaries.csv": []byte("x")})
	// honest: B under B, with and without an expected identity
	if _, err := c.OpenSnapshot(h, gB, snap, manB, idxB, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.OpenSnapshot(h, gB, snap, manB, idxB, &Identity{Project: "default", Kind: "filesystem", Name: "volume/payroll"}); err != nil {
		t.Fatal(err)
	}
	// the server lists group B as object A: the caller expects A and gets B's (genuine) data
	if _, err := c.OpenSnapshot(h, gB, snap, manB, idxB, &Identity{Project: "default", Kind: "filesystem", Name: "volume/docs"}); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("B presented as A: %v", err)
	}
	// B's manifest sealed under A's group (needs the key: a lying or buggy writer)
	swapped, idxS, _ := snapshotOf(t, k, gA, snap, "default", "volume/payroll", map[string][]byte{"salaries.csv": []byte("x")})
	if _, err := c.OpenSnapshot(h, gA, snap, swapped, idxS, nil); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("a manifest of B under A's group: %v", err)
	}
	// B's real manifest served under A's group fails authentication (bound by the AAD)
	if _, err := c.OpenSnapshot(h, gA, snap, manB, idxB, nil); err == nil {
		t.Fatal("B's manifest opened under A's group")
	}
}
