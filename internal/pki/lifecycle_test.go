package pki

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newCreds(t *testing.T, ca *CA, sans []string) (*ServerCreds, *x509.Certificate) {
	t.Helper()
	certPEM, keyPEM, err := ca.NewServerCert("srv", sans, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	pair.Certificate = append(pair.Certificate, ca.Cert.Raw)
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	return &ServerCreds{Cert: pair, Roots: pool}, leaf
}

// A connection opened before the server certificate is replaced keeps working; a new
// connection presents the new certificate. The client connects by IP (no SNI), the case
// in which Go never consults GetCertificate when Certificates is set.
func TestDynamicServerTLSHotReload(t *testing.T) {
	ca, _, _ := NewCA("test", time.Now())
	c1, leaf1 := newCreds(t, ca, []string{"127.0.0.1"})
	c2, leaf2 := newCreds(t, ca, []string{"127.0.0.1", "backup.example"})
	var cur atomic.Pointer[ServerCreds]
	cur.Store(c1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	srv.TLS = DynamicServerTLS(func() *ServerCreds { return cur.Load() })
	srv.StartTLS()
	defer srv.Close()

	cfg, err := ClientTLS(ca.CertPEM, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	type seen struct {
		fp     string
		reused bool
	}
	get := func(c *http.Client) seen {
		t.Helper()
		var s seen
		req, _ := http.NewRequest("GET", srv.URL, nil)
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { s.reused = i.Reused }}))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		s.fp = Fingerprint(resp.TLS.PeerCertificates[0])
		return s
	}
	before := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg.Clone()}}
	if s := get(before); s.fp != Fingerprint(leaf1) {
		t.Fatal("first connection did not see the first certificate")
	}
	cur.Store(c2) // the swap
	if s := get(before); !s.reused || s.fp != Fingerprint(leaf1) {
		t.Fatalf("the connection opened before the swap must keep working on its certificate: %+v", s)
	}
	after := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg.Clone()}}
	s := get(after)
	if s.reused || s.fp != Fingerprint(leaf2) {
		t.Fatalf("a new connection must see the new certificate: %+v", s)
	}
}

func TestDynamicServerTLSClientCertTimeIsTheCallersJob(t *testing.T) {
	ca, _, _ := NewCA("test", time.Now().Add(-72*time.Hour))
	c, _ := newCreds(t, ca, []string{"127.0.0.1"})
	var mu sync.Mutex
	var gotCN string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotCN = "none"
		if len(r.TLS.PeerCertificates) > 0 {
			gotCN = r.TLS.PeerCertificates[0].Subject.CommonName
		}
	}))
	srv.TLS = DynamicServerTLS(func() *ServerCreds { return c })
	srv.StartTLS()
	defer srv.Close()
	call := func(certPEM, keyPEM []byte) (string, error) {
		cfg, err := ClientTLS(ca.CertPEM, certPEM, keyPEM)
		if err != nil {
			return "", err
		}
		resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}).Get(srv.URL)
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		mu.Lock()
		defer mu.Unlock()
		return gotCN, nil
	}
	// expired two days ago: the handshake succeeds, the handler sees it (renewal decides)
	key, csr, _ := NewKeyAndCSR("x")
	expired, _, _ := ca.SignClient(csr, "benfica", time.Now().Add(-70*time.Hour), time.Hour)
	if cn, err := call(expired, key); err != nil || cn != "benfica" {
		t.Fatalf("expired but genuine certificate: %q %v", cn, err)
	}
	// a foreign CA's certificate never reaches the handler as an identity
	evil, _, _ := NewCA("evil", time.Now())
	key2, csr2, _ := NewKeyAndCSR("x")
	foreign, _, _ := evil.SignClient(csr2, "benfica", time.Now(), time.Hour)
	if cn, err := call(foreign, key2); err == nil && cn != "none" {
		t.Fatalf("foreign certificate authenticated as %q", cn)
	}
	// and VerifyClientCert itself rejects it even when presented directly
	fc, _ := ParseCertPEM(foreign)
	if VerifyClientCert(fc, nil, c.Roots) == nil {
		t.Fatal("foreign certificate verified")
	}
	ec, _ := ParseCertPEM(expired)
	if err := VerifyClientCert(ec, nil, c.Roots); err != nil {
		t.Fatalf("expired genuine certificate must verify (time is judged by the caller): %v", err)
	}
}

