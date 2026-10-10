// Package httpapi exposes the service layer over HTTPS with mutual TLS.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/server/service"
)

const (
	maxIndexBytes    = 256 << 20
	maxManifestBytes = 16 << 20
	maxJSONBytes     = 32 << 20
)

// Server serves the API.
type Server struct {
	Svc *service.Service
	Log *slog.Logger

	enrollLimit *limiter
}

type ctxKey struct{}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	if s.enrollLimit == nil {
		s.enrollLimit = newLimiter(20, 10)
	}
	// unauthenticated (token-authenticated), rate limited per address
	mux.HandleFunc("POST /v1/enroll", func(w http.ResponseWriter, r *http.Request) {
		if !s.enrollLimit.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusTooManyRequests, api.Error{Error: "too many enrolment attempts"})
			return
		}
		s.enroll(w, r)
	})

	h := func(fn func(http.ResponseWriter, *http.Request, *service.Host)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			host, err := s.authenticate(r)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			fn(w, r, host)
		}
	}
	mux.HandleFunc("GET /v1/whoami", h(s.whoami))
	mux.HandleFunc("GET /v1/desired-state", h(s.desiredState))
	mux.HandleFunc("PUT /v1/inventory", h(s.inventory))
	mux.HandleFunc("GET /v1/objects", h(s.objects))
	mux.HandleFunc("GET /v1/groups/{group}/snapshots", h(s.snapshots))
	mux.HandleFunc("GET /v1/groups/{group}/snapshots/{snap}/index", h(s.getIndex))
	mux.HandleFunc("GET /v1/groups/{group}/snapshots/{snap}/manifest", h(s.getManifest))
	mux.HandleFunc("GET /v1/chunks/{id}", h(s.getChunk))
	mux.HandleFunc("POST /v1/sessions", h(s.beginSession))
	mux.HandleFunc("POST /v1/sessions/{sid}/missing", h(s.missing))
	mux.HandleFunc("PUT /v1/sessions/{sid}/chunks/{id}", h(s.putChunk))
	mux.HandleFunc("PUT /v1/sessions/{sid}/index", h(s.putIndex))
	mux.HandleFunc("PUT /v1/sessions/{sid}/manifest", h(s.putManifest))
	mux.HandleFunc("POST /v1/sessions/{sid}/commit", h(s.commit))
	mux.HandleFunc("POST /v1/sessions/{sid}/heartbeat", h(s.heartbeat))
	mux.HandleFunc("DELETE /v1/sessions/{sid}", h(s.abort))
	mux.HandleFunc("PUT /v1/escrow", h(s.putEscrow))
	mux.HandleFunc("GET /v1/escrow", h(s.getEscrow))
	mux.HandleFunc("POST /v1/runs", h(s.startRun))
	mux.HandleFunc("POST /v1/runs/{id}/finish", h(s.finishRun))
	s.syncRoutes(mux, h)
	s.renewRoutes(mux, h)
	s.jobRoutes(mux, h)
	return mux
}

// authenticate maps the TLS client certificate to a host. The service verifies it
// (signature by a trusted CA, validity by its clock, revocation, disabled host).
func (s *Server) authenticate(r *http.Request) (*service.Host, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return nil, service.ErrUnauthorized
	}
	addr, _, _ := net.SplitHostPort(r.RemoteAddr)
	return s.Svc.HostFromCert(r.Context(), r.TLS.PeerCertificates[0], addr)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := http.StatusInternalServerError, "internal error"
	switch {
	case errors.Is(err, service.ErrUnauthorized):
		code, msg = http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, service.ErrForbidden):
		code, msg = http.StatusForbidden, err.Error()
	case errors.Is(err, service.ErrNotFound), errors.Is(err, datastore.ErrNotFound):
		code, msg = http.StatusNotFound, err.Error()
	case errors.Is(err, service.ErrConflict):
		code, msg = http.StatusConflict, err.Error()
	case errors.Is(err, service.ErrBadRequest):
		code, msg = http.StatusBadRequest, err.Error()
	case errors.Is(err, datastore.ErrReadOnly), errors.Is(err, datastore.ErrOffline):
		code, msg = http.StatusServiceUnavailable, err.Error()
	default:
		s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	writeJSON(w, code, api.Error{Error: msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error { return readJSONMax(r, v, maxJSONBytes) }

func readJSONMax(r *http.Request, v any, max int64) error {
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, max))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return err
	}
	return nil
}

