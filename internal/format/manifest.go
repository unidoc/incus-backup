package format

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// Manifest is the encrypted description of one backup snapshot. The server
// stores it opaquely; only hosts holding the key can read it.
type Manifest struct {
	Format      int               `json:"format"`
	SnapshotID  string            `json:"snapshot_id"`
	PreviousID  string            `json:"previous_id,omitempty"` // snapshot this one was derived from (generation chain)
	Created     time.Time         `json:"created"`
	Host        string            `json:"host"`
	Project     string            `json:"project"`
	Policy      string            `json:"policy"`
	Object      ObjectInfo        `json:"object"`
	IndexDigest string            `json:"index_digest"`
	Config      json.RawMessage   `json:"config,omitempty"` // instance/volume configuration as read from Incus
	Refs        []string          `json:"refs,omitempty"`   // for instance manifests: storage objects referenced
	Meta        map[string]string `json:"meta,omitempty"`
	Stats       Stats             `json:"stats"`
}

// ObjectInfo identifies the backed up storage object.
type ObjectInfo struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // "block", "filesystem" or "instance"
	Size uint64 `json:"size"`
}

// Stats records what a backup run did.
type Stats struct {
	LogicalBytes  uint64 `json:"logical_bytes"`
	DirtyBytes    uint64 `json:"dirty_bytes"`
	ReadBytes     uint64 `json:"read_bytes"`
	UploadedBytes uint64 `json:"uploaded_bytes"`
	FullRead      bool   `json:"full_read"`
	DurationMs    int64  `json:"duration_ms"`
}

// Manifest blob layout, format version 1:
//
//	magic "IBMF" | version 1 | reserved 3 | nonce 24 |
//	XChaCha20-Poly1305(k_manifest, nonce, json, aad) | trailer SHA-256 (32)
//
// aad = magic | version | reserved | group id | 0x00 | snapshot id
const (
	manifestMagic   = "IBMF"
	manifestVersion = 1
)

func manifestAAD(group, snapshot string) []byte {
	aad := []byte(manifestMagic)
	aad = append(aad, manifestVersion, 0, 0, 0)
	aad = append(aad, group...)
	aad = append(aad, 0)
	aad = append(aad, snapshot...)
	return aad
}

// GroupID returns the opaque, key-dependent group identifier of an object.
// The datastore directory carries this instead of names.
func (k *Keys) GroupID(project, kind, name string) string {
	m := hmac.New(sha256.New, k.ID[:])
	m.Write([]byte("group\x00" + project + "\x00" + kind + "\x00" + name))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// SealManifest encrypts the manifest for the given group. nonce may be nil.
func (k *Keys) SealManifest(m *Manifest, group string, nonce []byte) ([]byte, error) {
	m.Format = 1
	pt, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if nonce == nil {
		nonce = make([]byte, NonceSize)
		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			return nil, err
		}
	}
	aead, err := chacha20poly1305.NewX(k.Manifest[:])
	if err != nil {
		return nil, err
	}
	out := []byte(manifestMagic)
	out = append(out, manifestVersion, 0, 0, 0)
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, pt, manifestAAD(group, m.SnapshotID))
	return appendTrailer(out), nil
}

// OpenManifest decrypts and parses a manifest blob.
func (k *Keys) OpenManifest(blob []byte, group, snapshot string) (*Manifest, error) {
	body, err := checkTrailer(blob)
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if len(body) < 8+NonceSize+chacha20poly1305.Overhead || string(body[:4]) != manifestMagic {
		return nil, errors.New("manifest: bad magic or length")
	}
	if body[4] != manifestVersion {
		return nil, fmt.Errorf("manifest: unsupported version %d", body[4])
	}
	nonce := body[8 : 8+NonceSize]
	ct := body[8+NonceSize:]
	aead, err := chacha20poly1305.NewX(k.Manifest[:])
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, nonce, ct, manifestAAD(group, snapshot))
	if err != nil {
		return nil, errors.New("manifest authentication failed (wrong key, group, snapshot or corruption)")
	}
	m := &Manifest{}
	if err := json.Unmarshal(pt, m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.Format != 1 || m.SnapshotID != snapshot {
		return nil, errors.New("manifest: content does not match its location")
	}
	return m, nil
}

// NewSnapshotID returns a sortable id: YYYYMMDDHHMMSS-xxxxxxxx (UTC). It only
// uses [0-9a-f-] so it is safe in file names and bitmap names.
//
// With r == nil (production) the suffix is the microsecond within the second
// followed by one random byte, so ids created in the same second still sort in
// creation order ("latest snapshot" is decided by sorting ids). With an explicit
// reader (tests) the suffix is 4 bytes read from it.
func NewSnapshotID(t time.Time, r io.Reader) (string, error) {
	t = t.UTC()
	var b [4]byte
	if r == nil {
		var rb [1]byte
		if _, err := io.ReadFull(rand.Reader, rb[:]); err != nil {
			return "", err
		}
		v := uint32(t.Nanosecond()/1000)<<8 | uint32(rb[0])
		b = [4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	} else if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", err
	}
	return t.Format("20060102150405") + "-" + hex.EncodeToString(b[:]), nil
}

// ValidSnapshotID reports whether s looks like an id from NewSnapshotID.
func ValidSnapshotID(s string) bool {
	if len(s) != 14+1+8 || s[14] != '-' {
		return false
	}
	for i, c := range []byte(s) {
		if i == 14 {
			continue
		}
		if !(c >= '0' && c <= '9') && !(i > 14 && c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
