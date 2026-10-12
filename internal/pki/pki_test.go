package pki

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func setup(t *testing.T) (*CA, *httptest.Server, []byte) {
	t.Helper()
	now := time.Now()
	ca, _, err := NewCA("test", now)
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := ca.NewServerCert("srv", []string{"127.0.0.1"}, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ca.ServerTLS(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn := "none"
		if len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		_, _ = w.Write([]byte(cn))
	}))
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return ca, srv, ca.CertPEM
}

func get(t *testing.T, cfg *tls.Config, url string) (string, error) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func TestEnrolAndMutualTLS(t *testing.T) {
	ca, srv, caPEM := setup(t)
	// enrolment: pinned fingerprint, no client certificate yet
	body, err := get(t, PinnedTLS(Fingerprint(ca.Cert)), srv.URL)
	if err != nil || body != "none" {
		t.Fatalf("pinned: %q %v", body, err)
	}
	keyPEM, csrPEM, _ := NewKeyAndCSR("ignored-by-ca")
	certPEM, fp, err := ca.SignClient(csrPEM, "benfica", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cc, _ := ParseCertPEM(certPEM)
	if Fingerprint(cc) != fp || cc.Subject.CommonName != "benfica" {
		t.Fatal("subject/fingerprint")
	}
	cfg, err := ClientTLS(caPEM, certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	body, err = get(t, cfg, srv.URL)
	if err != nil || body != "benfica" {
		t.Fatalf("mtls: %q %v", body, err)
	}
}

func TestWrongPinAndWrongCARejected(t *testing.T) {
	_, srv, _ := setup(t)
	if _, err := get(t, PinnedTLS("00"+"11"), srv.URL); err == nil {
		t.Fatal("wrong pin accepted")
	}
	other, _, _ := NewCA("other", time.Now())
	cfg, _ := ClientTLS(other.CertPEM, nil, nil)
	if _, err := get(t, cfg, srv.URL); err == nil {
		t.Fatal("server accepted by a client that trusts a different CA")
	}
}

func TestClientCertFromOtherCARejectedByServer(t *testing.T) {
	ca, srv, caPEM := setup(t)
	evil, _, _ := NewCA("evil", time.Now())
	keyPEM, csrPEM, _ := NewKeyAndCSR("x")
	certPEM, _, _ := evil.SignClient(csrPEM, "benfica", time.Now(), time.Hour)
	cfg, _ := ClientTLS(caPEM, certPEM, keyPEM)
	// Go clients only offer certificates the server's CA list accepts, so the
	// foreign certificate is simply not sent: the handler sees no identity.
	body, err := get(t, cfg, srv.URL)
	if err == nil && body != "none" {
		t.Fatalf("server authenticated a client certificate from a foreign CA as %q", body)
	}
	_ = ca
}

func TestExpiredClientCertRejected(t *testing.T) {
	ca, srv, caPEM := setup(t)
	keyPEM, csrPEM, _ := NewKeyAndCSR("x")
	certPEM, _, _ := ca.SignClient(csrPEM, "benfica", time.Now().Add(-48*time.Hour), time.Hour)
	cfg, _ := ClientTLS(caPEM, certPEM, keyPEM)
	body, err := get(t, cfg, srv.URL)
	if err == nil && body != "none" {
		t.Fatalf("expired client certificate authenticated as %q", body)
	}
}

func TestSignClientRejectsBadCSR(t *testing.T) {
	ca, _, _ := setup(t)
	if _, _, err := ca.SignClient([]byte("junk"), "x", time.Now(), time.Hour); err == nil {
		t.Fatal("junk csr accepted")
	}
}

func TestCACannotBeUsedAsLeafForServerAuthImpersonation(t *testing.T) {
	// a client certificate must not validate as a server certificate
	ca, _, caPEM := setup(t)
	_, csrPEM, _ := NewKeyAndCSR("x")
	certPEM, _, _ := ca.SignClient(csrPEM, "benfica", time.Now(), time.Hour)
	cert, _ := ParseCertPEM(certPEM)
	if err := verifyChain([][]byte{cert.Raw, ca.Cert.Raw}, func(c *x509.Certificate) bool { return c.Equal(ca.Cert) }); err == nil {
		t.Fatal("client-only certificate verified for server auth")
	}
	_ = caPEM
}