func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	body := http.MaxBytesReader(w, r.Body, max)
	if n := r.ContentLength; n > 0 && n < max {
		// Fewer grow-and-copy steps than io.ReadAll for a 4 MiB chunk, but the memory follows the
		// bytes that ARRIVED, never the announced length alone: a peer that announces MaxBlobSize on
		// many HTTP/2 streams and sends nothing must not make the server allocate it for each.
		// The server itself enforces that the body is exactly Content-Length long.
		b := make([]byte, 0, min(n, readBodyFirst))
		for int64(len(b)) < n {
			if len(b) == cap(b) {
				nb := make([]byte, len(b), min(n, int64(cap(b))*4))
				copy(nb, b)
				b = nb
			}
			k, err := body.Read(b[len(b):cap(b)])
			b = b[:len(b)+k]
			if err == io.EOF {
				if int64(len(b)) < n {
					return nil, io.ErrUnexpectedEOF
				}
				break
			}
			if err != nil {
				return nil, err
			}
		}
		return b, nil
	}
	return io.ReadAll(body)
}

// readBodyFirst is the first buffer of readBody; it grows 4x per step up to the announced length
// (64 KiB, 256 KiB, 1 MiB, 4 MiB, then the length: four copies for a 4 MiB chunk).
const readBodyFirst = 64 << 10

func (s *Server) badJSON(w http.ResponseWriter, r *http.Request, err error) {
	s.fail(w, r, errors.Join(service.ErrBadRequest, err))
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10) // unauthenticated: a token and a CSR are small
	var req api.EnrollRequest
	if err := readJSON(r, &req); err != nil {
		s.badJSON(w, r, err)
		return
	}
	resp, err := s.Svc.Enroll(r.Context(), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request, h *service.Host) {
	repo, err := s.Svc.RepositoryByID(r.Context(), h.RepoID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, api.Whoami{Host: h.Name, Scope: h.Scope, Repository: repo.Name, DomainID: repo.DomainID, ServerTime: time.Now().UTC(), Version: s.Svc.Version, RepositoryMode: repo.Mode})
}

func (s *Server) desiredState(w http.ResponseWriter, r *http.Request, h *service.Host) {
	ds, err := s.Svc.DesiredState(r.Context(), h)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, ds)
}

