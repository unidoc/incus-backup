package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/unidoc/incus-backup/internal/client/config"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/remote"
)

// InitOptions describe `incus-backup init`.
type InitOptions struct {
	ConfigPath string
	Name       string // local name of the server entry
	Address    string
	Token      string
	Host       string // override the host name
	Passphrase string // protects a NEW key file; empty = unprotected (0600), needed for unattended runs
	// ConfirmRecovery is called with the printable recovery key of a NEW key. It
	// must return nil only after the operator confirmed the key is stored elsewhere.
	ConfirmRecovery func(recovery string) error
	// ExistingPassphrase unlocks an existing key file.
	ExistingPassphrase Passphrase
	// EscrowPublic (IBP1-...) wraps the key to an offline public key and stores the wrapped blob
	// on the server, so the key can be recovered with the offline private key alone.
	EscrowPublic string
	// EscrowPrivate (IBE1-...), for a host WITHOUT a key file, recovers the key from the escrow
	// blob stored on the server instead of creating a new key (disaster recovery).
	EscrowPrivate string
}

// InitResult reports what init did.
type InitResult struct {
	NewKey   bool
	DomainID string
	Server   string
	Repo     string
	Scope    string
	// EscrowStored: a wrapped copy of the key (for the offline key) is on the server.
	EscrowStored bool
	// EscrowExists: the server already held an escrow blob and kept it (it was NOT replaced).
	EscrowExists bool
}

