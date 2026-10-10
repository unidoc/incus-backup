// Package webtls builds the TLS configuration of the web listener (the management UI and /api/v1).
//
// The agent API keeps its own private CA and mutual TLS; browsers need a certificate they can trust, so
// the web listener has three modes:
//
//   - self:  a self-signed certificate generated on first start, kept in the state directory and renewed
//     automatically. Browsers warn once; the fingerprint is logged so it can be pinned.
//   - files: your own certificate and key (for example from an internal CA or certbot). The files are
//     watched and reloaded without a restart when they change, so renewals need no manual step.
//   - acme:  certificates are obtained and renewed automatically from an ACME CA (Let's Encrypt by default)
//     for the listed domains, using the TLS-ALPN-01 challenge on the web listener (it must be reachable
//     on port 443) and, optionally, HTTP-01 on port 80.
package webtls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// Config is the bootstrap configuration of the web listener (server.yaml `web:`).
type Config struct {
	// Mode is "self" (default), "files" or "acme".
	Mode string `yaml:"mode,omitempty"`
	// Cert and Key are the PEM files of mode "files".
	Cert string `yaml:"cert,omitempty"`
	Key  string `yaml:"key,omitempty"`
	// Domains are the names mode "acme" may obtain certificates for (also the SANs of mode "self").
	Domains []string `yaml:"domains,omitempty"`
	// Email is the ACME account contact.
	Email string `yaml:"email,omitempty"`
	// Directory is the ACME directory URL (default: Let's Encrypt production).
	Directory string `yaml:"acme_directory,omitempty"`
	// HTTPChallenge is an optional listen address (":80") for HTTP-01 and the http->https redirect.
	HTTPChallenge string `yaml:"acme_http_listen,omitempty"`
}

// Manager serves certificates for the listener.
type Manager struct {
	cfg      Config
	dir      string
	log      *slog.Logger
	now      func() time.Time
	mu       sync.RWMutex
	cur      *tls.Certificate
	leaf     *x509.Certificate
	loadedAt time.Time
	fileSig  string
	acme     *autocert.Manager
}

// New prepares a manager. dir is where generated and cached material lives.
func New(cfg Config, dir string, log *slog.Logger) (*Manager, error) {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{cfg: cfg, dir: dir, log: log, now: time.Now}
	switch cfg.Mode {
	case "", "self":
		m.cfg.Mode = "self"
	case "files":
		if cfg.Cert == "" || cfg.Key == "" {
			return nil, errors.New("web tls mode files needs cert and key")
		}
	case "acme":
		if len(cfg.Domains) == 0 {
			return nil, errors.New("web tls mode acme needs at least one domain")
		}
		m.acme = &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(filepath.Join(dir, "acme")),
			HostPolicy: autocert.HostWhitelist(cfg.Domains...),
			Email:      cfg.Email,
		}
		if cfg.Directory != "" {
			m.acme.Client = &acme.Client{DirectoryURL: cfg.Directory}
		}
	default:
		return nil, fmt.Errorf("web tls mode %q: want self, files or acme", cfg.Mode)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return m, nil
}

// Mode reports the active mode.
func (m *Manager) Mode() string { return m.cfg.Mode }

// TLSConfig returns the listener configuration. Certificates are looked up per handshake, so renewals and
// replaced files take effect without restarting.
func (m *Manager) TLSConfig() (*tls.Config, error) {
	switch m.cfg.Mode {
	case "acme":
		c := m.acme.TLSConfig()
		c.MinVersion = tls.VersionTLS12
		return c, nil
	default:
		if err := m.reload(); err != nil {
			return nil, err
		}
		return &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}, GetCertificate: m.getCert}, nil
	}
}

