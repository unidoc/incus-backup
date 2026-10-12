package service_test

import (
	"context"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/pki"
	"github.com/unidoc/incus-backup/internal/server/service"
)

const day = 24 * time.Hour

func (e *env) renew(cert *x509.Certificate) (*x509.Certificate, *api.RenewResponse, error) {
	e.t.Helper()
	_, csr, _ := pki.NewKeyAndCSR("whatever-the-client-says")
	resp, err := e.svc.Renew(context.Background(), cert, csr, "127.0.0.1")
	if err != nil {
		return nil, nil, err
	}
	c, err := pki.ParseCertPEM([]byte(resp.Certificate))
	if err != nil {
		e.t.Fatal(err)
	}
	return c, resp, nil
}

func (e *env) auth(c *x509.Certificate) (*service.Host, error) {
	return e.svc.HostFromCert(context.Background(), c, "127.0.0.1")
}

func (e *env) validCerts(host string) int {
	var n int
	if err := e.svc.DB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM host_certs hc JOIN hosts h ON h.id=hc.host_id WHERE h.name=$1 AND NOT hc.revoked`, host).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestRenewOverlapConfirmRevocation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h0, a := e.enrolCert("benfica", api.ScopeBackup)
	if a.NotAfter.Sub(e.now) < 700*day {
		t.Fatalf("validity %s", a.NotAfter.Sub(e.now))
	}
	e.now = e.now.Add(700 * day) // 30 days left
	b, resp, err := e.renew(a)
	if err != nil {
		t.Fatal(err)
	}
	if b.Subject.CommonName != "benfica" || resp.Host != "benfica" || resp.Scope != api.ScopeBackup || resp.Repository != "custody" {
		t.Fatalf("the identity must be kept (CN forced to the host name): %s %+v", b.Subject.CommonName, resp)
	}
	if b.NotAfter.Sub(e.now) < 700*day {
		t.Fatal("renewed certificate is not a full validity period")
	}
	// overlap: both work, same identity
	for _, c := range []*x509.Certificate{a, b} {
		h, err := e.auth(c)
		if err != nil || h.ID != h0.ID {
			t.Fatalf("overlap: %v %+v", err, h)
		}
	}
	if n := e.validCerts("benfica"); n != 2 {
		t.Fatalf("valid certificates during overlap: %d", n)
	}
	// the old one cannot confirm, the new one can, and that revokes the old one
	ha, _ := e.auth(a)
	if _, err := e.svc.ConfirmRenewal(ctx, ha); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("the old certificate confirmed: %v", err)
	}
	hb, _ := e.auth(b)
	if n, err := e.svc.ConfirmRenewal(ctx, hb); err != nil || n != 1 {
		t.Fatalf("confirm: %d %v", n, err)
	}
	if _, err := e.auth(a); !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("old certificate after confirm: %v", err)
	}
	if _, err := e.auth(b); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.ConfirmRenewal(ctx, hb); err != nil || n != 0 {
		t.Fatalf("confirm is idempotent: %d %v", n, err)
	}
}

func TestOverlapEndsAtExpiryWithoutConfirm(t *testing.T) {
	e := newEnv(t)
	_, a := e.enrolCert("benfica", api.ScopeBackup)
	e.now = a.NotAfter.Add(-10 * day)
	b, _, err := e.renew(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.auth(a); err != nil {
		t.Fatalf("old certificate inside its validity: %v", err)
	}
	e.now = a.NotAfter.Add(time.Minute)
	if _, err := e.auth(a); !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("expired old certificate: %v", err)
	}
	if _, err := e.auth(b); err != nil {
		t.Fatal(err)
	}
}

func TestRenewalOfExpiredCertificates(t *testing.T) {
	e := newEnv(t)
	_, a := e.enrolCert("benfica", api.ScopeBackup)
	// expired 10 days ago: no longer authenticates, but may still renew
	e.now = a.NotAfter.Add(10 * day)
	if _, err := e.auth(a); !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("expired certificate authenticated: %v", err)
	}
	b, _, err := e.renew(a)
	if err != nil {
		t.Fatalf("renewal within the grace period: %v", err)
	}
	if _, err := e.auth(b); err != nil {
		t.Fatal(err)
	}
	// expired more than 30 days ago: must enrol again
	_, c := e.enrolCert("porto", api.ScopeBackup)
	e.now = c.NotAfter.Add(31 * day)
	if _, _, err := e.renew(c); !errors.Is(err, service.ErrCertExpired) {
		t.Fatalf("renewal 31 days after expiry: %v", err)
	}
	e.now = c.NotAfter.Add(29 * day)
	if _, _, err := e.renew(c); err != nil {
		t.Fatalf("renewal 29 days after expiry: %v", err)
	}
}

func TestDisabledHostRejectsEveryCertificateAndCannotRenew(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, a := e.enrolCert("benfica", api.ScopeBackup)
	e.now = e.now.Add(700 * day)
	b, _, err := e.renew(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetHostDisabled(ctx, "custody", "benfica", api.ScopeBackup, true); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*x509.Certificate{a, b} {
		if _, err := e.auth(c); !errors.Is(err, service.ErrForbidden) {
			t.Fatalf("certificate of a disabled host: %v", err)
		}
	}
	e.now = e.now.Add(time.Hour)
	if _, _, err := e.renew(b); !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("disabled host renewed: %v", err)
	}
	// re-enabling restores both (nothing was revoked by disabling)
	if err := e.svc.SetHostDisabled(ctx, "custody", "benfica", api.ScopeBackup, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.auth(b); err != nil {
		t.Fatal(err)
	}
	// a foreign CA's certificate never renews
	evil, _, _ := pki.NewCA("evil", e.now)
	_, csr, _ := pki.NewKeyAndCSR("x")
	pemC, _, _ := evil.SignClient(csr, "benfica", e.now, day)
	fc, _ := pki.ParseCertPEM(pemC)
	if _, _, err := e.renew(fc); !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("foreign certificate renewed: %v", err)
	}
}

func TestRenewRateLimitSupersededAndLostAnswer(t *testing.T) {
	e := newEnv(t)
	_, a := e.enrolCert("benfica", api.ScopeBackup)
	e.now = e.now.Add(700 * day)
	b, _, err := e.renew(a)
	if err != nil {
		t.Fatal(err)
	}
	// immediately again: rate limited
	if _, _, err := e.renew(b); !errors.Is(err, service.ErrTooManyRequests) {
		t.Fatalf("second renewal within the interval: %v", err)
	}
	// the answer to B's renewal was "lost": B was never used, so A may renew once more;
	// that revokes B (an identity never holds more than two certificates)
	e.now = e.now.Add(11 * time.Minute)
	c, _, err := e.renew(a)
	if err != nil {
		t.Fatalf("renewal after a lost answer: %v", err)
	}
	if _, err := e.auth(b); !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("orphaned certificate still valid: %v", err)
	}
	if n := e.validCerts("benfica"); n != 2 {
		t.Fatalf("valid certificates: %d", n)
	}
	// C is used; now A is superseded and can no longer renew
	if _, err := e.auth(c); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(11 * time.Minute)
	if _, _, err := e.renew(a); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("superseded certificate renewed: %v", err)
	}
	if _, _, err := e.renew(c); err != nil {
		t.Fatalf("current certificate: %v", err)
	}
}

func TestConcurrentRenewalsHaveOneWinner(t *testing.T) {
	e := newEnv(t)
	_, a := e.enrolCert("benfica", api.ScopeBackup)
	e.now = e.now.Add(700 * day)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, csr, _ := pki.NewKeyAndCSR("x")
			_, errs[i] = e.svc.Renew(context.Background(), a, csr, "127.0.0.1")
		}(i)
	}
	wg.Wait()
	ok, limited := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, service.ErrTooManyRequests):
			limited++
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	if ok != 1 || limited != n-1 {
		t.Fatalf("winners %d, rate-limited %d", ok, limited)
	}
	if v := e.validCerts("benfica"); v != 2 {
		t.Fatalf("valid certificates after concurrent renewals: %d", v)
	}
}

func TestReEnrolmentRevokesEarlierCertificates(t *testing.T) {
	e := newEnv(t)
	_, a := e.enrolCert("benfica", api.ScopeBackup)
	_, b := e.enrolCert("benfica", api.ScopeBackup)
	if _, err := e.auth(a); !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("certificate of the previous enrolment: %v", err)
	}
	if _, err := e.auth(b); err != nil {
		t.Fatal(err)
	}
}

func TestCertificateStatusLevels(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, a := e.enrolCert("benfica", api.ScopeBackup)
	r, err := e.svc.CertificateStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Level != "ok" || len(r.Hosts) != 1 || r.Hosts[0].State != "ok" || r.CA.Level != "ok" || r.Server.Level != "ok" || r.CA.Fingerprint == "" {
		t.Fatalf("fresh: %+v", r)
	}
	if r.Hosts[0].Name != "benfica" || r.Hosts[0].Scope != api.ScopeBackup || r.Hosts[0].Fingerprint != pki.Fingerprint(a) {
		t.Fatalf("host: %+v", r.Hosts[0])
	}
	e.now = a.NotAfter.Add(-50 * day)
	r, _ = e.svc.CertificateStatus(ctx)
	if h := r.Hosts[0]; h.State != "expiring" || h.Level != "warn" || h.DaysLeft != 50 {
		t.Fatalf("50 days left: %+v", h)
	}
	e.now = a.NotAfter.Add(-10 * day)
	r, _ = e.svc.CertificateStatus(ctx)
	if h := r.Hosts[0]; h.State != "expiring" || h.Level != "critical" || r.Level != "critical" {
		t.Fatalf("10 days left: %+v", h)
	}
	b, _, err := e.renew(a)
	if err != nil {
		t.Fatal(err)
	}
	r, _ = e.svc.CertificateStatus(ctx)
	if h := r.Hosts[0]; h.State != "ok" || h.LastRenewed == nil || h.OtherValid != 1 || h.Fingerprint != pki.Fingerprint(b) {
		t.Fatalf("after renewal: %+v", h)
	}
	e.now = b.NotAfter.Add(time.Hour)
	r, _ = e.svc.CertificateStatus(ctx)
	if h := r.Hosts[0]; h.State != "expired" {
		t.Fatalf("expired: %+v", h)
	}
	// CA: warns a year before its ten years end
	e.now = r.CA.NotAfter.Add(-300 * day)
	r, _ = e.svc.CertificateStatus(ctx)
	if r.CA.Level != "warn" || r.CA.State != "expiring" {
		t.Fatalf("CA with 300 days left: %+v", r.CA)
	}
}

func TestServerSANsAndServerCertRenewal(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cert := func() *x509.Certificate {
		b, _ := os.ReadFile(filepath.Join(e.svc.StateDir, "server.crt"))
		c, err := pki.ParseCertPEM(b)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first := cert()
	sans, err := e.svc.ServerSANs(ctx)
	if err != nil || len(sans) < 3 { // bootstrap default: localhost, 127.0.0.1, ::1, hostname
		t.Fatalf("default SANs from the bootstrap certificate: %v %v", sans, err)
	}
	if issued, err := e.svc.EnsureServerCert(ctx); err != nil || issued {
		t.Fatalf("nothing to do: %v %v", issued, err)
	}
	for _, bad := range [][]string{{}, {"not a name"}, {"-x.example"}, {"a/b"}} {
		if err := e.svc.SetServerSANs(ctx, bad); !errors.Is(err, service.ErrBadRequest) {
			t.Fatalf("%v accepted: %v", bad, err)
		}
	}
	if err := e.svc.SetServerSANs(ctx, []string{"Backup.Example.", "10.0.0.5", "backup.example", "::1"}); err != nil {
		t.Fatal(err)
	}
	got := cert()
	if pki.Fingerprint(got) == pki.Fingerprint(first) || len(got.DNSNames) != 1 || got.DNSNames[0] != "backup.example" || len(got.IPAddresses) != 2 {
		t.Fatalf("reissued certificate: %v %v", got.DNSNames, got.IPAddresses)
	}
	if sans, _ := e.svc.ServerSANs(ctx); len(sans) != 3 {
		t.Fatalf("stored SANs: %v", sans)
	}
	// renewal 45 days before expiry, keeping the stored SANs
	e.now = got.NotAfter.Add(-46 * day)
	if issued, _ := e.svc.EnsureServerCert(ctx); issued {
		t.Fatal("renewed too early")
	}
	e.now = got.NotAfter.Add(-44 * day)
	if issued, err := e.svc.EnsureServerCert(ctx); err != nil || !issued {
		t.Fatalf("not renewed: %v", err)
	}
	if n := cert(); n.NotAfter.Before(got.NotAfter.Add(600*day)) || len(n.DNSNames) != 1 || n.DNSNames[0] != "backup.example" {
		t.Fatalf("renewed: %v %v", n.NotAfter, n.DNSNames)
	}
}

// CA rotation: the server keeps presenting the old CA while every identity renews
// from the new one; promotion is refused until then, and afterwards the old CA's
// certificates are no longer accepted.
func TestCARotation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	oldFP, _ := e.svc.CAFingerprint()
	_, a := e.enrolCert("benfica", api.ScopeBackup)
	_, p := e.enrolCert("peer", api.ScopeSyncRead)
	if err := e.svc.PromoteCA(ctx, false); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("promote without a rotation: %v", err)
	}
	newFP, err := e.svc.RotateCA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RotateCA(ctx); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("second rotation: %v", err)
	}
	if fp, _ := e.svc.CAFingerprint(); fp != oldFP {
		t.Fatal("the presented CA (tokens) must not change before promotion")
	}
	// existing certificates keep working; renewals come from the new CA with the bundle
	if _, err := e.auth(a); err != nil {
		t.Fatal(err)
	}
	b, resp, err := e.renew(a)
	if err != nil {
		t.Fatal(err)
	}
	bundle, _ := pki.ParseCertsPEM([]byte(resp.CA))
	if len(bundle) != 2 || pki.Fingerprint(bundle[0]) != oldFP || pki.Fingerprint(bundle[1]) != newFP || resp.SigningCA != newFP {
		t.Fatalf("bundle: %d %s", len(bundle), resp.SigningCA)
	}
	if b.CheckSignatureFrom(bundle[1]) != nil {
		t.Fatal("renewed certificate not signed by the new CA")
	}
	st, _ := e.svc.CertificateStatus(ctx)
	if st.Rotation == nil || len(st.Rotation.Pending) != 2 { // b never used yet, peer not renewed
		t.Fatalf("pending: %+v", st.Rotation)
	}
	if err := e.svc.PromoteCA(ctx, false); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("promotion with pending identities: %v", err)
	}
	if _, err := e.auth(b); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Hour)
	q, _, err := e.renew(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.auth(q); err != nil {
		t.Fatal(err)
	}
	st, _ = e.svc.CertificateStatus(ctx)
	if len(st.Rotation.Pending) != 0 {
		t.Fatalf("still pending: %v", st.Rotation.Pending)
	}
	if err := e.svc.PromoteCA(ctx, false); err != nil {
		t.Fatal(err)
	}
	if fp, _ := e.svc.CAFingerprint(); fp != newFP {
		t.Fatal("the new CA is not presented after promotion")
	}
	sb, _ := os.ReadFile(filepath.Join(e.svc.StateDir, "server.crt"))
	sc, _ := pki.ParseCertPEM(sb)
	if sc.CheckSignatureFrom(bundle[1]) != nil {
		t.Fatal("server certificate not reissued from the new CA")
	}
	if _, err := e.auth(b); err != nil {
		t.Fatalf("new-CA certificate after promotion: %v", err)
	}
	if _, err := e.auth(a); !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("old-CA certificate after promotion: %v", err)
	}
	st, _ = e.svc.CertificateStatus(ctx)
	if st.Rotation != nil || st.CA.Fingerprint != newFP {
		t.Fatalf("status after promotion: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(e.svc.StateDir, "ca-retired-"+oldFP[:16]+".key")); err != nil {
		t.Fatal("old CA not archived")
	}
	// a new enrolment pins the new CA
	tok, _ := e.svc.CreateToken(ctx, "custody", "new", api.ScopeBackup, time.Hour)
	if fp, _, _ := service.ParseToken(tok); fp != newFP {
		t.Fatal("token pins the old CA")
	}
}
