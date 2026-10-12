package datastore

import (
	"bytes"
	"crypto/sha256"
	"errors"
)

func trailerOK(b []byte) error {
	body, tr := b[:len(b)-sha256.Size], b[len(b)-sha256.Size:]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], tr) {
		return errors.New("trailer digest mismatch")
	}
	return nil
}