func (m *Manager) getCert(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	// cheap staleness check at most once a minute: re-read files / regenerate when near expiry
	m.mu.RLock()
	stale := m.now().Sub(m.loadedAt) > time.Minute
	m.mu.RUnlock()
	if stale {
		if err := m.reload(); err != nil {
			m.log.Error("reloading the web certificate failed; keeping the previous one", "err", err)
			m.mu.Lock()
			m.loadedAt = m.now() // retry in a minute, not on every handshake
			m.mu.Unlock()
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cur == nil {
		return nil, errors.New("no web certificate available")
	}
	return m.cur, nil
}

// Reload forces a re-read (SIGHUP).
func (m *Manager) Reload() error { return m.reload() }

func (m *Manager) reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	switch m.cfg.Mode {
	case "files":
		sig, err := fileSig(m.cfg.Cert, m.cfg.Key)
		if err != nil {
			return err
		}
		if sig == m.fileSig && m.cur != nil {
			m.loadedAt = now
			return nil
		}
		pair, err := tls.LoadX509KeyPair(m.cfg.Cert, m.cfg.Key)
		if err != nil {
			return fmt.Errorf("loading web certificate: %w", err)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return err
		}
		m.cur, m.leaf, m.fileSig, m.loadedAt = &pair, leaf, sig, now
		m.log.Info("web certificate loaded", "file", m.cfg.Cert, "expires", leaf.NotAfter.Format(time.RFC3339), "fingerprint", fp(leaf))
	default: // self
		if m.cur != nil && m.leaf.NotAfter.Sub(now) > 30*24*time.Hour {
			m.loadedAt = now
			return nil
		}
		crt, key := filepath.Join(m.dir, "web.crt"), filepath.Join(m.dir, "web.key")
		if pair, err := tls.LoadX509KeyPair(crt, key); err == nil {
			if leaf, err := x509.ParseCertificate(pair.Certificate[0]); err == nil && leaf.NotAfter.Sub(now) > 30*24*time.Hour && coversDomains(leaf, m.cfg.Domains) {
				m.cur, m.leaf, m.loadedAt = &pair, leaf, now
				return nil
			}
		}
		pair, leaf, certPEM, keyPEM, err := selfSigned(m.cfg.Domains, now)
		if err != nil {
			return err
		}
		if err := writeAtomic(key, keyPEM, 0o600); err != nil {
			return err
		}
		if err := writeAtomic(crt, certPEM, 0o644); err != nil {
			return err
		}
		m.cur, m.leaf, m.loadedAt = pair, leaf, now
		m.log.Info("generated a self-signed web certificate (browsers will warn once)", "expires", leaf.NotAfter.Format(time.RFC3339), "fingerprint", fp(leaf))
	}
	return nil
}

// Info describes the certificate in use, for the status page and notifications.
type Info struct {
	Mode      string    `json:"mode"`
	Subject   string    `json:"subject"`
	DNSNames  []string  `json:"dns_names"`
	NotAfter  time.Time `json:"not_after"`
	Finger    string    `json:"fingerprint"`
	Issuer    string    `json:"issuer"`
	DaysLeft  int       `json:"days_left"`
	AutoRenew bool      `json:"auto_renew"`
}

// Info returns the current certificate (loading it when needed). In acme mode the certificate is only
// known after the first handshake; the zero Info is returned until then.
func (m *Manager) Info() Info {
	in := Info{Mode: m.cfg.Mode, AutoRenew: m.cfg.Mode != "files"}
	m.mu.RLock()
	leaf := m.leaf
	m.mu.RUnlock()
	if leaf == nil {
		return in
	}
	in.Subject, in.DNSNames, in.NotAfter, in.Finger, in.Issuer = leaf.Subject.CommonName, leaf.DNSNames, leaf.NotAfter, fp(leaf), leaf.Issuer.CommonName
	in.DaysLeft = int(leaf.NotAfter.Sub(m.now()).Hours() / 24)
	return in
}

// ChallengeHandler serves ACME HTTP-01 challenges and redirects everything else to https; nil unless
// mode acme with an http listen address configured.
func (m *Manager) ChallengeHandler() http.Handler {
	if m.acme == nil || m.cfg.HTTPChallenge == "" {
		return nil
	}
	return m.acme.HTTPHandler(nil)
}

// HTTPChallengeAddr is where ChallengeHandler should listen.
func (m *Manager) HTTPChallengeAddr() string { return m.cfg.HTTPChallenge }

// Watch re-reads files and renews self-signed certificates in the background until ctx ends.
func (m *Manager) Watch(ctx context.Context) {
	if m.cfg.Mode == "acme" {
		return // autocert renews on demand and in the background by itself
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.reload(); err != nil {
				m.log.Error("web certificate reload failed", "err", err)
			}
		}
	}
}

func fileSig(paths ...string) (string, error) {
	h := sha256.New()
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s|%d|%d;", p, st.Size(), st.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fp(c *x509.Certificate) string { s := sha256.Sum256(c.Raw); return hex.EncodeToString(s[:]) }

func coversDomains(c *x509.Certificate, domains []string) bool {
	for _, d := range domains {
		if err := c.VerifyHostname(d); err != nil {
			return false
		}
	}
	return true
}

func selfSigned(domains []string, now time.Time) (*tls.Certificate, *x509.Certificate, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sn, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	host, _ := os.Hostname()
	if host == "" {
		host = "incus-backup"
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn, Subject: pkix.Name{CommonName: host, Organization: []string{"incus-backup"}},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	names := append([]string{host, "localhost"}, domains...)
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	tmpl.IPAddresses = append(tmpl.IPAddresses, net.IPv4(127, 0, 0, 1), net.IPv6loopback)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	leaf, _ := x509.ParseCertificate(der)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf, certPEM, keyPEM, nil
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
