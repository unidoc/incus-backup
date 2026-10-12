package jobspec

import (
	"crypto/sha256"
)

// RequestDigest is the digest a sealed delivery (format.SealBinding, "IBS2") binds: SHA-256 over a
// domain tag, the job type and the CANONICAL parameters (Validate: paths cleaned, defaults applied).
// The browser computes it from the parameters it sent (the same Go code, in WebAssembly), the host
// from the parameters it received; when the server changes anything (another object, snapshot,
// paths or recipient key), the digests differ and the delivery does not authenticate in the browser.
// Validate is idempotent on its own output, so the server re-canonicalising the parameters it
// stores does not change the digest.
func RequestDigest(typ string, raw []byte) ([]byte, error) {
	_, canon, err := Validate(typ, raw)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte("incus-backup job request v1\x00"))
	h.Write([]byte(typ))
	h.Write([]byte{0})
	h.Write(canon)
	return h.Sum(nil), nil
}
