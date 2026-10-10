// Command testidp is the OpenID provider of the browser end-to-end tests (web/e2e): a real OIDC
// provider (github.com/oauth2-proxy/mockoidc: discovery, JWKS, authorize, token with PKCE and client
// secret checks, RS256 ID tokens) on 127.0.0.1 that approves every authorization at once. The test
// says who signs in next with POST /__next {"sub","email","email_verified","groups"}; without one the
// last person signs in again. It prints one JSON line with the issuer and the client credentials.
//
// Test tooling only: it is not part of either product binary.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/oauth2-proxy/mockoidc"
)

type next struct {
	Sub           string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Groups        []string `json:"groups"`
}

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "address")
	flag.Parse()
	m, err := mockoidc.NewServer(nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var mu sync.Mutex
	who := next{Sub: "sub-e2e-admin", Email: "ada@example.com", EmailVerified: true, Groups: []string{"backup-admins"}}
	_ = m.AddMiddleware(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case mockoidc.AuthorizationEndpoint:
				_ = r.ParseForm()
				if r.Form.Get("code_challenge_method") != "S256" || r.Form.Get("nonce") == "" {
					http.Error(w, "this provider requires PKCE (S256) and a nonce", 400)
					return
				}
				mu.Lock()
				u := who
				mu.Unlock()
				name, _, _ := strings.Cut(u.Email, "@")
				m.QueueUser(&mockoidc.MockUser{Subject: u.Sub, Email: u.Email, EmailVerified: u.EmailVerified, PreferredUsername: name, Groups: u.Groups})
			}
			h.ServeHTTP(w, r)
		})
	})
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := m.Start(ln, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// the control endpoint is on its own listener (mockoidc serves only its own routes)
	cl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go func() {
		_ = http.Serve(cl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var n next
			if r.Method != http.MethodPost || r.URL.Path != "/__next" || json.NewDecoder(r.Body).Decode(&n) != nil || n.Sub == "" {
				http.Error(w, "POST /__next {sub, email, email_verified, groups}", 400)
				return
			}
			mu.Lock()
			who = n
			mu.Unlock()
			w.WriteHeader(204)
		}))
	}()
	b, _ := json.Marshal(map[string]string{"issuer": m.Issuer(), "client_id": m.ClientID, "client_secret": m.ClientSecret, "control": "http://" + cl.Addr().String() + "/__next"})
	fmt.Println(string(b))
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	_ = m.Shutdown()
}