func TestTrustBundle(t *testing.T) {
	a, _, _ := NewCA("a", time.Now())
	b, _, _ := NewCA("b", time.Now())
	bundle := EncodeCertsPEM(a.Cert, b.Cert)
	cs, err := ParseCertsPEM(bundle)
	if err != nil || len(cs) != 2 || Fingerprint(cs[1]) != Fingerprint(b.Cert) {
		t.Fatalf("bundle: %v %d", err, len(cs))
	}
	if _, err := ParseCertsPEM([]byte("junk")); err == nil {
		t.Fatal("junk parsed")
	}
	// a client with the bundle trusts a server of either CA; with one CA only that one
	for _, ca := range []*CA{a, b} {
		c, _ := newCreds(t, ca, []string{"127.0.0.1"})
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.TLS = DynamicServerTLS(func() *ServerCreds { return c })
		srv.StartTLS()
		cfg, _ := ClientTLS(bundle, nil, nil)
		if _, err := get(t, cfg, srv.URL); err != nil {
			t.Fatalf("bundle client refused CA %s: %v", ca.Cert.Subject.CommonName, err)
		}
		only, _ := ClientTLS(a.CertPEM, nil, nil)
		_, err := get(t, only, srv.URL)
		if (ca == a) != (err == nil) {
			t.Fatalf("single-CA client and CA %s: %v", ca.Cert.Subject.CommonName, err)
		}
		srv.Close()
	}
}

func TestReplacePairAndCrashRepair(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "client")
	ca, _, _ := NewCA("t", time.Now())
	mk := func() ([]byte, []byte) {
		k, csr, _ := NewKeyAndCSR("x")
		c, _, _ := ca.SignClient(csr, "x", time.Now(), time.Hour)
		return c, k
	}
	c1, k1 := mk()
	c2, k2 := mk()
	if err := ReplacePair(base, c1, k1); err != nil {
		t.Fatal(err)
	}
	if err := ReplacePair(base, c2, k1); err == nil {
		t.Fatal("mismatched pair installed")
	}
	if err := ReplacePair(base, c2, k2); err != nil {
		t.Fatal(err)
	}
	gc, gk, err := LoadPair(base)
	if err != nil || string(gc) != string(c2) || string(gk) != string(k2) {
		t.Fatal("new pair not loaded")
	}
	if b, _ := os.ReadFile(base + ".crt.prev"); string(b) != string(c1) {
		t.Fatal("previous certificate not kept")
	}
	if st, _ := os.Stat(base + ".key"); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode())
	}
	// crash between the two renames of a renewal: the new key is in place, the certificate is the old one
	c3, k3 := mk()
	_ = WriteFileAtomic(base+".crt.prev", c2, 0o600)
	_ = WriteFileAtomic(base+".key.prev", k2, 0o600)
	if err := WriteFileAtomic(base+".key", k3, 0o600); err != nil {
		t.Fatal(err)
	}
	gc, gk, err = LoadPair(base)
	if err != nil || string(gc) != string(c2) || string(gk) != string(k2) {
		t.Fatalf("crash state not repaired to the previous pair: %v", err)
	}
	_ = c3
	// RestorePrev puts the previous pair back
	if err := ReplacePair(base, c3, k3); err != nil {
		t.Fatal(err)
	}
	if err := RestorePrev(base); err != nil {
		t.Fatal(err)
	}
	if gc, _, _ = LoadPair(base); string(gc) != string(c2) {
		t.Fatal("RestorePrev did not restore")
	}
	// no temp files left behind
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name()[0] == '.' {
			t.Fatalf("left over: %s", e.Name())
		}
	}
	// garbage everywhere: an error, not a wrong pair
	_ = os.WriteFile(base+".key", []byte("x"), 0o600)
	_ = os.WriteFile(base+".key.prev", []byte("x"), 0o600)
	if _, _, err := LoadPair(base); err == nil {
		t.Fatal("garbage accepted")
	}
}
