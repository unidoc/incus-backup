package service

// Certificate lifecycle: the private CA, the server certificate (renewed and
// hot-reloaded), host certificates as a SET (renewal with overlap, confirmation,
// revocation), the status report and the CA rotation. docs/certificates.md is the
// design; this file is its only implementation.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/pki"
	"github.com/unidoc/incus-backup/internal/remote"
)

// ErrTooManyRequests is returned when an identity renews again too soon.
var ErrTooManyRequests = errors.New("too many requests")

// ErrCertExpired: the certificate expired too long ago to renew; the host must enrol again.
var ErrCertExpired = errors.New("certificate expired")

// CertOptions tune the lifecycle; zero values mean the defaults.
type CertOptions struct {
	// RenewMinInterval: an identity renews at most once per interval (default 10 minutes).
	RenewMinInterval time.Duration
}

func (o CertOptions) renewMinInterval() time.Duration {
	if o.RenewMinInterval > 0 {
		return o.RenewMinInterval
	}
	return 10 * time.Minute
}

// Status thresholds.
const (
	WarnBefore     = 60 * 24 * time.Hour
	CriticalBefore = 14 * 24 * time.Hour
	CAWarnBefore   = 365 * 24 * time.Hour
	CACritBefore   = 90 * 24 * time.Hour
)

const pkiLockKey = int64(0x69626b5f706b6931) // "ibk_pki1"

// State-directory files.
const (
	fileCA       = "ca"      // ca.crt / ca.key: the CA the server presents and tokens pin
	fileCANext   = "ca-next" // ca-next.crt / ca-next.key: during a rotation, the CA that signs client certificates
	fileServer   = "server"  // server.crt / server.key
	settingsSANs = "sans"
)

// pkiSnap is one consistent view of the PKI files.
type pkiSnap struct {
	ca     *pki.CA
	next   *pki.CA // nil unless a rotation is under way
	roots  *x509.CertPool
	server *x509.Certificate
	creds  *pki.ServerCreds
}

// signer is the CA that signs client certificates now.
func (p *pkiSnap) signer() *pki.CA {
	if p.next != nil {
		return p.next
	}
	return p.ca
}

// bundle is the trust bundle handed to clients: the presenting CA first.
func (p *pkiSnap) bundle() []byte {
	if p.next == nil {
		return p.ca.CertPEM
	}
	return pki.EncodeCertsPEM(p.ca.Cert, p.next.Cert)
}

type certState struct {
	mu        sync.Mutex
	stamp     string
	lastCheck time.Time
	cur       atomic.Pointer[pkiSnap]
}

func (s *Service) pkiFiles() []string {
	var out []string
	for _, b := range []string{fileCA, fileCANext, fileServer} {
		out = append(out, s.stateFile(b+".crt"), s.stateFile(b+".key"))
	}
	return out
}

func (s *Service) pkiStamp() string {
	var b strings.Builder
	for _, f := range s.pkiFiles() {
		if fi, err := os.Stat(f); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", f, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return b.String()
}

// pki returns the current PKI view, re-reading the files when any of them changed
// (another process, e.g. a CLI `ca sans set`, may have replaced them). A failed
// re-read keeps the previous view: a half-written state must never take TLS down.
func (s *Service) pki() (*pkiSnap, error) {
	st := &s.pkiState
	st.mu.Lock()
	defer st.mu.Unlock()
	return s.refreshLocked()
}

func (s *Service) refreshLocked() (*pkiSnap, error) {
	st := &s.pkiState
	stamp := s.pkiStamp()
	cur := st.cur.Load()
	if cur != nil && stamp == st.stamp {
		return cur, nil
	}
	snap, err := s.loadPKI()
	if err != nil {
		if cur != nil {
			s.Log.Error("re-reading the PKI files failed; keeping the loaded certificates", "err", err)
			return cur, nil
		}
		return nil, err
	}
	st.stamp = stamp
	st.cur.Store(snap)
	if cur != nil && (pki.Fingerprint(cur.server) != pki.Fingerprint(snap.server) || pki.Fingerprint(cur.ca.Cert) != pki.Fingerprint(snap.ca.Cert)) {
		s.Log.Info("server certificate reloaded", "fingerprint", pki.Fingerprint(snap.server), "not_after", snap.server.NotAfter, "sans", pki.SANs(snap.server))
	}
	return snap, nil
}

func (s *Service) loadCAFiles(base string) (*pki.CA, error) {
	c, k, err := pki.LoadPair(s.stateFile(base))
	if err != nil {
		return nil, err
	}
	return pki.LoadCA(c, k)
}

func (s *Service) loadPKI() (*pkiSnap, error) {
	ca, err := s.loadCAFiles(fileCA)
	if err != nil {
		return nil, fmt.Errorf("reading CA (run `incus-backup-server init` first): %w", err)
	}
	p := &pkiSnap{ca: ca, roots: x509.NewCertPool()}
	p.roots.AddCert(ca.Cert)
	if _, err := os.Stat(s.stateFile(fileCANext + ".crt")); err == nil {
		next, err := s.loadCAFiles(fileCANext)
		if err != nil {
			return nil, fmt.Errorf("reading the next CA: %w", err)
		}
		if pki.Fingerprint(next.Cert) != pki.Fingerprint(ca.Cert) { // equal = an interrupted promotion
			p.next = next
			p.roots.AddCert(next.Cert)
		}
	}
	certPEM, keyPEM, err := pki.LoadPair(s.stateFile(fileServer))
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if p.server, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
		return nil, err
	}
	// Present the CA certificate in the chain so clients can pin it at enrolment.
	pair.Certificate = append(pair.Certificate[:1], ca.Cert.Raw)
	pair.Leaf = p.server
	p.creds = &pki.ServerCreds{Cert: pair, Roots: p.roots}
	return p, nil
}

