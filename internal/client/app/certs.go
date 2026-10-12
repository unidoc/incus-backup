package app

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"time"

	"github.com/unidoc/incus-backup/internal/client/config"
	"github.com/unidoc/incus-backup/internal/pki"
	"github.com/unidoc/incus-backup/internal/remote"
)

// CertCheckEvery is how often the daemon looks at each client certificate (it also
// looks at start). A failed renewal is retried after its backoff instead.
const CertCheckEvery = 24 * time.Hour

func (a *App) initRenewers() {
	a.renewers = map[string]*remote.Renewer{}
	a.certCheck = map[string]time.Time{}
	for name, t := range a.Eng.Targets {
		a.renewers[name] = &remote.Renewer{Dir: a.Cfg.ServerDir(name), Client: t.Client, Now: func() time.Time { return a.Eng.Now() }, Log: a.Log.With("server", name)}
	}
}

// RenewDue renews, for every configured server independently, the client
// certificate when less than 45 days remain (or the server rotated its CA). It
// checks at start and then daily; a failure is retried after its backoff.
func (a *App) RenewDue(ctx context.Context) {
	now := a.Eng.Now()
	for _, name := range a.Cfg.ServerNames() {
		r := a.renewers[name]
		if r == nil || now.Before(a.certCheck[name]) {
			continue
		}
		out, err := r.Check(ctx, false)
		next := now.Add(CertCheckEvery)
		switch {
		case err != nil:
			a.Log.Error("client certificate renewal failed", "server", name, "err", err)
			if st := remote.LoadRenewState(r.Dir); st.NextAttempt.After(now) && st.NextAttempt.Before(next) {
				next = st.NextAttempt
			}
		case out.Renewed:
			a.Log.Info("client certificate renewed", "server", name, "reason", out.Reason, "not_after", out.NotAfter, "confirmed", out.Confirmed)
		}
		if st := remote.LoadRenewState(r.Dir); st.ConfirmPending && now.Add(time.Hour).Before(next) {
			next = now.Add(time.Hour)
		}
		a.certCheck[name] = next
	}
}

// RenewNow renews the certificate for one server (or all when name is "") right
// away, ignoring the threshold and the backoff.
func RenewNow(ctx context.Context, cfg *config.Config, name string, now func() time.Time, log *slog.Logger) (map[string]*remote.RenewOutcome, error) {
	out := map[string]*remote.RenewOutcome{}
	for _, n := range cfg.ServerNames() {
		if name != "" && n != name {
			continue
		}
		cl, err := remote.NewFromDir(cfg.Servers[n].Address, cfg.ServerDir(n), "")
		if err != nil {
			return out, fmt.Errorf("server %s: %w", n, err)
		}
		r := &remote.Renewer{Dir: cfg.ServerDir(n), Client: cl, Now: now, Log: log}
		o, err := r.Check(ctx, true)
		if err != nil {
			return out, fmt.Errorf("server %s: %w", n, err)
		}
		out[n] = o
	}
	if name != "" && len(out) == 0 {
		return nil, fmt.Errorf("no server called %q is configured", name)
	}
	return out, nil
}

// CertStatus is the state of one client certificate.
type CertStatus struct {
	Server   string
	NotAfter time.Time
	DaysLeft int
	State    string // ok | renew-due | expired | missing
	Issuer   string // CN of the CA that signed it
	Err      error  // unreadable files
	remote.RenewState
}

// CertStatuses reads every server's certificate and renewal state (no network).
func CertStatuses(cfg *config.Config, now time.Time) []CertStatus {
	var out []CertStatus
	for _, n := range cfg.ServerNames() {
		dir := cfg.ServerDir(n)
		cs := CertStatus{Server: n, RenewState: remote.LoadRenewState(dir), State: "ok"}
		ca, crt, _, err := remote.LoadCredentials(dir)
		var c *x509Cert
		if err == nil {
			c, err = parseWithIssuer(crt, ca)
		}
		if err != nil {
			cs.State, cs.Err = "missing", err
			out = append(out, cs)
			continue
		}
		cs.NotAfter, cs.Issuer = c.cert.NotAfter, c.issuer
		left := c.cert.NotAfter.Sub(now)
		cs.DaysLeft = int(left.Hours() / 24)
		switch {
		case left <= 0:
			cs.State = "expired"
		case left < pki.RenewBefore:
			cs.State = "renew-due"
		}
		out = append(out, cs)
	}
	return out
}

