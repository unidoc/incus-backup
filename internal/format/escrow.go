package format

import (
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Offline escrow of the master key.
//
// An escrow key pair is an X25519 key pair generated once, whose PRIVATE half lives offline (a
// safe, a hardware token, a sealed envelope). The master key is wrapped to the PUBLIC half
// (ephemeral-static ECDH, HKDF-SHA256, XChaCha20-Poly1305) and the wrapped blob can be stored
// anywhere, including on the backup servers: nobody who lacks the private half can open it, so
// the zero-knowledge boundary does not move. Disaster recovery then needs only the offline
// private key and read access to the repository, not the host or a paper recovery key.
//
//	wrapped = "IBW1" | domain id (16 ascii) | ephemeral public (32) | nonce (24) | AEAD(master) (32+16)
//	aad     = "incus-backup escrow v1" | domain id | recipient public | ephemeral public
const (
	escrowMagic      = "IBW1"
	escrowPrivPrefix = "IBE1" // printable private key
	escrowPubPrefix  = "IBP1" // printable public key
	domainLen        = 16
)

// GenerateEscrowKeyPair returns a printable private key (keep offline) and public key.
func GenerateEscrowKeyPair() (priv, pub string, err error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return encodeChecked(escrowPrivPrefix, k.Bytes()), encodeChecked(escrowPubPrefix, k.PublicKey().Bytes()), nil
}

func encodeChecked(prefix string, raw []byte) string {
	sum := sha256.Sum256(append([]byte("incus-backup escrow "+prefix), raw...))
	s := b32.EncodeToString(append(append([]byte{}, raw...), sum[:4]...))
	parts := []string{prefix}
	for len(s) > 0 {
		n := 5
		if len(s) < n {
			n = len(s)
		}
		parts = append(parts, s[:n])
		s = s[n:]
	}
	return strings.Join(parts, "-")
}

func decodeChecked(prefix, s string, want int) ([]byte, error) {
	s = strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "\n", "", "\t", "").Replace(s))
	if !strings.HasPrefix(s, prefix) {
		return nil, fmt.Errorf("expected a key starting with %s", prefix)
	}
	raw, err := b32.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil || len(raw) != want+4 {
		return nil, errors.New("not a valid key (wrong length or characters)")
	}
	body, sum := raw[:want], raw[want:]
	h := sha256.Sum256(append([]byte("incus-backup escrow "+prefix), body...))
	if !hmac.Equal(sum, h[:4]) {
		return nil, errors.New("key checksum mismatch (typo?)")
	}
	return body, nil
}

func escrowKey(shared, eph, recipient []byte) ([]byte, error) {
	r := hkdf.New(sha256.New, shared, append(append([]byte{}, eph...), recipient...), []byte("incus-backup escrow key v1"))
	k := make([]byte, chacha20poly1305.KeySize)
	_, err := io.ReadFull(r, k)
	return k, err
}

func escrowAAD(domain string, recipient, eph []byte) []byte {
	aad := []byte("incus-backup escrow v1")
	aad = append(aad, domain...)
	aad = append(aad, recipient...)
	return append(aad, eph...)
}

// WrapMasterKey wraps master to the escrow public key.
func WrapMasterKey(master []byte, pub string) ([]byte, error) {
	if len(master) != KeySize {
		return nil, errors.New("master key has wrong size")
	}
	pb, err := decodeChecked(escrowPubPrefix, pub, 32)
	if err != nil {
		return nil, fmt.Errorf("escrow public key: %w", err)
	}
	rpub, err := ecdh.X25519().NewPublicKey(pb)
	if err != nil {
		return nil, err
	}
	keys, err := DeriveKeys(master)
	if err != nil {
		return nil, err
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := eph.ECDH(rpub)
	if err != nil {
		return nil, err
	}
	k, err := escrowKey(shared, eph.PublicKey().Bytes(), pb)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte(escrowMagic), keys.DomainID...)
	out = append(out, eph.PublicKey().Bytes()...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, master, escrowAAD(keys.DomainID, pb, eph.PublicKey().Bytes())), nil
}

// WrappedDomain returns the key domain id a wrapped blob claims to hold (not authenticated until unwrapped).
func WrappedDomain(wrapped []byte) (string, error) {
	if len(wrapped) < len(escrowMagic)+domainLen || string(wrapped[:4]) != escrowMagic {
		return "", errors.New("not an escrow blob")
	}
	return string(wrapped[4 : 4+domainLen]), nil
}

// UnwrapMasterKey opens a wrapped blob with the escrow private key and checks that the master key it
// holds belongs to the key domain the blob names.
func UnwrapMasterKey(wrapped []byte, priv string) (master []byte, domain string, err error) {
	domain, err = WrappedDomain(wrapped)
	if err != nil {
		return nil, "", err
	}
	rest := wrapped[4+domainLen:]
	if len(rest) != 32+chacha20poly1305.NonceSizeX+KeySize+chacha20poly1305.Overhead {
		return nil, "", errors.New("escrow blob has the wrong length")
	}
	sb, err := decodeChecked(escrowPrivPrefix, priv, 32)
	if err != nil {
		return nil, "", fmt.Errorf("escrow private key: %w", err)
	}
	sk, err := ecdh.X25519().NewPrivateKey(sb)
	if err != nil {
		return nil, "", err
	}
	eph, err := ecdh.X25519().NewPublicKey(rest[:32])
	if err != nil {
		return nil, "", err
	}
	shared, err := sk.ECDH(eph)
	if err != nil {
		return nil, "", err
	}
	pb := sk.PublicKey().Bytes()
	k, err := escrowKey(shared, rest[:32], pb)
	if err != nil {
		return nil, "", err
	}
	aead, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, "", err
	}
	nonce := rest[32 : 32+aead.NonceSize()]
	master, err = aead.Open(nil, nonce, rest[32+aead.NonceSize():], escrowAAD(domain, pb, rest[:32]))
	if err != nil {
		return nil, "", errors.New("this escrow private key does not open the blob (wrong key, or the blob was altered)")
	}
	keys, err := DeriveKeys(master)
	if err != nil || keys.DomainID != domain {
		return nil, "", errors.New("the unwrapped key does not match the key domain the blob names")
	}
	return master, domain, nil
}

// EscrowPublicOf returns the public key matching a private key.
func EscrowPublicOf(priv string) (string, error) {
	sb, err := decodeChecked(escrowPrivPrefix, priv, 32)
	if err != nil {
		return "", err
	}
	sk, err := ecdh.X25519().NewPrivateKey(sb)
	if err != nil {
		return "", err
	}
	return encodeChecked(escrowPubPrefix, sk.PublicKey().Bytes()), nil
}