// creds is the TLS credential source: it re-checks the files at most once a second,
// so a certificate replaced by any process is served from the next handshake on.
func (s *Service) creds() *pki.ServerCreds {
	st := &s.pkiState
	st.mu.Lock()
	defer st.mu.Unlock()
	if time.Since(st.lastCheck) >= time.Second {
		st.lastCheck = time.Now()
		if p, err := s.refreshLocked(); err == nil {
			return p.creds
		}
	}
	return st.cur.Load().creds
}

// withPKILock serialises certificate issuing across processes (daemon and CLI).
func (s *Service) withPKILock(ctx context.Context, fn func() error) error {
	conn, err := s.DB.Pool().Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, pkiLockKey); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, pkiLockKey)
	return fn()
}

// InitPKI creates the CA and server certificate in StateDir (fails if present).
func (s *Service) InitPKI(sans []string) error {
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(s.stateFile("ca.crt")); err == nil {
		return errors.New("state directory already initialised")
	}
	ca, caKey, err := pki.NewCA("incus-backup server CA", s.Now())
	if err != nil {
		return err
	}
	if err := pki.WriteFileAtomic(s.stateFile("ca.key"), caKey, 0o600); err != nil {
		return err
	}
	if err := pki.WriteFileAtomic(s.stateFile("ca.crt"), ca.CertPEM, 0o644); err != nil {
		return err
	}
	if len(sans) == 0 {
		sans = []string{"localhost", "127.0.0.1", "::1"}
		if h, _ := os.Hostname(); h != "" {
			sans = append(sans, h)
		}
	}
	return s.writeServerCert(ca, sans)
}

func (s *Service) writeServerCert(ca *pki.CA, sans []string) error {
	cert, key, err := ca.NewServerCert("incus-backup-server", sans, s.Now(), pki.ServerValidity)
	if err != nil {
		return err
	}
	return pki.ReplacePair(s.stateFile(fileServer), cert, key)
}

// CAFingerprint returns the fingerprint clients pin (it is part of every token).
func (s *Service) CAFingerprint() (string, error) {
	p, err := s.pki()
	if err != nil {
		return "", err
	}
	return pki.Fingerprint(p.ca.Cert), nil
}

// ServerTLSConfig returns the TLS configuration of the API listener. The
// certificate is renewed first when needed; afterwards the configuration follows
// every renewal or SAN change without a restart (see CertLoop).
func (s *Service) ServerTLSConfig() (*tlsConfig, error) {
	if _, err := s.pki(); err != nil {
		return nil, err
	}
	if _, err := s.EnsureServerCert(context.Background()); err != nil {
		return nil, err
	}
	return pki.DynamicServerTLS(s.creds), nil
}

// ---- server certificate and SANs --------------------------------------------------

var dnsNameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}))*\.?$`)

func normSANs(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if ip := net.ParseIP(v); ip != nil {
			v = ip.String()
		} else if !dnsNameRe.MatchString(v) || len(v) > 253 {
			return nil, bad("%q is neither an IP address nor a DNS name", v)
		} else {
			v = strings.ToLower(strings.TrimSuffix(v, "."))
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, bad("at least one SAN is required")
	}
	return out, nil
}

// canonSANs normalises without judging (for comparisons and certificates issued earlier).
func canonSANs(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if ip := net.ParseIP(v); ip != nil {
			v = ip.String()
		} else {
			v = strings.ToLower(strings.TrimSuffix(v, "."))
		}
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func sameSet(a, b []string) bool {
	a, b = canonSANs(a), canonSANs(b)
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// ServerSANs returns the configured SANs: the stored setting, else the SANs of the
// current certificate (the bootstrap value given to `init`).
func (s *Service) ServerSANs(ctx context.Context) ([]string, error) {
	var raw string
	err := s.DB.Pool().QueryRow(ctx, `SELECT value FROM server_settings WHERE key=$1`, settingsSANs).Scan(&raw)
	if err == nil {
		var v []string
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, fmt.Errorf("server_settings.sans: %w", err)
		}
		return v, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	p, err := s.pki()
	if err != nil {
		return nil, err
	}
	return canonSANs(pki.SANs(p.server)), nil
}

// SetServerSANs stores the SAN list and reissues the server certificate with it.
// The running listener serves the new certificate from its next handshake on.
func (s *Service) SetServerSANs(ctx context.Context, sans []string) error {
	n, err := normSANs(sans)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(n)
	if _, err := s.DB.Pool().Exec(ctx, `INSERT INTO server_settings (key, value, updated_at) VALUES ($1,$2,$3)
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=EXCLUDED.updated_at`, settingsSANs, string(b), s.Now()); err != nil {
		return err
	}
	_, err = s.EnsureServerCert(ctx)
	return err
}

// serverCertReason says why the server certificate must be reissued ("" = it is fine).
func (s *Service) serverCertReason(p *pkiSnap, want []string) string {
	now := s.Now()
	c := p.server
	have := pki.SANs(c)
	switch {
	case c.NotAfter.Sub(now) < pki.RenewBefore:
		return fmt.Sprintf("it expires on %s", c.NotAfter.Format(time.DateOnly))
	case c.CheckSignatureFrom(p.ca.Cert) != nil:
		return "it was not issued by the current CA"
	case !sameSet(have, want):
		return fmt.Sprintf("the SANs changed from %v to %v", have, want)
	}
	return ""
}

// EnsureServerCert reissues the server certificate when it expires within
// pki.RenewBefore, when the SAN list changed or when the CA was rotated. It reports
// whether it issued one.
func (s *Service) EnsureServerCert(ctx context.Context) (bool, error) {
	p, err := s.pki()
	if err != nil {
		return false, err
	}
	want, err := s.ServerSANs(ctx)
	if err != nil {
		return false, err
	}
	if s.serverCertReason(p, want) == "" {
		return false, nil
	}
	issued := false
	err = s.withPKILock(ctx, func() error {
		p, err := s.pki() // another process may have done it meanwhile
		if err != nil {
			return err
		}
		why := s.serverCertReason(p, want)
		if why == "" {
			return nil
		}
		s.Log.Info("issuing a new server certificate", "reason", why, "sans", want)
		if err := s.writeServerCert(p.ca, want); err != nil {
			return err
		}
		issued = true
		_, err = s.pki()
		return err
	})
	return issued, err
}

// CertLoop keeps the server certificate current until ctx ends: it checks at start
// and then every minute (renewal at < 45 days, SAN edits, CA promotion), and picks up
// certificates replaced by other processes. Failures are logged and retried.
func (s *Service) CertLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if _, err := s.EnsureServerCert(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error("server certificate check failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- enrolment and authentication ---------------------------------------------------

// Enroll redeems a token: signs the CSR and registers the host. Re-enrolling an
// existing identity revokes all of its earlier certificates.
func (s *Service) Enroll(ctx context.Context, req api.EnrollRequest) (*api.EnrollResponse, error) {
	_, secret, err := ParseToken(req.Token)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	p, err := s.pki()
	if err != nil {
		return nil, err
	}
	now := s.Now()
	tx, err := s.DB.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var tokID, repoID, hostName, scope string
	err = tx.QueryRow(ctx, `SELECT id, repository_id, host_name, scope FROM enrollment_tokens
		WHERE token_hash=$1 AND used_at IS NULL AND expires_at > $2 FOR UPDATE`, tokenHash(secret), now).
		Scan(&tokID, &repoID, &hostName, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: token invalid, expired or already used", ErrUnauthorized)
	}
	if err != nil {
		return nil, err
	}
	signer := p.signer()
	certPEM, fp, err := signer.SignClient([]byte(req.CSR), hostName, now, pki.ClientValidity)
	if err != nil {
		return nil, bad("%v", err)
	}
	cert, _ := pki.ParseCertPEM(certPEM)
	info := req.Info
	if info == nil {
		info = map[string]any{}
	}
	var hostID string
	if err := tx.QueryRow(ctx, `INSERT INTO hosts (repository_id, name, scope, cert_fingerprint, info, enrolled_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (repository_id, name, scope) DO UPDATE SET cert_fingerprint=EXCLUDED.cert_fingerprint, info=EXCLUDED.info, disabled=false, enrolled_at=EXCLUDED.enrolled_at, last_renewed_at=NULL
		RETURNING id`,
		repoID, hostName, scope, fp, info, now).Scan(&hostID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE host_certs SET revoked=true, revoked_at=$2 WHERE host_id=$1 AND NOT revoked`, hostID, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_certs (fingerprint, host_id, not_before, not_after, created_at, issuer_fingerprint) VALUES ($1,$2,$3,$4,$5,$6)`,
		fp, hostID, cert.NotBefore, cert.NotAfter, now, pki.Fingerprint(signer.Cert)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE enrollment_tokens SET used_at=$2 WHERE id=$1`, tokID, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	repo, err := s.RepositoryByID(ctx, repoID)
	if err != nil {
		return nil, err
	}
	return &api.EnrollResponse{Certificate: string(certPEM), CA: string(p.bundle()), Host: hostName, Scope: scope, Repository: repo.Name}, nil
}

