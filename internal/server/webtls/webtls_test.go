package webtls

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// handshake does a real TLS handshake against the manager's config and returns the served leaf.
func handshake(t *testing.T, cfg *tls.Config, name string) *x509.Certificate {
	t.Helper()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := tls.Server(c2, cfg)
	done := make(chan error, 1)
	go func() { done <- srv.Handshake() }()
	cli := tls.Client(c1, &tls.Config{InsecureSkipVerify: true, ServerName: name})
	if err := cli.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return cli.ConnectionState().PeerCertificates[0]
}

func TestSelfSignedIsGeneratedPersistedReusedAndRenewed(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Config{Domains: []string{"backup.example.test"}}, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := m.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	first := handshake(t, cfg, "backup.example.test")
	if err := first.VerifyHostname("backup.example.test"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "web.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", err, fi)
	}
	// a restart reuses the stored certificate
	m2, _ := New(Config{Domains: []string{"backup.example.test"}}, dir, nil)
	cfg2, _ := m2.TLSConfig()
	if again := handshake(t, cfg2, ""); again.SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatal("restart regenerated the certificate")
	}
	// a new domain forces a new certificate that covers it
	m3, _ := New(Config{Domains: []string{"other.example.test"}}, dir, nil)
	cfg3, _ := m3.TLSConfig()
	if c := handshake(t, cfg3, ""); c.VerifyHostname("other.example.test") != nil {
		t.Fatal("new domain not covered")
	}
	// within 30 days of expiry the certificate is replaced automatically
	m3.now = func() time.Time { return first.NotAfter.Add(-10 * 24 * time.Hour) }
	m3.loadedAt = time.Time{}
	renewed := handshake(t, cfg3, "")
	if !renewed.NotAfter.After(first.NotAfter.Add(300 * 24 * time.Hour)) {
		t.Fatalf("not renewed: expires %v", renewed.NotAfter)
	}
	if in := m3.Info(); in.Mode != "self" || !in.AutoRenew || in.Finger == "" {
		t.Fatalf("%+v", in)
	}
}

func TestFilesAreHotReloadedWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, notAfterDays int) *x509.Certificate {
		_, leaf, c, k, err := selfSigned([]string{name}, time.Now().AddDate(0, 0, notAfterDays-365))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "c.pem"), c, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "k.pem"), k, 0o600); err != nil {
			t.Fatal(err)
		}
		return leaf
	}
	a := write("one.example.test", 90)
	m, err := New(Config{Mode: "files", Cert: filepath.Join(dir, "c.pem"), Key: filepath.Join(dir, "k.pem")}, filepath.Join(dir, "s"), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := m.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := handshake(t, cfg, ""); got.SerialNumber.Cmp(a.SerialNumber) != 0 {
		t.Fatal("wrong certificate served")
	}
	// renewal by an external tool replaces the files; the next stale check (>1 min) picks it up
	b := write("one.example.test", 365)
	m.mu.Lock()
	m.loadedAt = time.Now().Add(-2 * time.Minute)
	m.mu.Unlock()
	if got := handshake(t, cfg, ""); got.SerialNumber.Cmp(b.SerialNumber) != 0 {
		t.Fatal("renewed file was not picked up")
	}
	// a broken replacement is ignored: the old certificate keeps serving and the error is logged
	if err := os.WriteFile(filepath.Join(dir, "c.pem"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.loadedAt = time.Now().Add(-2 * time.Minute)
	m.mu.Unlock()
	if got := handshake(t, cfg, ""); got.SerialNumber.Cmp(b.SerialNumber) != 0 {
		t.Fatal("a corrupt replacement took the listener down")
	}
	if in := m.Info(); in.AutoRenew || in.DaysLeft < 300 {
		t.Fatalf("%+v", in)
	}
}

func TestConfigValidationAndACMEPolicy(t *testing.T) {
	for _, bad := range []Config{{Mode: "files"}, {Mode: "acme"}, {Mode: "magic"}} {
		if _, err := New(bad, t.TempDir(), nil); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	m, err := New(Config{Mode: "acme", Domains: []string{"backup.example.test"}, Email: "ops@example.test"}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := m.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	// only configured names may trigger an ACME order: an arbitrary SNI is refused before any network call
	if _, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.example.test"}); err == nil {
		t.Fatal("ACME would issue for an unconfigured name")
	}
	hasALPN := false
	for _, p := range cfg.NextProtos {
		hasALPN = hasALPN || p == "acme-tls/1"
	}
	if !hasALPN {
		t.Fatal("TLS-ALPN-01 challenge protocol not enabled")
	}
}
