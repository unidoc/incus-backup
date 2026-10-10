package engine

import (
	"os"
	"path/filepath"

	"github.com/unidoc/incus-backup/internal/format"
)

// Spool holds encrypted chunk blobs on local disk between capture and upload.
// Capture must finish while an NBD freeze is held; uploading (slow, over a
// network) happens afterwards from the spool, so the freeze stays short.
type Spool struct {
	dir string
}

// NewSpool creates a private spool directory under base.
func NewSpool(base string) (*Spool, error) {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	d, err := os.MkdirTemp(base, "run-*")
	if err != nil {
		return nil, err
	}
	return &Spool{dir: d}, nil
}

func (s *Spool) path(domain string, id format.ID) string {
	return filepath.Join(s.dir, domain, id.String())
}

// Put stores a blob (idempotent).
func (s *Spool) Put(domain string, id format.ID, blob []byte) error {
	p := s.path(domain, id)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Get returns a stored blob.
func (s *Spool) Get(domain string, id format.ID) ([]byte, error) {
	return os.ReadFile(s.path(domain, id))
}

// Has reports whether a blob is spooled.
func (s *Spool) Has(domain string, id format.ID) bool {
	_, err := os.Stat(s.path(domain, id))
	return err == nil
}

// Close removes the spool directory.
func (s *Spool) Close() { _ = os.RemoveAll(s.dir) }