func (s *Server) inventory(w http.ResponseWriter, r *http.Request, h *service.Host) {
	var inv api.Inventory
	if err := readJSON(r, &inv); err != nil {
		s.badJSON(w, r, err)
		return
	}
	if err := s.Svc.UpsertInventory(r.Context(), h, inv); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request, h *service.Host) {
	l, err := s.Svc.Snapshots(r.Context(), h, r.PathValue("group"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, l)
}

func writeBlob(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	// the length lets the client allocate the buffer once instead of growing it while reading
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
}

func (s *Server) getIndex(w http.ResponseWriter, r *http.Request, h *service.Host) {
	b, err := s.Svc.ReadIndex(r.Context(), h, r.PathValue("group"), r.PathValue("snap"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeLimitedBlob(w, r, h, b)
}

func (s *Server) getManifest(w http.ResponseWriter, r *http.Request, h *service.Host) {
	b, err := s.Svc.ReadManifest(r.Context(), h, r.PathValue("group"), r.PathValue("snap"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeBlob(w, b)
}

func (s *Server) getChunk(w http.ResponseWriter, r *http.Request, h *service.Host) {
	b, err := s.Svc.ReadChunk(r.Context(), h, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeLimitedBlob(w, r, h, b)
}

func (s *Server) beginSession(w http.ResponseWriter, r *http.Request, h *service.Host) {
	var req api.SessionRequest
	if err := readJSON(r, &req); err != nil {
		s.badJSON(w, r, err)
		return
	}
	resp, err := s.Svc.BeginSession(r.Context(), h, r.Header.Get(api.HeaderDomain), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) missing(w http.ResponseWriter, r *http.Request, h *service.Host) {
	var req api.MissingRequest
	if err := readJSON(r, &req); err != nil {
		s.badJSON(w, r, err)
		return
	}
	m, err := s.Svc.SessionMissing(r.Context(), h, r.PathValue("sid"), req.IDs)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if m == nil {
		m = []string{}
	}
	writeJSON(w, 200, api.MissingResponse{Missing: m})
}

func (s *Server) putChunk(w http.ResponseWriter, r *http.Request, h *service.Host) {
	s.limitUpload(r, h)
	b, err := readBody(w, r, datastore.MaxBlobSize+1)
	if err != nil {
		s.badJSON(w, r, err)
		return
	}
	if err := s.Svc.SessionPutChunk(r.Context(), h, r.PathValue("sid"), r.PathValue("id"), b); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putIndex(w http.ResponseWriter, r *http.Request, h *service.Host) {
	s.limitUpload(r, h)
	b, err := readBody(w, r, maxIndexBytes)
	if err != nil {
		s.badJSON(w, r, err)
		return
	}
	if err := s.Svc.SessionPutIndex(r.Context(), h, r.PathValue("sid"), b); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putManifest(w http.ResponseWriter, r *http.Request, h *service.Host) {
	b, err := readBody(w, r, maxManifestBytes)
	if err != nil {
		s.badJSON(w, r, err)
		return
	}
	if err := s.Svc.SessionPutManifest(r.Context(), h, r.PathValue("sid"), b); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) commit(w http.ResponseWriter, r *http.Request, h *service.Host) {
	var req api.CommitRequest
	if err := readJSON(r, &req); err != nil {
		s.badJSON(w, r, err)
		return
	}
	resp, err := s.Svc.SessionCommit(r.Context(), h, r.PathValue("sid"), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) putEscrow(w http.ResponseWriter, r *http.Request, h *service.Host) {
	b, err := readBody(w, r, 8192)
	if err != nil {
		s.badJSON(w, r, err)
		return
	}
	if err := s.Svc.PutEscrow(r.Context(), h, r.Header.Get(api.HeaderDomain), b); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getEscrow(w http.ResponseWriter, r *http.Request, h *service.Host) {
	b, err := s.Svc.GetEscrow(r.Context(), h)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(b)
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, h *service.Host) {
	exp, err := s.Svc.SessionHeartbeat(r.Context(), h, r.PathValue("sid"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, api.SessionResponse{Session: r.PathValue("sid"), Expires: exp})
}

func (s *Server) abort(w http.ResponseWriter, r *http.Request, h *service.Host) {
	if err := s.Svc.SessionAbort(r.Context(), h, r.PathValue("sid")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request, h *service.Host) {
	var req api.RunStart
	if err := readJSON(r, &req); err != nil {
		s.badJSON(w, r, err)
		return
	}
	id, err := s.Svc.StartRun(r.Context(), h, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.RunStartResponse{Run: id})
}

func (s *Server) finishRun(w http.ResponseWriter, r *http.Request, h *service.Host) {
	var req api.RunFinish
	if err := readJSON(r, &req); err != nil {
		s.badJSON(w, r, err)
		return
	}
	if err := s.Svc.FinishRun(r.Context(), h, r.PathValue("id"), req); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Serve listens with mutual TLS until ctx ends.
func (s *Server) Serve(ctx context.Context, addr string) error {
	tlsCfg, err := s.Svc.ServerTLSConfig()
	if err != nil {
		return err
	}
	tlsCfg.NextProtos = []string{"h2", "http/1.1"} // explicit: the per-handshake config (hot reload) copies these at every handshake
	srv := s.NewHTTPServer()
	srv.Addr, srv.TLSConfig = addr, tlsCfg
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	err = srv.ListenAndServeTLS("", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// HTTP/2 receive windows of the API server. A client multiplexes all of its chunk uploads over
// ONE connection, so the connection window bounds the bytes in flight to the server: with Go's
// default (1 MiB) an upload can never be faster than 1 MiB per round trip (50 MiB/s at 20 ms,
// 12 MiB/s at 80 ms) however many workers send. The windows only cost memory when a handler
// stops reading; the handlers read every body at once. They are bounded because a peer without a
// certificate can connect (enrolment): 32 MiB per connection still allows ~400 MB/s at 80 ms.
const (
	h2ConnWindow   = 32 << 20
	h2StreamWindow = 8 << 20
)

// NewHTTPServer returns the API server (Handler, timeouts, HTTP/2 settings) without an address
// or TLS configuration; Serve, the tests and the benchmarks all use it.
func (s *Server) NewHTTPServer() *http.Server {
	return &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
		HTTP2: &http.HTTP2Config{
			MaxReceiveBufferPerConnection: h2ConnWindow,
			MaxReceiveBufferPerStream:     h2StreamWindow,
		},
	}
}

var _ = strings.TrimSpace

func (s *Server) objects(w http.ResponseWriter, r *http.Request, h *service.Host) {
	l, err := s.Svc.Objects(r.Context(), h)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, l)
}
