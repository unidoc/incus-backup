package pki

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Validity periods and renewal thresholds (docs/certificates.md).
const (
	ClientValidity = 2 * 365 * 24 * time.Hour // client and sync-peer certificates
	ServerValidity = 2 * 365 * 24 * time.Hour // the server certificate
	// RenewBefore: clients, peers and the server renew when less than this remains.
	RenewBefore = 45 * 24 * time.Hour
	// RenewGrace: a certificate that expired less than this long ago may still renew
	// (a host that was switched off for a few weeks); after that it must enrol again.
	RenewGrace = 30 * 24 * time.Hour
)

// ParseCertsPEM parses every CERTIFICATE block of b (a trust bundle). It fails
// when there is none.
func ParseCertsPEM(b []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("not a PEM certificate")
	}
	return out, nil
}

// EncodeCertsPEM is the PEM concatenation of certs (a trust bundle).
func EncodeCertsPEM(certs ...*x509.Certificate) []byte {
	var b bytes.Buffer
	for _, c := range certs {
		_ = pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}
	return b.Bytes()
}

// SANs returns the DNS names and IP addresses of a certificate, in that order.
func SANs(c *x509.Certificate) []string {
	var out []string
	out = append(out, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	return out
}

// VerifyClientCert checks that leaf is a client certificate signed by one of roots.
// Validity in TIME is deliberately not judged here (the time used is clamped into
// the certificate's own validity): the caller decides with its own clock, because
// renewal accepts a certificate that expired recently and everything else does not.
func VerifyClientCert(leaf *x509.Certificate, intermediates []*x509.Certificate, roots *x509.CertPool) error {
	t := time.Now()
	if t.After(leaf.NotAfter) {
		t = leaf.NotAfter.Add(-time.Second)
	}
	if t.Before(leaf.NotBefore) {
		t = leaf.NotBefore.Add(time.Second)
	}
	inter := x509.NewCertPool()
	for _, c := range intermediates {
		inter.AddCert(c)
	}
	_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: t, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	return err
}

// ServerCreds is what the server presents and whom it trusts at one moment.
type ServerCreds struct {
	Cert  tls.Certificate // leaf followed by the presenting CA
	Roots *x509.CertPool  // CAs whose client certificates are accepted
}

// DynamicServerTLS returns a server configuration that reads the credentials from
// get on EVERY handshake, so a renewed server certificate or a changed trust bundle
// takes effect without a restart (connections already open keep theirs).
//
// Client certificates are optional at the TLS layer (enrolment has none yet) and
// are checked for a valid signature by a trusted CA; their validity in time is
// judged by the service with its own clock (see VerifyClientCert).
func DynamicServerTLS(get func() *ServerCreds) *tls.Config {
	verify := func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return nil
		}
		var certs []*x509.Certificate
		for _, r := range raw {
			c, err := x509.ParseCertificate(r)
			if err != nil {
				return err
			}
			certs = append(certs, c)
		}
		return VerifyClientCert(certs[0], certs[1:], get().Roots)
	}
	base := &tls.Config{MinVersion: tls.VersionTLS13}
	perConn := func() *tls.Config {
		c := get()
		return &tls.Config{
			MinVersion:            tls.VersionTLS13,
			Certificates:          []tls.Certificate{c.Cert},
			ClientAuth:            tls.RequestClientCert,
			ClientCAs:             c.Roots, // only the hint sent to clients; verification is verify above
			VerifyPeerCertificate: verify,
			NextProtos:            base.NextProtos,
		}
	}
	first := perConn()
	base.Certificates = first.Certificates // so http.Server/httptest see a certificate source
	base.ClientAuth, base.ClientCAs, base.VerifyPeerCertificate = first.ClientAuth, first.ClientCAs, verify
	base.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { c := get(); return &c.Cert, nil }
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) { return perConn(), nil }
	return base
}

// WriteFileAtomic replaces path with b: temp file in the same directory, fsync,
// rename, fsync of the directory. Readers see the old or the new content, never a mix.
func WriteFileAtomic(path string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		f.Close()
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
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return SyncDir(dir)
}

// SyncDir fsyncs a directory (makes renames in it durable).
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// PairMatches reports whether certPEM and keyPEM belong together.
func PairMatches(certPEM, keyPEM []byte) bool {
	_, err := tls.X509KeyPair(certPEM, keyPEM)
	return err == nil
}

// LoadPair reads base.crt and base.key. Two renames are not atomic together, so a
// crash while a renewal replaced them can leave a certificate next to the other
// one's key; the .prev copies kept by ReplacePair resolve that: the first matching
// combination of {crt, crt.prev} x {key, key.prev} wins, the current files first.
func LoadPair(base string) (certPEM, keyPEM []byte, err error) {
	read := func(p string) []byte { b, _ := os.ReadFile(p); return b }
	crt, key := read(base+".crt"), read(base+".key")
	crtPrev, keyPrev := read(base+".crt.prev"), read(base+".key.prev")
	if crt == nil && crtPrev == nil {
		_, err := os.ReadFile(base + ".crt")
		return nil, nil, err
	}
	for _, p := range [][2][]byte{{crt, key}, {crt, keyPrev}, {crtPrev, key}, {crtPrev, keyPrev}} {
		if p[0] != nil && p[1] != nil && PairMatches(p[0], p[1]) {
			return p[0], p[1], nil
		}
	}
	return nil, nil, fmt.Errorf("%s.crt and %s.key do not belong together (and no .prev copy matches)", base, base)
}

// ReplacePair installs a new certificate and key as base.crt/base.key, first saving
// the current pair as base.crt.prev/base.key.prev. Every step is atomic and durable;
// LoadPair repairs a crash between the two renames.
func ReplacePair(base string, certPEM, keyPEM []byte) error {
	if !PairMatches(certPEM, keyPEM) {
		return errors.New("refusing to install a certificate with a key that does not match it")
	}
	if oc, ok, err := LoadPair(base); err == nil {
		if err := WriteFileAtomic(base+".crt.prev", oc, 0o600); err != nil {
			return err
		}
		if err := WriteFileAtomic(base+".key.prev", ok, 0o600); err != nil {
			return err
		}
	}
	if err := WriteFileAtomic(base+".key", keyPEM, 0o600); err != nil {
		return err
	}
	return WriteFileAtomic(base+".crt", certPEM, 0o600)
}

// RestorePrev puts the .prev pair back (a renewed certificate the server refused).
func RestorePrev(base string) error {
	crt, err := os.ReadFile(base + ".crt.prev")
	if err != nil {
		return err
	}
	key, err := os.ReadFile(base + ".key.prev")
	if err != nil {
		return err
	}
	if !PairMatches(crt, key) {
		return errors.New("the .prev certificate and key do not match")
	}
	if err := WriteFileAtomic(base+".key", key, 0o600); err != nil {
		return err
	}
	return WriteFileAtomic(base+".crt", crt, 0o600)
}