// certRow is a host certificate joined with its identity.
type certRow struct {
	host      Host
	current   string
	revoked   bool
	notAfter  time.Time
	parent    string
	firstUsed *time.Time
	confirmed *time.Time
	renewed   *time.Time
}

const certRowCols = `h.id, h.repository_id, h.name, h.scope, h.disabled, h.cert_fingerprint, h.last_renewed_at,
	hc.fingerprint, hc.revoked, hc.not_after, hc.parent_fingerprint, hc.first_used_at, hc.confirmed_at`

func scanCertRow(r pgx.Row) (*certRow, error) {
	c := &certRow{}
	err := r.Scan(&c.host.ID, &c.host.RepoID, &c.host.Name, &c.host.Scope, &c.host.Disabled, &c.current, &c.renewed,
		&c.host.Fingerprint, &c.revoked, &c.notAfter, &c.parent, &c.firstUsed, &c.confirmed)
	return c, err
}

// verifyIssued checks that cert was signed by a CA this server trusts (time aside).
func (s *Service) verifyIssued(cert *x509.Certificate) error {
	p, err := s.pki()
	if err != nil {
		return err
	}
	if err := pki.VerifyClientCert(cert, nil, p.roots); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return nil
}

// HostFromCert authenticates a client certificate: it must be signed by a trusted
// CA, be valid now (the service clock), be one of the identity's certificates and
// not revoked, and the identity must not be disabled. Disabling a host therefore
// rejects every certificate it holds.
func (s *Service) HostFromCert(ctx context.Context, cert *x509.Certificate, addr string) (*Host, error) {
	if err := s.verifyIssued(cert); err != nil {
		return nil, err
	}
	now := s.Now()
	if now.After(cert.NotAfter) {
		return nil, fmt.Errorf("%w: the certificate expired on %s", ErrUnauthorized, cert.NotAfter.Format(time.DateOnly))
	}
	if now.Before(cert.NotBefore) {
		return nil, fmt.Errorf("%w: the certificate is not valid yet", ErrUnauthorized)
	}
	fp := pki.Fingerprint(cert)
	c, err := scanCertRow(s.DB.Pool().QueryRow(ctx, `SELECT `+certRowCols+` FROM host_certs hc JOIN hosts h ON h.id=hc.host_id WHERE hc.fingerprint=$1`, fp))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if c.revoked {
		return nil, fmt.Errorf("%w: certificate revoked", ErrUnauthorized)
	}
	if c.host.Disabled {
		return nil, fmt.Errorf("%w: host disabled", ErrForbidden)
	}
	// rows migrated from before host_certs carry estimated dates: the certificate itself is authoritative
	// The write is rate limited: every authenticated request used to commit one PostgreSQL update on the same
	// host row (one per uploaded chunk). The certificate bookkeeping still happens at once when it is needed.
	needFix := c.firstUsed == nil || !c.notAfter.Equal(cert.NotAfter)
	v, seen := s.lastSeen.Load(c.host.ID)
	if needFix || !seen || v.(hostSeen).addr != addr || now.Sub(v.(hostSeen).at) >= hostSeenEvery || now.Before(v.(hostSeen).at) {
		s.lastSeen.Store(c.host.ID, hostSeen{at: now, addr: addr})
		_, _ = s.DB.Pool().Exec(ctx, `WITH u AS (UPDATE host_certs SET first_used_at=COALESCE(first_used_at,$2), not_before=$4, not_after=$5 WHERE fingerprint=$3
				AND (first_used_at IS NULL OR not_after<>$5 OR not_before<>$4))
			UPDATE hosts SET last_seen_at=$2, last_addr=$6 WHERE id=$1`, c.host.ID, now, fp, cert.NotBefore, cert.NotAfter, addr)
	}
	h := c.host
	return &h, nil
}

