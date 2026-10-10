// Package format implements the byte-level repository formats of incus-backup:
// key derivation, chunk blobs, indexes and manifests. It is specified in
// docs/format.md and every construction here has deterministic test vectors.
//
// Only audited primitives from golang.org/x/crypto and the standard library are
// used: HKDF-SHA256, HMAC-SHA256, XChaCha20-Poly1305, Argon2id, SHA-256.
package format

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// KeySize is the size of every key in bytes.
	KeySize = 32
	// NonceSize is the XChaCha20-Poly1305 nonce size.
	NonceSize = chacha20poly1305.NonceSizeX
	// IDSize is the size of a chunk id (HMAC-SHA256 output).
	IDSize = sha256.Size

	hkdfSalt = "incus-backup/v1"
)

// ID is a keyed chunk identifier.
type ID [IDSize]byte

// ZeroID marks an all-zero chunk that has no blob.
var ZeroID ID

// String returns the lowercase hex form.
func (i ID) String() string { return hex.EncodeToString(i[:]) }

// IsZero reports whether the id is the zero-chunk marker.
func (i ID) IsZero() bool { return i == ZeroID }

// ParseID parses a 64-character hex id.
func ParseID(s string) (ID, error) {
	var id ID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != IDSize {
		return id, fmt.Errorf("invalid chunk id %q", s)
	}
	copy(id[:], b)
	return id, nil
}

// Keys holds the sub-keys of one repository key domain.
type Keys struct {
	Enc      [KeySize]byte // chunk blob encryption
	ID       [KeySize]byte // keyed chunk identity (HMAC)
	Manifest [KeySize]byte // manifest encryption
	Delivery [KeySize]byte // authenticates sealed deliveries (sealedbox.go, IBS2)
	// DomainID is a non-secret identifier of the key domain. Servers store it
	// so that hosts holding a different master key are rejected.
	DomainID string
}

// GenerateMaster returns a fresh random master key.
func GenerateMaster() ([]byte, error) {
	m := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, m); err != nil {
		return nil, err
	}
	return m, nil
}

func derive(master []byte, info string) ([KeySize]byte, error) {
	var out [KeySize]byte
	r := hkdf.New(sha256.New, master, []byte(hkdfSalt), []byte(info))
	if _, err := io.ReadFull(r, out[:]); err != nil {
		return out, err
	}
	return out, nil
}

// DeriveKeys derives all sub-keys from the 32-byte master key.
func DeriveKeys(master []byte) (*Keys, error) {
	if len(master) != KeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", KeySize, len(master))
	}
	k := &Keys{}
	var err error
	if k.Enc, err = derive(master, "chunk-encryption"); err != nil {
		return nil, err
	}
	if k.ID, err = derive(master, "chunk-id"); err != nil {
		return nil, err
	}
	if k.Manifest, err = derive(master, "manifest-encryption"); err != nil {
		return nil, err
	}
	if k.Delivery, err = derive(master, "delivery-auth"); err != nil {
		return nil, err
	}
	d, err := derive(master, "domain-id")
	if err != nil {
		return nil, err
	}
	k.DomainID = hex.EncodeToString(d[:8])
	return k, nil
}

// ChunkID returns the keyed id of a plaintext chunk: HMAC-SHA256(k_id, plaintext).
func (k *Keys) ChunkID(plaintext []byte) ID {
	m := hmac.New(sha256.New, k.ID[:])
	m.Write(plaintext)
	var id ID
	copy(id[:], m.Sum(nil))
	return id
}

// ---- recovery key --------------------------------------------------------

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

const recoveryPrefix = "IBR1"

func recoveryChecksum(master []byte) []byte {
	h := sha256.Sum256(append([]byte("incus-backup recovery key v1"), master...))
	return h[:4]
}

// EncodeRecoveryKey renders the master key as a printable string with a checksum:
// "IBR1-XXXXX-XXXXX-...". Groups are 5 characters.
func EncodeRecoveryKey(master []byte) (string, error) {
	if len(master) != KeySize {
		return "", errors.New("master key has wrong size")
	}
	raw := append(append([]byte{}, master...), recoveryChecksum(master)...)
	s := b32.EncodeToString(raw)
	var parts []string
	parts = append(parts, recoveryPrefix)
	for len(s) > 0 {
		n := 5
		if len(s) < n {
			n = len(s)
		}
		parts = append(parts, s[:n])
		s = s[n:]
	}
	return strings.Join(parts, "-"), nil
}

// DecodeRecoveryKey parses and checksum-verifies a recovery key string.
func DecodeRecoveryKey(s string) ([]byte, error) {
	s = strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "\n", "", "\t", "").Replace(s))
	if !strings.HasPrefix(s, recoveryPrefix) {
		return nil, errors.New("recovery key must start with IBR1")
	}
	raw, err := b32.DecodeString(strings.TrimPrefix(s, recoveryPrefix))
	if err != nil {
		return nil, fmt.Errorf("recovery key is not valid: %w", err)
	}
	if len(raw) != KeySize+4 {
		return nil, errors.New("recovery key has the wrong length")
	}
	master, sum := raw[:KeySize], raw[KeySize:]
	if !hmac.Equal(sum, recoveryChecksum(master)) {
		return nil, errors.New("recovery key checksum mismatch (typo?)")
	}
	return append([]byte{}, master...), nil
}

