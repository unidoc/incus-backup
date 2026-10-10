package remote

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/pki"
)

// Renew asks for a new certificate for the identity of the presented one. It is
// deliberately not retried: a renewal whose answer was lost is retried later by the
// caller's own backoff (the server accepts the old certificate once more for that).
func (c *Client) Renew(ctx context.Context, csrPEM []byte) (*api.RenewResponse, error) {
	b, err := json.Marshal(api.RenewRequest{CSR: string(csrPEM)})
	if err != nil {
		return nil, err
	}
	var r api.RenewResponse
	return &r, c.doOnce(ctx, "POST", "/v1/renew", bytes.NewReader(b), "application/json", &r, nil, nil)
}

// ConfirmRenewal tells the server the new certificate works: it revokes the old ones.
func (c *Client) ConfirmRenewal(ctx context.Context) (int, error) {
	var r api.RenewConfirmResponse
	err := retry(ctx, func() error { return c.do(ctx, "POST", "/v1/renew/confirm", api.RenewConfirm{}, &r, nil) })
	return r.Revoked, err
}

// CertInfo returns what the server knows about the presented certificate.
func (c *Client) CertInfo(ctx context.Context) (*api.CertInfo, error) {
	var r api.CertInfo
	return &r, retry(ctx, func() error { return c.do(ctx, "GET", "/v1/cert", nil, &r, nil) })
}

// ---- credential directories ------------------------------------------------------

// A credential directory holds ca.crt (the trust bundle), client.crt, client.key
// and renew.json; client.crt.prev/client.key.prev are the previous pair.

// LoadCredentials reads a credential directory (repairing a renewal interrupted
// between its two renames, see pki.LoadPair).
func LoadCredentials(dir string) (caPEM, certPEM, keyPEM []byte, err error) {
	caPEM, err = os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, nil, nil, err
	}
	certPEM, keyPEM, err = pki.LoadPair(filepath.Join(dir, "client"))
	return caPEM, certPEM, keyPEM, err
}

// NewFromDir builds a client from a credential directory; it re-reads the directory
// when the server refuses its certificate (another process renewed it).
func NewFromDir(addr, dir, domain string) (*Client, error) {
	ca, crt, key, err := LoadCredentials(dir)
	if err != nil {
		return nil, err
	}
	c, err := New(addr, ca, crt, key, domain)
	if err != nil {
		return nil, err
	}
	c.Reload = func() ([]byte, []byte, []byte, error) { return LoadCredentials(dir) }
	return c, nil
}

// RenewState is the renewal bookkeeping of one credential directory (renew.json).
type RenewState struct {
	LastCheck   time.Time `json:"last_check"`
	LastAttempt time.Time `json:"last_attempt"`
	LastSuccess time.Time `json:"last_success"`
	NextAttempt time.Time `json:"next_attempt"` // backoff after a failure
	Failures    int       `json:"failures"`
	LastError   string    `json:"last_error"`
	// ConfirmPending: a new certificate is installed but the server has not been told
	// it works yet (the old one stays valid until then or until it expires).
	ConfirmPending bool `json:"confirm_pending"`
}

// LoadRenewState reads renew.json (zero state when absent or unreadable).
func LoadRenewState(dir string) RenewState {
	var st RenewState
	if b, err := os.ReadFile(filepath.Join(dir, "renew.json")); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func saveRenewState(dir string, st RenewState) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	return pki.WriteFileAtomic(filepath.Join(dir, "renew.json"), b, 0o600)
}

// Backoff is the wait after the n-th consecutive failure: 1h, 2h, 4h, ... at most 24h.
func Backoff(n int) time.Duration {
	d := time.Hour
	for i := 1; i < n && d < 24*time.Hour; i++ {
		d *= 2
	}
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	return d
}

// Renewer keeps the certificate of one credential directory current.
type Renewer struct {
	Dir    string
	Client *Client // switched to the new credentials after a renewal
	Now    func() time.Time
	Before time.Duration // renew when less than this remains (default pki.RenewBefore)
	Log    *slog.Logger
}

