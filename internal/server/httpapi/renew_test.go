package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/pki"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/httpapi"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/testdb"
)

const day = 24 * time.Hour

// clock is a settable clock safe for concurrent readers.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func (c *clock) Set(t time.Time)     { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func newClock(t time.Time) *clock    { return &clock{t: t} }
func quietLog() *slog.Logger         { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func (f *cfix) addr() string         { return f.srv.Listener.Addr().String() }
func (f *cfix) newService() *service.Service {
	s := service.New(f.db, f.state, "test", quietLog())
	s.Now = f.clk.Now
	return s
}

type cfix struct {
	t     *testing.T
	svc   *service.Service
	srv   *httptest.Server
	clk   *clock
	db    *db.DB
	state string
}

// clockFixture is a server whose service runs on a controllable clock. wrap, when
// set, sits in front of the API handler.
func clockFixture(t *testing.T, wrap func(http.Handler) http.Handler) *cfix {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f := &cfix{t: t, clk: newClock(time.Now()), db: d, state: filepath.Join(dir, "state")}
	f.svc = f.newService()
	if err := f.svc.InitPKI([]string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateRepository(ctx, "custody", filepath.Join(dir, "ds")); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.svc.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler = (&httpapi.Server{Svc: f.svc, Log: quietLog()}).Handler()
	if wrap != nil {
		h = wrap(h)
	}
	f.srv = httptest.NewUnstartedServer(h)
	f.srv.TLS = cfg
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

// enrolDir enrols an identity and stores its credentials like a client does.
func (f *cfix) enrolDir(name, scope string) string {
	f.t.Helper()
	tok, err := f.svc.CreateToken(context.Background(), "custody", name, scope, time.Hour)
	if err != nil {
		f.t.Fatal(err)
	}
	en, err := remote.Enroll(context.Background(), f.addr(), tok, name, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	dir := filepath.Join(f.t.TempDir(), name)
	_ = os.MkdirAll(dir, 0o700)
	for n, b := range map[string][]byte{"ca.crt": en.CAPEM, "client.crt": en.CertPEM, "client.key": en.KeyPEM} {
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	return dir
}

func (f *cfix) clientFromDir(dir string) *remote.Client {
	f.t.Helper()
	c, err := remote.NewFromDir(f.addr(), dir, "")
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func readCert(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRenewOverHTTPOverlapAndConfirm(t *testing.T) {
	f := clockFixture(t, nil)
	ctx := context.Background()
	dir := f.enrolDir("benfica", api.ScopeBackup)
	oldCA, oldCert, oldKey, _ := remote.LoadCredentials(dir)
	c := f.clientFromDir(dir)
	r := &remote.Renewer{Dir: dir, Client: c, Now: f.clk.Now, Log: quietLog()}
	out, err := r.Check(ctx, false)
	if err != nil || out.Renewed || out.Skipped != "not due" {
		t.Fatalf("fresh certificate: %+v %v", out, err)
	}
	f.clk.Add(690 * day) // 40 days left
	out, err = r.Check(ctx, false)
	if err != nil || !out.Renewed || !out.Confirmed {
		t.Fatalf("renewal: %+v %v", out, err)
	}
	_, newCert, _, _ := remote.LoadCredentials(dir)
	if string(newCert) == string(oldCert) || string(readCert(t, filepath.Join(dir, "client.crt.prev"))) != string(oldCert) {
		t.Fatal("files not replaced, or no .prev backup")
	}
	if who, err := c.Whoami(ctx); err != nil || who.Host != "benfica" {
		t.Fatalf("renewed client: %+v %v", who, err)
	}
	// the confirmed renewal revoked the old certificate
	old, _ := remote.New(f.addr(), oldCA, oldCert, oldKey, "")
	if _, err := old.Whoami(ctx); !remote.IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("old certificate after confirmation: %v", err)
	}
	st := remote.LoadRenewState(dir)
	if st.LastSuccess.IsZero() || st.Failures != 0 || st.ConfirmPending {
		t.Fatalf("state: %+v", st)
	}
}

// Another process (the daemon) renews and confirms: a client that still holds the old
// certificate in memory reloads it from disk on its next 401 and carries on.
func TestClientReloadsCertificateRenewedByAnotherProcess(t *testing.T) {
	f := clockFixture(t, nil)
	ctx := context.Background()
	dir := f.enrolDir("benfica", api.ScopeBackup)
	cli := f.clientFromDir(dir)
	if _, err := cli.Whoami(ctx); err != nil {
		t.Fatal(err)
	}
	daemon := f.clientFromDir(dir)
	if out, err := (&remote.Renewer{Dir: dir, Client: daemon, Now: f.clk.Now}).Check(ctx, true); err != nil || !out.Confirmed {
		t.Fatalf("%+v %v", out, err)
	}
	if who, err := cli.Whoami(ctx); err != nil || who.Host != "benfica" {
		t.Fatalf("the CLI's client did not pick up the renewed certificate: %v", err)
	}
	// a body is replayed too
	if err := cli.PutInventory(ctx, api.Inventory{}); err != nil {
		t.Fatalf("request with a body after reload: %v", err)
	}
}

func TestRenewRefusalsOverHTTP(t *testing.T) {
	f := clockFixture(t, nil)
	ctx := context.Background()
	dir := f.enrolDir("benfica", api.ScopeBackup)
	c := f.clientFromDir(dir)
	r := &remote.Renewer{Dir: dir, Client: c, Now: f.clk.Now}
	// rate limited: a second forced renewal right away
	if _, err := r.Check(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Check(ctx, true); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("second renewal: %v", err)
	}
	// a disabled host cannot renew; the failure is recorded with a backoff
	f.clk.Add(time.Hour)
	if err := f.svc.SetHostDisabled(ctx, "custody", "benfica", api.ScopeBackup, true); err != nil {
		t.Fatal(err)
	}
	f.clk.Add(700 * day)
	if _, err := r.Check(ctx, false); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("disabled host: %v", err)
	}
	st := remote.LoadRenewState(dir)
	if st.Failures != 2 || !st.NextAttempt.Equal(f.clk.Now().Add(2*time.Hour)) || st.LastError == "" {
		t.Fatalf("backoff state: %+v", st)
	}
	if out, err := r.Check(ctx, false); err != nil || out.Renewed || !strings.Contains(out.Skipped, "backing off") {
		t.Fatalf("attempt during the backoff: %+v %v", out, err)
	}
	// expired more than 30 days ago: refused with an explanation, even when enabled
	if err := f.svc.SetHostDisabled(ctx, "custody", "benfica", api.ScopeBackup, false); err != nil {
		t.Fatal(err)
	}
	f.clk.Add(70 * day)
	_, err := r.Check(ctx, true)
	if !remote.IsStatus(err, http.StatusUnauthorized) || !strings.Contains(err.Error(), "enrol again") {
		t.Fatalf("expired too long: %v", err)
	}
}

// The new certificate is refused when it is tried: the client rolls back to the old
// pair (still valid), and the next attempt succeeds (the server lets the old
// certificate renew again because the new one was never used).
func TestRenewerRollsBackARefusedCertificate(t *testing.T) {
	var mu sync.Mutex
	refuse := ""
	f := clockFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			fp := refuse
			mu.Unlock()
			if r.URL.Path == "/v1/cert" && len(r.TLS.PeerCertificates) > 0 && fp != "" && pki.Fingerprint(r.TLS.PeerCertificates[0]) != fp {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	ctx := context.Background()
	dir := f.enrolDir("benfica", api.ScopeBackup)
	_, oldCert, _, _ := remote.LoadCredentials(dir)
	oc, _ := pki.ParseCertPEM(oldCert)
	mu.Lock()
	refuse = pki.Fingerprint(oc) // every certificate but the old one is refused on /v1/cert
	mu.Unlock()
	c := f.clientFromDir(dir)
	r := &remote.Renewer{Dir: dir, Client: c, Now: f.clk.Now}
	f.clk.Add(700 * day)
	if _, err := r.Check(ctx, false); err == nil || !strings.Contains(err.Error(), "refused the renewed certificate") {
		t.Fatalf("expected a rollback: %v", err)
	}
	if _, cur, _, _ := remote.LoadCredentials(dir); string(cur) != string(oldCert) {
		t.Fatal("old certificate not restored on disk")
	}
	if _, err := c.Whoami(ctx); err != nil {
		t.Fatalf("rolled-back client: %v", err)
	}
	mu.Lock()
	refuse = ""
	mu.Unlock()
	f.clk.Add(2 * time.Hour) // after the backoff
	out, err := r.Check(ctx, false)
	if err != nil || !out.Renewed || !out.Confirmed {
		t.Fatalf("retry: %+v %v", out, err)
	}
}

// The server certificate is replaced by ANOTHER process (a CLI `ca sans` next to the
// daemon): the listener serves the new certificate to new connections within a
// second, without a restart, and a connection opened before the swap keeps working.
func TestServerCertificateHotReloadAcrossProcesses(t *testing.T) {
	f := clockFixture(t, nil)
	ctx := context.Background()
	dir := f.enrolDir("benfica", api.ScopeBackup)
	ca, crt, key, _ := remote.LoadCredentials(dir)
	cfg, err := pki.ClientTLS(ca, crt, key)
	if err != nil {
		t.Fatal(err)
	}
	type seen struct {
		fp     string
		dns    []string
		reused bool
	}
	get := func(c *http.Client) seen {
		t.Helper()
		var s seen
		req, _ := http.NewRequest("GET", "https://"+f.addr()+"/v1/whoami", nil)
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { s.reused = i.Reused }}))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		s.fp, s.dns = pki.Fingerprint(resp.TLS.PeerCertificates[0]), resp.TLS.PeerCertificates[0].DNSNames
		return s
	}
	before := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg.Clone()}}
	s0 := get(before)
	cli := f.newService() // the CLI process: its own service on the same database and state directory
	if err := cli.SetServerSANs(ctx, []string{"127.0.0.1", "backup.example"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if s := get(before); !s.reused || s.fp != s0.fp {
		t.Fatalf("connection from before the swap: %+v", s)
	}
	after := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg.Clone()}}
	s1 := get(after)
	if s1.reused || s1.fp == s0.fp || len(s1.dns) != 1 || s1.dns[0] != "backup.example" {
		t.Fatalf("new connection after the swap: %+v (before %s)", s1, s0.fp)
	}
	// and the daemon's own view agrees
	st, err := f.svc.CertificateStatus(ctx)
	if err != nil || st.Server.Fingerprint != s1.fp {
		t.Fatalf("status: %v %+v", err, st.Server)
	}
}

// A CA rotation end to end over TLS: a client enrolled before it keeps working, learns
// the new CA by renewing (early, because the signing CA changed), and still trusts the
// server after promotion. A client that never renewed stops trusting it.
func TestCARotationOverTLS(t *testing.T) {
	f := clockFixture(t, nil)
	ctx := context.Background()
	dir := f.enrolDir("benfica", api.ScopeBackup)
	stale := f.enrolDir("stale", api.ScopeBackup)
	c := f.clientFromDir(dir)
	staleClient := f.clientFromDir(stale)
	if _, err := f.svc.RotateCA(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // the listener re-reads the PKI files at most once a second
	r := &remote.Renewer{Dir: dir, Client: c, Now: f.clk.Now}
	out, err := r.Check(ctx, false) // 730 days left, but the signing CA changed
	if err != nil || !out.Renewed || !strings.Contains(out.Reason, "rotation") {
		t.Fatalf("early renewal on rotation: %+v %v", out, err)
	}
	if b, _ := pki.ParseCertsPEM(readCert(t, filepath.Join(dir, "ca.crt"))); len(b) != 2 {
		t.Fatal("the client did not store the trust bundle")
	}
	if out, err := r.Check(ctx, false); err != nil || out.Renewed {
		t.Fatalf("renewed again: %+v %v", out, err)
	}
	if err := f.svc.PromoteCA(ctx, false); err == nil {
		t.Fatal("promoted while one identity is still on the old CA")
	}
	if err := f.svc.PromoteCA(ctx, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	fresh := f.clientFromDir(dir) // a new connection sees the new server certificate
	if who, err := fresh.Whoami(ctx); err != nil || who.Host != "benfica" {
		t.Fatalf("renewed client after promotion: %v", err)
	}
	staleFresh := f.clientFromDir(stale)
	if _, err := staleFresh.Whoami(ctx); err == nil {
		t.Fatal("a client that never learned the new CA still trusts the server (or its old-CA certificate was accepted)")
	}
	_ = staleClient
}

// The production listener (Server.Serve: http.Server.ListenAndServeTLS, which clones the
// configuration and adds h2) negotiates HTTP/2 and hot-reloads its certificate.
func TestProductionListenerHTTP2AndHotReload(t *testing.T) {
	f := clockFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	done := make(chan error, 1)
	go func() { done <- (&httpapi.Server{Svc: f.svc, Log: quietLog()}).Serve(ctx, addr) }()
	dir := f.enrolDir("benfica", api.ScopeBackup) // enrols through the test listener; same service
	ca, crt, key, _ := remote.LoadCredentials(dir)
	cfg, _ := pki.ClientTLS(ca, crt, key)
	get := func() (string, string) {
		t.Helper()
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg.Clone(), ForceAttemptHTTP2: true}}
		defer c.CloseIdleConnections()
		var resp *http.Response
		var err error
		for i := 0; i < 50; i++ {
			if resp, err = c.Get("https://" + addr + "/v1/whoami"); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		return resp.Proto, pki.Fingerprint(resp.TLS.PeerCertificates[0])
	}
	proto, fp0 := get()
	if proto != "HTTP/2.0" {
		t.Fatalf("protocol %s", proto)
	}
	if err := f.newService().SetServerSANs(context.Background(), []string{"127.0.0.1", "other.example"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	proto, fp1 := get()
	if proto != "HTTP/2.0" || fp1 == fp0 {
		t.Fatalf("after the swap: %s, same certificate %v", proto, fp1 == fp0)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
