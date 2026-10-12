package httpapi

import (
	"errors"
	"net"
	"net/http"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// renewRoutes registers certificate renewal (docs/certificates.md):
//
//	POST /v1/renew          authenticated by the certificate being renewed (it may have
//	                        expired up to 30 days ago), body api.RenewRequest
//	POST /v1/renew/confirm  authenticated by the NEW certificate; revokes the others
//	GET  /v1/cert           what the server knows about the presented certificate
func (s *Server) renewRoutes(mux *http.ServeMux, h func(func(http.ResponseWriter, *http.Request, *service.Host)) http.HandlerFunc) {
	lim := newLimiter(10, 5) // per address; the service also limits per identity
	mux.HandleFunc("POST /v1/renew", func(w http.ResponseWriter, r *http.Request) {
		if !lim.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusTooManyRequests, api.Error{Error: "too many renewal attempts"})
			return
		}
		s.renew(w, r)
	})
	mux.HandleFunc("POST /v1/renew/confirm", h(s.renewConfirm))
	mux.HandleFunc("GET /v1/cert", h(s.certInfo))
}

func (s *Server) renew(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		s.fail(w, r, service.ErrUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req api.RenewRequest
	if err := readJSON(r, &req); err != nil {
		s.badJSON(w, r, err)
		return
	}
	addr, _, _ := net.SplitHostPort(r.RemoteAddr)
	resp, err := s.Svc.Renew(r.Context(), r.TLS.PeerCertificates[0], []byte(req.CSR), addr)
	switch {
	case errors.Is(err, service.ErrCertExpired):
		writeJSON(w, http.StatusUnauthorized, api.Error{Error: err.Error()})
	case errors.Is(err, service.ErrTooManyRequests):
		w.Header().Set("Retry-After", "600")
		writeJSON(w, http.StatusTooManyRequests, api.Error{Error: err.Error()})
	case err != nil:
		s.fail(w, r, err)
	default:
		writeJSON(w, http.StatusOK, resp)
	}
}

func (s *Server) renewConfirm(w http.ResponseWriter, r *http.Request, h *service.Host) {
	n, err := s.Svc.ConfirmRenewal(r.Context(), h)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, api.RenewConfirmResponse{Revoked: n})
}

func (s *Server) certInfo(w http.ResponseWriter, r *http.Request, h *service.Host) {
	info, err := s.Svc.CertInfo(r.Context(), h, r.TLS.PeerCertificates[0])
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}
