package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/webtls"
)

// webTLS is the web listener's certificate manager once serveWeb set it up; GET /certificates reports
// its certificate through webCertInfo (wired as mgmt.Server.WebCert before any listener starts, so the
// admin socket can already serve while the web listener is still being set up).
var webTLS atomic.Pointer[webtls.Manager]

// webCertInfo describes the web listener certificate, or nil while there is no web listener.
func webCertInfo() any {
	if m := webTLS.Load(); m != nil {
		return m.Info()
	}
	return nil
}

// serveWeb starts the HTTPS listener of the management UI and API. A failure to bind is fatal: an
// administrator who configured a web address expects it, and silently running without it hides the
// misconfiguration.
func (a *app) serveWeb(ctx context.Context, ms *mgmt.Server, addr string) error {
	tm, err := webtls.New(a.cfg.Web, filepath.Join(a.cfg.StateDir, "web"), a.log)
	if err != nil {
		return fmt.Errorf("web listener: %w", err)
	}
	tc, err := tm.TLSConfig()
	if err != nil {
		return fmt.Errorf("web listener: %w", err)
	}
	webTLS.Store(tm)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("web listener: %w", err)
	}
	srv := &http.Server{Handler: ms.WebHandler(), TLSConfig: tc, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("web listener stopped", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
		ms.FlushAudit() // refusal/read summaries still pending when the server stops
	}()
	go tm.Watch(ctx)
	if h := tm.ChallengeHandler(); h != nil {
		go func() {
			hs := &http.Server{Addr: tm.HTTPChallengeAddr(), Handler: h, ReadHeaderTimeout: 10 * time.Second}
			go func() { <-ctx.Done(); _ = hs.Close() }()
			if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.log.Error("ACME http listener stopped", "err", err)
			}
		}()
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if err := tm.Reload(); err != nil {
					a.log.Error("web certificate reload failed", "err", err)
				}
			}
		}
	}()
	a.log.Info("web UI listening", "addr", addr, "tls", tm.Mode())
	if need, _ := ms.Auth.SetupNeeded(ctx); need {
		if tok, err := ms.Auth.SetupToken(ctx); err == nil && tok != "" {
			a.log.Warn("no user exists yet: open the web UI and create the first administrator with this one-time setup token (or run `incus-backup-server user create`)", "setup_token", tok)
		}
	}
	return nil
}

// housekeepingLoop runs the small periodic jobs of the management plane.
func (a *app) housekeepingLoop(ctx context.Context, ms *mgmt.Server) {
	modes := time.NewTicker(5 * time.Second)
	purge := time.NewTicker(10 * time.Minute)
	defer modes.Stop()
	defer purge.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-modes.C:
			a.svc.RefreshModes(ctx) // a mode set from another process reaches this daemon within seconds
		case <-purge.C:
			_ = ms.Auth.PurgeSessions(ctx)
		}
	}
}