// ---- key file ------------------------------------------------------------

// KeyFile is the on-disk representation of the master key.
type KeyFile struct {
	Version    int    `json:"version"`
	Protection string `json:"protection"` // "passphrase" or "none"
	DomainID   string `json:"domain_id"`
	// passphrase protection
	Argon2 *Argon2Params `json:"argon2,omitempty"`
	Nonce  string        `json:"nonce,omitempty"`   // base64
	Wrap   string        `json:"wrapped,omitempty"` // base64, AEAD(master)
	// no protection (file permissions only)
	Master string `json:"master,omitempty"` // base64
}

// Argon2Params are the Argon2id parameters stored in the key file.
type Argon2Params struct {
	Time    uint32 `json:"time"`
	MemKiB  uint32 `json:"memory_kib"`
	Threads uint8  `json:"threads"`
	Salt    string `json:"salt"` // base64
}

// DefaultArgon2 returns production parameters (64 MiB, 3 passes).
func DefaultArgon2() Argon2Params {
	return Argon2Params{Time: 3, MemKiB: 64 * 1024, Threads: 4}
}

func keyFileAAD(domainID string) []byte { return []byte("incus-backup keyfile v1|" + domainID) }

// NewKeyFile wraps master. An empty passphrase produces an unprotected file
// that relies on file permissions (needed for unattended schedulers).
// salt and nonce may be nil to use random values (tests inject fixed ones).
func NewKeyFile(master []byte, passphrase string, p Argon2Params, salt, nonce []byte) (*KeyFile, error) {
	k, err := DeriveKeys(master)
	if err != nil {
		return nil, err
	}
	kf := &KeyFile{Version: 1, DomainID: k.DomainID}
	if passphrase == "" {
		kf.Protection = "none"
		kf.Master = base64.StdEncoding.EncodeToString(master)
		return kf, nil
	}
	if salt == nil {
		salt = make([]byte, 16)
		if _, err := io.ReadFull(rand.Reader, salt); err != nil {
			return nil, err
		}
	}
	if nonce == nil {
		nonce = make([]byte, NonceSize)
		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			return nil, err
		}
	}
	p.Salt = base64.StdEncoding.EncodeToString(salt)
	kek := argon2.IDKey([]byte(passphrase), salt, p.Time, p.MemKiB, p.Threads, KeySize)
	aead, err := chacha20poly1305.NewX(kek)
	if err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, master, keyFileAAD(k.DomainID))
	kf.Protection = "passphrase"
	kf.Argon2 = &p
	kf.Nonce = base64.StdEncoding.EncodeToString(nonce)
	kf.Wrap = base64.StdEncoding.EncodeToString(ct)
	return kf, nil
}

// Marshal returns the JSON form.
func (kf *KeyFile) Marshal() ([]byte, error) { return json.MarshalIndent(kf, "", "  ") }

// ParseKeyFile parses a key file.
func ParseKeyFile(b []byte) (*KeyFile, error) {
	kf := &KeyFile{}
	if err := json.Unmarshal(b, kf); err != nil {
		return nil, fmt.Errorf("parsing key file: %w", err)
	}
	if kf.Version != 1 {
		return nil, fmt.Errorf("unsupported key file version %d", kf.Version)
	}
	return kf, nil
}

// Unwrap returns the master key. It verifies that the key matches DomainID.
func (kf *KeyFile) Unwrap(passphrase string) ([]byte, error) {
	var master []byte
	switch kf.Protection {
	case "none":
		m, err := base64.StdEncoding.DecodeString(kf.Master)
		if err != nil {
			return nil, err
		}
		master = m
	case "passphrase":
		if kf.Argon2 == nil {
			return nil, errors.New("key file is missing argon2 parameters")
		}
		if passphrase == "" {
			return nil, errors.New("key file is passphrase protected: passphrase required")
		}
		salt, err := base64.StdEncoding.DecodeString(kf.Argon2.Salt)
		if err != nil {
			return nil, err
		}
		nonce, err := base64.StdEncoding.DecodeString(kf.Nonce)
		if err != nil {
			return nil, err
		}
		ct, err := base64.StdEncoding.DecodeString(kf.Wrap)
		if err != nil {
			return nil, err
		}
		kek := argon2.IDKey([]byte(passphrase), salt, kf.Argon2.Time, kf.Argon2.MemKiB, kf.Argon2.Threads, KeySize)
		aead, err := chacha20poly1305.NewX(kek)
		if err != nil {
			return nil, err
		}
		master, err = aead.Open(nil, nonce, ct, keyFileAAD(kf.DomainID))
		if err != nil {
			return nil, errors.New("wrong passphrase or corrupted key file")
		}
	default:
		return nil, fmt.Errorf("unknown key file protection %q", kf.Protection)
	}
	k, err := DeriveKeys(master)
	if err != nil {
		return nil, err
	}
	if k.DomainID != kf.DomainID {
		return nil, errors.New("key file domain id does not match its key")
	}
	return master, nil
}
