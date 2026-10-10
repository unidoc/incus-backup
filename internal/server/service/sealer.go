package service

// AuxSealer seals small server-side secrets that live in their own tables (the TOTP seeds of the
// management users) with the same secret-store key as the credentials, bound to a purpose string so
// a sealed value cannot be moved to another use.
type AuxSealer struct {
	S       *Service
	Purpose string
}

// Sealer returns a sealer for purpose.
func (s *Service) Sealer(purpose string) *AuxSealer { return &AuxSealer{S: s, Purpose: purpose} }

// Seal encrypts plain.
func (a *AuxSealer) Seal(plain []byte) ([]byte, error) { return a.S.seal(a.Purpose, plain) }

// Open decrypts a value sealed by Seal.
func (a *AuxSealer) Open(sealed []byte) ([]byte, error) { return a.S.unseal(a.Purpose, sealed) }