// Init creates (or reuses) the key, enrols at the server and writes the
// configuration. Order matters: the recovery key must be confirmed BEFORE
// anything is written, and enrolment happens before the key can be used to
// back anything up, so a failed init leaves nothing half-configured.
func Init(ctx context.Context, o InitOptions) (*InitResult, error) {
	if o.Address == "" || o.Token == "" {
		return nil, errors.New("--server and --token are required")
	}
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return nil, err
	}
	if o.Host != "" {
		cfg.Host = o.Host
	}
	name := o.Name
	if name == "" {
		name = strings.NewReplacer(".", "-", ":", "-").Replace(strings.Split(o.Address, ":")[0])
	}
	if _, dup := cfg.Servers[name]; dup {
		return nil, fmt.Errorf("a server called %q is already configured", name)
	}
	res := &InitResult{Server: name}
	wroteKey := false // this run created the key file (new or recovered from escrow)

	var keys *format.Keys
	var master []byte
	var recoveredEn *remote.Enrolled
	_, statErr := os.Stat(cfg.KeyFile)
	switch {
	case statErr == nil:
		keys, master, err = LoadKeys(cfg.KeyFile, o.ExistingPassphrase)
		if err != nil {
			return nil, err
		}
	case o.EscrowPrivate != "":
		// Disaster recovery: enrol first (the token is single use), fetch the wrapped key from the
		// server, open it with the offline private key and only then write the key file.
		en, err := remote.Enroll(ctx, o.Address, o.Token, cfg.Host, map[string]any{"client": "incus-backup"})
		if err != nil {
			return nil, fmt.Errorf("enrolling at %s: %w", o.Address, err)
		}
		cl, err := remote.New(o.Address, en.CAPEM, en.CertPEM, en.KeyPEM, "")
		if err != nil {
			return nil, err
		}
		blob, err := cl.GetEscrow(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetching the escrow blob from %s: %w", o.Address, err)
		}
		m, _, err := format.UnwrapMasterKey(blob, o.EscrowPrivate)
		if err != nil {
			return nil, err
		}
		kf, err := format.NewKeyFile(m, o.Passphrase, format.DefaultArgon2(), nil, nil)
		if err != nil {
			return nil, err
		}
		b, err := kf.Marshal()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(cfg.KeyFile), 0o700); err != nil {
			return nil, err
		}
		if err := writeNew(cfg.KeyFile, b); err != nil {
			return nil, err
		}
		wroteKey = true
		keys, _ = format.DeriveKeys(m)
		master, recoveredEn = m, en
	default:
		m0, err := format.GenerateMaster()
		if err != nil {
			return nil, err
		}
		master = m0
		recovery, _ := format.EncodeRecoveryKey(master)
		if o.ConfirmRecovery == nil {
			return nil, errors.New("a new key was needed but the recovery key cannot be confirmed in this mode")
		}
		if err := o.ConfirmRecovery(recovery); err != nil {
			return nil, err
		}
		kf, err := format.NewKeyFile(master, o.Passphrase, format.DefaultArgon2(), nil, nil)
		if err != nil {
			return nil, err
		}
		b, err := kf.Marshal()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(cfg.KeyFile), 0o700); err != nil {
			return nil, err
		}
		if err := writeNew(cfg.KeyFile, b); err != nil {
			return nil, err
		}
		wroteKey = true
		keys, _ = format.DeriveKeys(master)
		res.NewKey = true
	}
	res.DomainID = keys.DomainID

	en := recoveredEn
	if en == nil {
		en, err = remote.Enroll(ctx, o.Address, o.Token, cfg.Host, map[string]any{"client": "incus-backup"})
		if err != nil {
			return nil, fmt.Errorf("enrolling at %s: %w", o.Address, err)
		}
	}
	cl, err := remote.New(o.Address, en.CAPEM, en.CertPEM, en.KeyPEM, keys.DomainID)
	if err != nil {
		return nil, err
	}
	who, err := cl.Whoami(ctx)
	if err != nil {
		return nil, fmt.Errorf("the server accepted the token but a test request failed: %w", err)
	}
	if who.DomainID != "" && who.DomainID != keys.DomainID {
		if wroteKey { // the key we just wrote protects nothing here: do not leave a stray key behind to be mistaken for the real one
			_ = os.Remove(cfg.KeyFile)
		}
		return nil, fmt.Errorf("%s already belongs to key domain %s but this host's key is %s. The token has been used up. Import the key that created that repository first (incus-backup key import, from your recovery key), then ask the server for a new token and run init again. A new key was NOT kept",
			o.Address, who.DomainID, keys.DomainID)
	}
	if who.Scope != "backup" {
		return nil, fmt.Errorf("the token grants scope %q, a host needs \"backup\"", who.Scope)
	}
	dir := cfg.ServerDir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for fn, b := range map[string][]byte{"ca.crt": en.CAPEM, "client.crt": en.CertPEM, "client.key": en.KeyPEM} {
		if err := os.WriteFile(filepath.Join(dir, fn), b, 0o600); err != nil {
			return nil, err
		}
	}
	if o.EscrowPublic != "" {
		wrapped, err := format.WrapMasterKey(master, o.EscrowPublic)
		if err != nil {
			return nil, err
		}
		switch err := cl.PutEscrow(ctx, wrapped); {
		case err == nil:
			res.EscrowStored = true
		case remote.IsStatus(err, 409):
			res.EscrowExists = true // somebody stored one already (maybe not for this key): the operator should check
		default:
			return nil, fmt.Errorf("storing the escrow blob on %s: %w", o.Address, err)
		}
	}
	cfg.Servers[name] = config.Server{Address: o.Address}
	if cfg.Control == "" {
		cfg.Control = name
	}
	if err := cfg.Save(o.ConfigPath); err != nil {
		return nil, err
	}
	res.Repo, res.Scope = who.Repository, who.Scope
	return res, nil
}

// writeNew creates a file that must not exist yet, mode 0600.
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ImportKey writes a key file from a recovery key (disaster recovery on a new
// host). It refuses to overwrite an existing key file. Returns the key domain id.
func ImportKey(keyFile, recovery, passphrase string) (string, error) {
	master, err := format.DecodeRecoveryKey(recovery)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(keyFile); err == nil {
		return "", fmt.Errorf("%s already exists: refusing to overwrite a key", keyFile)
	}
	kf, err := format.NewKeyFile(master, passphrase, format.DefaultArgon2(), nil, nil)
	if err != nil {
		return "", err
	}
	b, err := kf.Marshal()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return "", err
	}
	if err := writeNew(keyFile, b); err != nil {
		return "", err
	}
	return kf.DomainID, nil
}
