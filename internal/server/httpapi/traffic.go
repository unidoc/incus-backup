package httpapi

import (
	"io"
	"net/http"

	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/traffic"
)

// Traffic control (docs/traffic.md): chunk and index bodies pass through the database's limiter, which
// also counts every host's live throughput for GET /api/v1/traffic/current.

func (s *Server) limiter() *traffic.Limiter {
	if s.Svc == nil || s.Svc.DB == nil {
		return nil
	}
	return traffic.For(s.Svc.DB.Pool())
}

// limitUpload makes the request body of host h obey the upload rules.
func (s *Server) limitUpload(r *http.Request, h *service.Host) {
	if l := s.limiter(); l != nil {
		r.Body = struct {
			io.Reader
			io.Closer
		}{l.Reader(r.Context(), h.RepoID, h.Name, r.Body), r.Body}
	}
}

// writeLimitedBlob sends b to host h under the download rules.
func (s *Server) writeLimitedBlob(w http.ResponseWriter, r *http.Request, h *service.Host, b []byte) {
	l := s.limiter()
	if l == nil {
		writeBlob(w, b)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = l.Writer(r.Context(), h.RepoID, h.Name, w).Write(b)
}