// CertInfo describes the presented certificate (GET /v1/cert).
func (s *Service) CertInfo(ctx context.Context, h *Host, cert *x509.Certificate) (*api.CertInfo, error) {
	p, err := s.pki()
	if err != nil {
		return nil, err
	}
	var cur string
	if err := s.DB.Pool().QueryRow(ctx, `SELECT cert_fingerprint FROM hosts WHERE id=$1`, h.ID).Scan(&cur); err != nil {
		return nil, err
	}
	fp := pki.Fingerprint(cert)
	return &api.CertInfo{Fingerprint: fp, NotAfter: cert.NotAfter, Current: fp == cur, SigningCA: pki.Fingerprint(p.signer().Cert), ServerNow: s.Now()}, nil
}

// Renew issues a new certificate for the identity of cert (POST /v1/renew).
//
//   - cert must be signed by a trusted CA, not revoked, of a host that is not
//     disabled, and must not have expired more than pki.RenewGrace ago;
//   - it must be the identity's CURRENT certificate, or the parent of a current
//     certificate that was never used or confirmed (the answer to a renewal was
//     lost: without this the host could never renew again);
//   - an identity renews at most once per CertOptions.RenewMinInterval (429), which
//     also makes concurrent renewals with one certificate yield exactly one winner;
//   - the new certificate becomes current; the presented one stays valid (overlap)
//     until it expires or the client confirms; any other pending certificate of the
//     identity is revoked, so an identity never holds more than two.
func (s *Service) Renew(ctx context.Context, cert *x509.Certificate, csrPEM []byte, addr string) (*api.RenewResponse, error) {
	if err := s.verifyIssued(cert); err != nil {
		return nil, err
	}
	now := s.Now()
	if now.After(cert.NotAfter.Add(pki.RenewGrace)) {
		return nil, fmt.Errorf("%w on %s, more than %d days ago: create a new token and enrol again", ErrCertExpired, cert.NotAfter.Format(time.DateOnly), int(pki.RenewGrace.Hours()/24))
	}
	if now.Before(cert.NotBefore) {
		return nil, fmt.Errorf("%w: the certificate is not valid yet", ErrUnauthorized)
	}
	p, err := s.pki()
	if err != nil {
		return nil, err
	}
	fp := pki.Fingerprint(cert)
	tx, err := s.DB.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	c, err := scanCertRow(tx.QueryRow(ctx, `SELECT `+certRowCols+` FROM host_certs hc JOIN hosts h ON h.id=hc.host_id WHERE hc.fingerprint=$1 FOR UPDATE OF h`, fp))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if c.revoked {
		return nil, fmt.Errorf("%w: certificate revoked", ErrUnauthorized)
	}
	if c.host.Disabled {
		return nil, fmt.Errorf("%w: host disabled", ErrForbidden)
	}
	if c.renewed != nil && now.Sub(*c.renewed) < s.Certs.renewMinInterval() {
		return nil, fmt.Errorf("%w: this identity renewed at %s; try again later", ErrTooManyRequests, c.renewed.UTC().Format(time.RFC3339))
	}
	if fp != c.current {
		cur, err := scanCertRow(tx.QueryRow(ctx, `SELECT `+certRowCols+` FROM host_certs hc JOIN hosts h ON h.id=hc.host_id WHERE hc.fingerprint=$1`, c.current))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err != nil || cur.parent != fp || cur.firstUsed != nil || cur.confirmed != nil {
			return nil, fmt.Errorf("%w: this certificate has been replaced by a newer one; use that one", ErrConflict)
		}
	}
	signer := p.signer()
	certPEM, newFP, err := signer.SignClient(csrPEM, c.host.Name, now, pki.ClientValidity)
	if err != nil {
		return nil, bad("%v", err)
	}
	nc, _ := pki.ParseCertPEM(certPEM)
	if _, err := tx.Exec(ctx, `UPDATE host_certs SET revoked=true, revoked_at=$3 WHERE host_id=$1 AND fingerprint<>$2 AND NOT revoked`, c.host.ID, fp, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_certs (fingerprint, host_id, not_before, not_after, created_at, parent_fingerprint, issuer_fingerprint) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		newFP, c.host.ID, nc.NotBefore, nc.NotAfter, now, fp, pki.Fingerprint(signer.Cert)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE hosts SET cert_fingerprint=$2, last_renewed_at=$3, last_seen_at=$3, last_addr=$4 WHERE id=$1`, c.host.ID, newFP, now, addr); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	repo, err := s.RepositoryByID(ctx, c.host.RepoID)
	if err != nil {
		return nil, err
	}
	s.Log.Info("certificate renewed", "host", c.host.Name, "scope", c.host.Scope, "repository", repo.Name, "not_after", nc.NotAfter)
	return &api.RenewResponse{Certificate: string(certPEM), CA: string(p.bundle()), NotAfter: nc.NotAfter, Host: c.host.Name, Scope: c.host.Scope,
		Repository: repo.Name, SigningCA: pki.Fingerprint(signer.Cert)}, nil
}

// ConfirmRenewal is called with the NEW certificate after it worked: every other
// certificate of the identity is revoked at once instead of at its expiry.
func (s *Service) ConfirmRenewal(ctx context.Context, h *Host) (int, error) {
	now := s.Now()
	tx, err := s.DB.Pool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var cur string
	if err := tx.QueryRow(ctx, `SELECT cert_fingerprint FROM hosts WHERE id=$1 FOR UPDATE`, h.ID).Scan(&cur); err != nil {
		return 0, err
	}
	if cur != h.Fingerprint {
		return 0, fmt.Errorf("%w: only the current certificate can confirm a renewal", ErrConflict)
	}
	ct, err := tx.Exec(ctx, `UPDATE host_certs SET revoked=true, revoked_at=$3 WHERE host_id=$1 AND fingerprint<>$2 AND NOT revoked`, h.ID, cur, now)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE host_certs SET confirmed_at=COALESCE(confirmed_at,$2) WHERE fingerprint=$1`, cur, now); err != nil {
		return 0, err
	}
	return int(ct.RowsAffected()), tx.Commit(ctx)
}