// RenewOutcome says what Check did.
type RenewOutcome struct {
	Renewed   bool
	Confirmed bool
	Skipped   string // why nothing was attempted ("" when an attempt was made)
	NotAfter  time.Time
	Reason    string // why a renewal was due
}

func (r *Renewer) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Renewer) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

// lock serialises renewals of one directory across processes (daemon and CLI).
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".renew.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// issuerIn returns the fingerprint of the CA in bundle that signed cert ("" if none).
func issuerIn(cert *x509.Certificate, bundle []*x509.Certificate) string {
	for _, ca := range bundle {
		if cert.CheckSignatureFrom(ca) == nil {
			return pki.Fingerprint(ca)
		}
	}
	return ""
}

// Check renews the certificate when less than Before remains, when the server signs
// with a CA other than the one that signed it (CA rotation), or when force is set.
// The flow: new key + CSR -> POST /v1/renew -> write ca.crt and the pair atomically
// (previous pair kept as .prev) -> switch the client -> GET /v1/cert with the new
// certificate -> POST /v1/renew/confirm. A refused new certificate is rolled back;
// failures back off (Backoff) unless force is set.
func (r *Renewer) Check(ctx context.Context, force bool) (*RenewOutcome, error) {
	unlock, err := lockDir(r.Dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// another process may have renewed meanwhile: work from what is on disk
	if r.Client.reloadCreds() {
		r.log().Info("certificate reloaded from disk", "dir", r.Dir)
	}
	now := r.now()
	st := LoadRenewState(r.Dir)
	caPEM, certPEM, keyPEM, err := LoadCredentials(r.Dir)
	if err != nil {
		return nil, err
	}
	cert, err := pki.ParseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	bundle, err := pki.ParseCertsPEM(caPEM)
	if err != nil {
		return nil, err
	}
	out := &RenewOutcome{NotAfter: cert.NotAfter}
	if st.ConfirmPending {
		if err := r.confirm(ctx, &st, cert); err != nil {
			r.log().Warn("confirming the renewed certificate failed; will retry", "dir", r.Dir, "err", err)
		} else {
			out.Confirmed = true
		}
	}
	before := r.Before
	if before == 0 {
		before = pki.RenewBefore
	}
	left := cert.NotAfter.Sub(now)
	switch {
	case force:
		out.Reason = "requested"
	case left < before:
		out.Reason = fmt.Sprintf("%d days left", int(left.Hours()/24))
	default:
		var info api.CertInfo // one attempt: an unreachable server must not delay the caller
		if err := r.Client.doOnce(ctx, "GET", "/v1/cert", nil, "", &info, nil, nil); err == nil && info.SigningCA != "" && info.SigningCA != issuerIn(cert, bundle) {
			out.Reason = "the server signs with a new CA (rotation)"
		}
	}
	st.LastCheck = now
	if out.Reason == "" {
		out.Skipped = "not due"
		return out, saveRenewState(r.Dir, st)
	}
	if !force && now.Before(st.NextAttempt) {
		out.Skipped = "backing off after a failure until " + st.NextAttempt.Format(time.RFC3339)
		return out, saveRenewState(r.Dir, st)
	}
	st.LastAttempt = now
	nc, err := r.renew(ctx, caPEM, certPEM, keyPEM, cert, bundle, &st)
	if err != nil {
		st.Failures++
		st.LastError = err.Error()
		st.NextAttempt = now.Add(Backoff(st.Failures))
		_ = saveRenewState(r.Dir, st)
		return out, err
	}
	out.Renewed, out.NotAfter = true, nc.NotAfter
	out.Confirmed = !st.ConfirmPending
	st.Failures, st.LastError, st.NextAttempt, st.LastSuccess = 0, "", time.Time{}, now
	return out, saveRenewState(r.Dir, st)
}

func (r *Renewer) confirm(ctx context.Context, st *RenewState, cert *x509.Certificate) error {
	info, err := r.Client.CertInfo(ctx)
	if err != nil {
		return err
	}
	if info.Fingerprint != pki.Fingerprint(cert) || !info.Current {
		return fmt.Errorf("the server does not see the renewed certificate as current")
	}
	if _, err := r.Client.ConfirmRenewal(ctx); err != nil {
		return err
	}
	st.ConfirmPending = false
	return nil
}

func (r *Renewer) renew(ctx context.Context, oldCA, oldCert, oldKey []byte, cert *x509.Certificate, bundle []*x509.Certificate, st *RenewState) (*x509.Certificate, error) {
	keyPEM, csrPEM, err := pki.NewKeyAndCSR(cert.Subject.CommonName)
	if err != nil {
		return nil, err
	}
	resp, err := r.Client.Renew(ctx, csrPEM)
	if err != nil {
		return nil, fmt.Errorf("renewal request: %w", err)
	}
	newCert := []byte(resp.Certificate)
	nc, err := pki.ParseCertPEM(newCert)
	if err != nil {
		return nil, fmt.Errorf("the server returned an unreadable certificate: %w", err)
	}
	if !pki.PairMatches(newCert, keyPEM) {
		return nil, errors.New("the server returned a certificate for a different key")
	}
	newBundle, err := pki.ParseCertsPEM([]byte(resp.CA))
	if err != nil {
		return nil, fmt.Errorf("the server returned an unreadable CA bundle: %w", err)
	}
	// the new bundle must keep at least one CA we trust (the one the server presents),
	// and must have signed the new certificate
	known := map[string]bool{}
	for _, c := range bundle {
		known[pki.Fingerprint(c)] = true
	}
	overlap := false
	for _, c := range newBundle {
		overlap = overlap || known[pki.Fingerprint(c)]
	}
	if !overlap || issuerIn(nc, newBundle) == "" {
		return nil, errors.New("the server returned a CA bundle unrelated to the pinned CA, or one that did not sign the new certificate")
	}
	base := filepath.Join(r.Dir, "client")
	rollback := func(why error) error {
		e1 := pki.WriteFileAtomic(filepath.Join(r.Dir, "ca.crt"), oldCA, 0o600)
		e2 := pki.ReplacePair(base, oldCert, oldKey)
		e3 := r.Client.SetCredentials(oldCA, oldCert, oldKey)
		if err := errors.Join(e1, e2, e3); err != nil {
			return fmt.Errorf("%w (and restoring the previous certificate failed: %v)", why, err)
		}
		return why
	}
	if !bytes.Equal(oldCA, []byte(resp.CA)) {
		_ = pki.WriteFileAtomic(filepath.Join(r.Dir, "ca.crt.prev"), oldCA, 0o600)
		if err := pki.WriteFileAtomic(filepath.Join(r.Dir, "ca.crt"), []byte(resp.CA), 0o600); err != nil {
			return nil, err
		}
	}
	if err := pki.ReplacePair(base, newCert, keyPEM); err != nil {
		return nil, rollback(err)
	}
	if err := r.Client.SetCredentials([]byte(resp.CA), newCert, keyPEM); err != nil {
		return nil, rollback(err)
	}
	r.log().Info("certificate renewed", "dir", r.Dir, "not_after", nc.NotAfter)
	// prove the new certificate works on a fresh connection before revoking the old one
	info, err := r.Client.CertInfo(ctx)
	switch {
	case err != nil && (IsStatus(err, http.StatusUnauthorized) || IsStatus(err, http.StatusForbidden)):
		return nil, rollback(fmt.Errorf("the server refused the renewed certificate: %w", err))
	case err == nil && info.Fingerprint != pki.Fingerprint(nc):
		return nil, rollback(errors.New("the server saw a different certificate than the renewed one"))
	case err != nil:
		// unknown outcome (network): keep the new certificate, the old one stays valid; confirm later
		st.ConfirmPending = true
		r.log().Warn("could not verify the renewed certificate yet; will confirm later", "err", err)
		return nc, nil
	}
	if _, err := r.Client.ConfirmRenewal(ctx); err != nil {
		st.ConfirmPending = true
		r.log().Warn("confirming the renewed certificate failed; will retry", "err", err)
		return nc, nil
	}
	st.ConfirmPending = false
	return nc, nil
}
