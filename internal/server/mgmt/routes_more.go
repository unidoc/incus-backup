package mgmt

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// moreRoutes: settings, onboarding and the raw (encrypted) snapshot material the browser decrypts itself.
func (s *Server) moreRoutes(h routeFn) {
	h("GET /api/v1/settings", func(w http.ResponseWriter, r *http.Request) {
		kv, err := s.Svc.Settings(r.Context())
		s.reply(w, r, kv, err)
	})
	h("PUT /api/v1/settings", func(w http.ResponseWriter, r *http.Request) {
		var kv map[string]string
		if err := decode(r, &kv); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, s.Svc.SetSettings(r.Context(), kv))
	})

	// Onboarding: one call yields the one-time token AND the exact command to run on the new host.
	h("POST /api/v1/onboarding", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var req struct {
			Host     string  `json:"host"`
			Scope    string  `json:"scope"`
			TTLHours float64 `json:"ttl_hours"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if req.Scope == "" {
			req.Scope = "backup"
		}
		ttl := time.Duration(req.TTLHours * float64(time.Hour))
		if ttl <= 0 {
			ttl = 24 * time.Hour
		}
		tok, err := s.Svc.CreateToken(r.Context(), repo, req.Host, req.Scope, ttl)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		addr := s.advertise(r)
		cmd := fmt.Sprintf("incus-backup init --server %s --token %s --name %s", addr, tok, req.Host)
		writeJSON(w, http.StatusCreated, map[string]any{
			"token": tok, "server": addr, "command": cmd, "expires_in_hours": ttl.Hours(),
			"install": "apk add incus-backup   # Alpine; or copy the static binary",
			"note":    "the token is shown only now and works once",
		})
	}))

	// Raw repository material for browser-side decryption (the server only ever sees ciphertext).
	h("GET /api/v1/snapshots/{group}/{snap}/index", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		rp, err := s.Svc.Repository(r.Context(), repo)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		b, err := s.Svc.RepoIndex(r.Context(), rp, r.PathValue("group"), r.PathValue("snap"))
		s.blob(w, r, b, err)
	}))
	h("GET /api/v1/snapshots/{group}/{snap}/manifest", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		rp, err := s.Svc.Repository(r.Context(), repo)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		b, err := s.Svc.RepoManifest(r.Context(), rp, r.PathValue("group"), r.PathValue("snap"))
		s.blob(w, r, b, err)
	}))
	h("GET /api/v1/chunks/{id}", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		rp, err := s.Svc.Repository(r.Context(), repo)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		b, err := s.Svc.RepoChunk(r.Context(), rp, r.PathValue("id"))
		s.blob(w, r, b, err)
	}))
}

func (s *Server) blob(w http.ResponseWriter, r *http.Request, b []byte, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, max-age=3600, immutable") // content-addressed: never changes
	_, _ = w.Write(b)
}

// advertise is the address a new host should use to reach the agent API: the configured setting, else
// the name the administrator used to reach the UI with the agent API's port.
func (s *Server) advertise(r *http.Request) string {
	if a := s.Svc.Setting(r.Context(), "advertise_address"); a != "" {
		return a
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	port := "8007"
	if i := strings.LastIndex(s.AgentListen, ":"); i >= 0 && s.AgentListen[i+1:] != "" {
		port = s.AgentListen[i+1:]
	}
	if host == "" || host == "@" {
		host = "SERVER"
	}
	return net.JoinHostPort(host, port)
}