// ---- status -------------------------------------------------------------------------

// CertSummary describes one certificate in the status report.
type CertSummary struct {
	Subject     string    `json:"subject,omitempty"`
	Fingerprint string    `json:"fingerprint"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	DaysLeft    int       `json:"days_left"`
	State       string    `json:"state"` // ok | expiring | expired (hosts also: untrusted)
	Level       string    `json:"level"` // ok | warn | critical
}

func summarize(now, notBefore, notAfter time.Time, warn, crit time.Duration) CertSummary {
	left := notAfter.Sub(now)
	c := CertSummary{NotBefore: notBefore, NotAfter: notAfter, DaysLeft: int(left.Hours() / 24), State: "ok", Level: "ok"}
	switch {
	case left <= 0:
		c.State, c.Level = "expired", "critical"
	case left < crit:
		c.State, c.Level = "expiring", "critical"
	case left < warn:
		c.State, c.Level = "expiring", "warn"
	}
	return c
}

func certSummary(now time.Time, c *x509.Certificate, warn, crit time.Duration) CertSummary {
	s := summarize(now, c.NotBefore, c.NotAfter, warn, crit)
	s.Subject, s.Fingerprint = c.Subject.CommonName, pki.Fingerprint(c)
	return s
}

// HostCertStatus is one identity in the status report.
type HostCertStatus struct {
	Repository  string     `json:"repository"`
	Name        string     `json:"name"`
	Scope       string     `json:"scope"`
	Disabled    bool       `json:"disabled"`
	LastRenewed *time.Time `json:"last_renewed"`
	LastSeen    *time.Time `json:"last_seen"`
	// OtherValid counts certificates still accepted besides the current one (an unconfirmed renewal).
	OtherValid int `json:"other_valid"`
	// OnOldCA: during a CA rotation, the current certificate is not yet from the new CA (or never used).
	OnOldCA bool `json:"on_old_ca"`
	// Untrusted: issued by a CA the server no longer trusts; the identity must enrol again.
	Untrusted bool `json:"untrusted"`
	CertSummary
}

// PeerCertStatus is a credential THIS server holds at a sync peer.
type PeerCertStatus struct {
	Repository string `json:"repository"`
	Job        string `json:"job"`
	Error      string `json:"error,omitempty"`
	CertSummary
}

// RotationStatus describes a CA rotation under way.
type RotationStatus struct {
	Next CertSummary `json:"next"`
	// Pending lists identities that still need a renewal from the new CA before `ca promote`.
	Pending []string `json:"pending"`
}

// CertReport is the certificate status for the management UI and notifications.
type CertReport struct {
	Generated  time.Time        `json:"generated"`
	CA         CertSummary      `json:"ca"`
	Rotation   *RotationStatus  `json:"rotation,omitempty"`
	Server     CertSummary      `json:"server"`
	ServerSANs []string         `json:"server_sans"`
	WantSANs   []string         `json:"want_sans"`
	Hosts      []HostCertStatus `json:"hosts"`
	SyncPeers  []PeerCertStatus `json:"sync_peers"`
	Level      string           `json:"level"` // worst level of everything above
}

// peerCredentialStatus reads the sync credentials under StateDir/sync/<repo>/<job>/.
func (s *Service) peerCredentialStatus(now time.Time) []PeerCertStatus {
	out := []PeerCertStatus{}
	repos, _ := os.ReadDir(filepath.Join(s.StateDir, "sync"))
	for _, r := range repos {
		jobs, _ := os.ReadDir(filepath.Join(s.StateDir, "sync", r.Name()))
		for _, j := range jobs {
			dir := filepath.Join(s.StateDir, "sync", r.Name(), j.Name())
			ps := PeerCertStatus{Repository: r.Name(), Job: j.Name()}
			crt, _, err := pki.LoadPair(filepath.Join(dir, "client"))
			var c *x509.Certificate
			if err == nil {
				c, err = pki.ParseCertPEM(crt)
			}
			if err != nil {
				ps.Error = err.Error()
				ps.State, ps.Level = "expired", "critical"
			} else {
				ps.CertSummary = certSummary(now, c, WarnBefore, CriticalBefore)
			}
			if rs := remote.LoadRenewState(dir); rs.LastError != "" && ps.Error == "" {
				ps.Error = "last renewal attempt failed: " + rs.LastError
			}
			out = append(out, ps)
		}
	}
	return out
}

// CertificateStatus reports the CA, the server certificate, every identity and the
// sync-peer credentials with their expiry levels (warn < 60 days, critical < 14 days;
// CA warn < 1 year, critical < 90 days).
func (s *Service) CertificateStatus(ctx context.Context) (*CertReport, error) {
	p, err := s.pki()
	if err != nil {
		return nil, err
	}
	now := s.Now()
	r := &CertReport{Generated: now, CA: certSummary(now, p.ca.Cert, CAWarnBefore, CACritBefore), Server: certSummary(now, p.server, WarnBefore, CriticalBefore)}
	r.ServerSANs = canonSANs(pki.SANs(p.server))
	if r.WantSANs, err = s.ServerSANs(ctx); err != nil {
		return nil, err
	}
	if !sameSet(r.ServerSANs, r.WantSANs) && r.Server.Level == "ok" {
		r.Server.Level = "warn" // the daemon reissues it within a minute; if it stays, it is not running
	}
	var nextFP string
	trusted := map[string]bool{pki.Fingerprint(p.ca.Cert): true}
	if p.next != nil {
		trusted[pki.Fingerprint(p.next.Cert)] = true
		nextFP = pki.Fingerprint(p.next.Cert)
		r.Rotation = &RotationStatus{Next: certSummary(now, p.next.Cert, CAWarnBefore, CACritBefore), Pending: []string{}}
	}
	rows, err := s.DB.Pool().Query(ctx, `SELECT r.name, h.name, h.scope, h.disabled, h.last_renewed_at, h.last_seen_at,
			hc.fingerprint, hc.not_before, hc.not_after, hc.issuer_fingerprint, hc.first_used_at IS NOT NULL,
			(SELECT count(*) FROM host_certs o WHERE o.host_id=h.id AND o.fingerprint<>h.cert_fingerprint AND NOT o.revoked AND o.not_after > $1)
		FROM hosts h JOIN repositories r ON r.id=h.repository_id JOIN host_certs hc ON hc.fingerprint=h.cert_fingerprint
		ORDER BY r.name, h.name, h.scope`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h HostCertStatus
		var nb, na time.Time
		var issuer string
		var used bool
		if err := rows.Scan(&h.Repository, &h.Name, &h.Scope, &h.Disabled, &h.LastRenewed, &h.LastSeen, &h.Fingerprint, &nb, &na, &issuer, &used, &h.OtherValid); err != nil {
			return nil, err
		}
		fp := h.Fingerprint
		h.CertSummary = summarize(now, nb, na, WarnBefore, CriticalBefore)
		h.Fingerprint = fp
		if issuer != "" && !trusted[issuer] {
			// issued by a CA that is no longer trusted (`ca promote --force`): it cannot authenticate
			h.State, h.Level, h.Untrusted = "untrusted", "critical", true
		}
		if r.Rotation != nil && !h.Disabled && (issuer != nextFP || !used) {
			h.OnOldCA = true
			r.Rotation.Pending = append(r.Rotation.Pending, h.Repository+"/"+h.Name+"/"+h.Scope)
		}
		r.Hosts = append(r.Hosts, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	r.SyncPeers = s.peerCredentialStatus(now)
	r.Level = worst(r)
	return r, nil
}

func worst(r *CertReport) string {
	rank := map[string]int{"ok": 0, "warn": 1, "critical": 2}
	w := "ok"
	see := func(l string) {
		if rank[l] > rank[w] {
			w = l
		}
	}
	see(r.CA.Level)
	see(r.Server.Level)
	for _, h := range r.Hosts {
		if !h.Disabled {
			see(h.Level)
		}
	}
	for _, p := range r.SyncPeers {
		see(p.Level)
	}
	return w
}

// ---- CA rotation ------------------------------------------------------------------

// RotateCA starts a CA rotation (phase 1): a new CA is created and trusted next to
// the current one, and signs every client certificate issued from now on. The server
// keeps presenting the current CA, so every client keeps working; each client learns
// the new CA (trust bundle) with its next renewal, which clients do early once they
// see that the signing CA changed (GET /v1/cert). Finish with PromoteCA.
func (s *Service) RotateCA(ctx context.Context) (string, error) {
	var fp string
	err := s.withPKILock(ctx, func() error {
		p, err := s.pki()
		if err != nil {
			return err
		}
		if p.next != nil {
			return fmt.Errorf("%w: a CA rotation is already under way (next CA %s): finish it with `ca promote`", ErrConflict, pki.Fingerprint(p.next.Cert)[:16])
		}
		now := s.Now()
		ca, key, err := pki.NewCA("incus-backup server CA "+now.UTC().Format("2006-01"), now)
		if err != nil {
			return err
		}
		// certificates from before any rotation carry no issuer: they are the current CA's
		if _, err := s.DB.Pool().Exec(ctx, `UPDATE host_certs SET issuer_fingerprint=$1 WHERE issuer_fingerprint=''`, pki.Fingerprint(p.ca.Cert)); err != nil {
			return err
		}
		if err := pki.WriteFileAtomic(s.stateFile(fileCANext+".key"), key, 0o600); err != nil {
			return err
		}
		if err := pki.WriteFileAtomic(s.stateFile(fileCANext+".crt"), ca.CertPEM, 0o644); err != nil {
			return err
		}
		fp = pki.Fingerprint(ca.Cert)
		_, err = s.pki()
		return err
	})
	return fp, err
}

// PromoteCA finishes a rotation (phase 2): the new CA becomes the presented one,
// the server certificate is reissued from it, the old CA is archived and no longer
// trusted. It refuses while an enabled identity's current certificate is not from
// the new CA or was never used (that client may not have the new trust bundle and
// would stop trusting the server); force overrides that, cutting those clients off.
func (s *Service) PromoteCA(ctx context.Context, force bool) error {
	st, err := s.CertificateStatus(ctx)
	if err != nil {
		return err
	}
	if st.Rotation == nil {
		return fmt.Errorf("%w: no CA rotation is under way (start one with `ca rotate`)", ErrConflict)
	}
	if len(st.Rotation.Pending) > 0 && !force {
		return fmt.Errorf("%w: %d identity(ies) have not renewed from the new CA yet: %s", ErrConflict, len(st.Rotation.Pending), strings.Join(st.Rotation.Pending, ", "))
	}
	err = s.withPKILock(ctx, func() error {
		p, err := s.pki()
		if err != nil {
			return err
		}
		if p.next == nil {
			return nil
		}
		old := pki.Fingerprint(p.ca.Cert)
		oc, ok, err := pki.LoadPair(s.stateFile(fileCA))
		if err != nil {
			return err
		}
		arch := s.stateFile("ca-retired-" + old[:16])
		if err := pki.WriteFileAtomic(arch+".key", ok, 0o600); err != nil {
			return err
		}
		if err := pki.WriteFileAtomic(arch+".crt", oc, 0o644); err != nil {
			return err
		}
		nc, nk, err := pki.LoadPair(s.stateFile(fileCANext))
		if err != nil {
			return err
		}
		if err := pki.ReplacePair(s.stateFile(fileCA), nc, nk); err != nil {
			return err
		}
		// from here on ca == ca-next, which loadPKI treats as "no rotation"
		_ = os.Remove(s.stateFile(fileCA + ".crt.prev"))
		_ = os.Remove(s.stateFile(fileCA + ".key.prev"))
		if err := os.Remove(s.stateFile(fileCANext + ".crt")); err != nil {
			return err
		}
		_ = os.Remove(s.stateFile(fileCANext + ".key"))
		if err := pki.SyncDir(s.StateDir); err != nil {
			return err
		}
		p, err = s.pki()
		if err != nil {
			return err
		}
		want, err := s.ServerSANs(ctx)
		if err != nil {
			return err
		}
		if err := s.writeServerCert(p.ca, want); err != nil {
			return err
		}
		_, err = s.pki()
		s.Log.Info("CA rotation finished", "old", old[:16], "new", pki.Fingerprint(p.ca.Cert)[:16])
		return err
	})
	return err
}