// CertChecks are the doctor findings about client certificates.
func CertChecks(cfg *config.Config, now time.Time) []Check {
	var out []Check
	for _, cs := range CertStatuses(cfg, now) {
		name := "certificate " + cs.Server
		fail := ""
		if cs.LastError != "" {
			fail = fmt.Sprintf("; the last renewal attempt (%s, %d in a row) failed: %s; next attempt %s",
				cs.LastAttempt.Format("2006-01-02 15:04"), cs.Failures, cs.LastError, cs.NextAttempt.Format("2006-01-02 15:04"))
		}
		switch {
		case cs.State == "missing":
			out = append(out, Check{name, "fail", fmt.Sprintf("cannot read the client certificate: %v", cs.Err)})
		case cs.State == "expired" && now.Sub(cs.NotAfter) > pki.RenewGrace:
			out = append(out, Check{name, "fail", fmt.Sprintf("the client certificate expired on %s, more than %d days ago: create a new token on the server and enrol again (`incus-backup init --server ... --token ...` with a new --name)",
				cs.NotAfter.Format("2006-01-02"), int(pki.RenewGrace.Hours()/24))})
		case cs.State == "expired":
			out = append(out, Check{name, "fail", fmt.Sprintf("the client certificate expired on %s; it can still be renewed until %s: run `incus-backup cert renew`%s",
				cs.NotAfter.Format("2006-01-02"), cs.NotAfter.Add(pki.RenewGrace).Format("2006-01-02"), fail)})
		case cs.LastError != "" && cs.State == "renew-due":
			out = append(out, Check{name, "warn", fmt.Sprintf("expires on %s (%d days) and renewal is failing%s", cs.NotAfter.Format("2006-01-02"), cs.DaysLeft, fail)})
		case cs.ConfirmPending:
			out = append(out, Check{name, "warn", fmt.Sprintf("renewed (valid until %s) but the server has not confirmed it yet; the daemon retries hourly", cs.NotAfter.Format("2006-01-02"))})
		case cs.State == "renew-due":
			out = append(out, Check{name, "info", fmt.Sprintf("expires on %s (%d days): the daemon renews it automatically (or run `incus-backup cert renew`)", cs.NotAfter.Format("2006-01-02"), cs.DaysLeft)})
		case cs.LastError != "":
			out = append(out, Check{name, "warn", fmt.Sprintf("valid until %s (%d days)%s", cs.NotAfter.Format("2006-01-02"), cs.DaysLeft, fail)})
		default:
			out = append(out, Check{name, "ok", fmt.Sprintf("valid until %s (%d days), renewed automatically %d days before expiry", cs.NotAfter.Format("2006-01-02"), cs.DaysLeft, int(pki.RenewBefore.Hours()/24))})
		}
	}
	return out
}

// CertNotes are one-line warnings for `status` (nothing when every certificate is fine).
func CertNotes(cfg *config.Config, now time.Time) []string {
	var out []string
	for _, c := range CertChecks(cfg, now) {
		if c.Status == "fail" || c.Status == "warn" {
			out = append(out, c.Name+": "+c.Detail)
		}
	}
	return out
}

type x509Cert struct {
	cert   *x509.Certificate
	issuer string
}

func parseWithIssuer(crt, ca []byte) (*x509Cert, error) {
	c, err := pki.ParseCertPEM(crt)
	if err != nil {
		return nil, err
	}
	out := &x509Cert{cert: c, issuer: c.Issuer.CommonName}
	if cas, err := pki.ParseCertsPEM(ca); err == nil {
		for _, a := range cas {
			if c.CheckSignatureFrom(a) == nil {
				out.issuer = a.Subject.CommonName + " " + pki.Fingerprint(a)[:16]
			}
		}
	}
	return out, nil
}
