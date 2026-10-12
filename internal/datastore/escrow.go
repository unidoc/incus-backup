package datastore

import (
	"errors"
)

// Escrow blobs are the master key wrapped to an OFFLINE public key (format.WrapMasterKey). The
// server can neither open nor use them; they are stored so that disaster recovery needs only the
// offline private key and an enrolled host. One blob per key domain, create-if-absent: a
// compromised host cannot silently replace it; an administrator clears it locally.

const maxEscrowBytes = 4096

func escrowKey(domain string) string { return "escrow/" + domain + ".ibw" }

func validDomain(d string) error {
	if len(d) != 16 {
		return errors.New("invalid key domain id")
	}
	for _, c := range []byte(d) {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return errors.New("invalid key domain id")
		}
	}
	return nil
}

// PutEscrow stores the blob for a domain; ErrExists if one is already there.
func (s *Store) PutEscrow(domain string, blob []byte) error {
	if err := validDomain(domain); err != nil {
		return err
	}
	if len(blob) == 0 || len(blob) > maxEscrowBytes {
		return errors.New("escrow blob has an implausible size")
	}
	return s.b.Put(escrowKey(domain), blob, true)
}

// GetEscrow returns the blob for a domain (ErrNotFound if none).
func (s *Store) GetEscrow(domain string) ([]byte, error) {
	if err := validDomain(domain); err != nil {
		return nil, err
	}
	return s.b.Get(escrowKey(domain))
}

// DeleteEscrow removes the blob (admin only).
func (s *Store) DeleteEscrow(domain string) error {
	if err := validDomain(domain); err != nil {
		return err
	}
	return s.b.Delete(escrowKey(domain))
}
