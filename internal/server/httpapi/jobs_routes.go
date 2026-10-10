package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/jobspec"
	"github.com/unidoc/incus-backup/internal/server/jobs"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// jobRoutes registers the host side of the job protocol (mutual TLS, backup scope; a host only
// ever sees its own jobs). The management side (create, list, cancel, deliveries for the browser) is
// a Go API in package jobs.
func (s *Server) jobRoutes(mux *http.ServeMux, h func(func(http.ResponseWriter, *http.Request, *service.Host)) http.HandlerFunc) {
	mux.HandleFunc("GET /v1/jobs/next", h(s.jobNext))
	mux.HandleFunc("POST /v1/jobs/{id}/progress", h(s.jobProgress))
	mux.HandleFunc("POST /v1/jobs/{id}/finish", h(s.jobFinish))
	mux.HandleFunc("GET /v1/jobs/{id}/cancel-requested", h(s.jobCancelRequested))
	mux.HandleFunc("PUT /v1/jobs/{id}/delivery", h(s.jobDelivery))
	mux.HandleFunc("POST /v1/restore-tests", h(s.restoreTestReport))
}

func (s *Server) jobs() *jobs.Service { return jobs.For(s.Svc) }

func (s *Server) jobNext(w http.ResponseWriter, r *http.Request, h *service.Host) {
	wait := 0
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			s.fail(w, r, jobsBad("wait must be a number of seconds"))
			return
		}
		wait = n
	}
	var types []string
	if v := r.URL.Query().Get("types"); v != "" {
		types = strings.Split(v, ",")
	}
	j, err := s.jobs().Next(r.Context(), h, time.Duration(wait)*time.Second, types)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if j == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func jobsBad(msg string) error { return &badRequest{msg} }

type badRequest struct{ msg string }

func (b *badRequest) Error() string { return "bad request: " + b.msg }
func (b *badRequest) Unwrap() error { return service.ErrBadRequest }

func (s *Server) jobProgress(w http.ResponseWriter, r *http.Request, h *service.Host) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var p jobspec.Progress
	if err := readJSON(r, &p); err != nil {
		s.badJSON(w, r, err)
		return
	}
	resp, err := s.jobs().Progress(r.Context(), h, r.PathValue("id"), p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) jobFinish(w http.ResponseWriter, r *http.Request, h *service.Host) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var f jobspec.Finish
	if err := readJSON(r, &f); err != nil {
		s.badJSON(w, r, err)
		return
	}
	if err := s.jobs().Finish(r.Context(), h, r.PathValue("id"), f); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) jobCancelRequested(w http.ResponseWriter, r *http.Request, h *service.Host) {
	cs, err := s.jobs().CancelRequested(r.Context(), h, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cs)
}

// DeliveryReceipt answers a delivery upload.
type DeliveryReceipt struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func (s *Server) jobDelivery(w http.ResponseWriter, r *http.Request, h *service.Host) {
	attempt, err := strconv.Atoi(r.Header.Get(jobspec.HeaderAttempt))
	if err != nil {
		s.fail(w, r, jobsBad("missing "+jobspec.HeaderAttempt+" header"))
		return
	}
	if r.ContentLength > jobs.MaxDeliveryBytes {
		s.fail(w, r, jobsBad("a delivery is too large"))
		return
	}
	body := http.MaxBytesReader(w, r.Body, jobs.MaxDeliveryBytes+1)
	size, sum, err := s.jobs().PutDelivery(r.Context(), h, r.PathValue("id"), attempt, body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, DeliveryReceipt{Size: size, SHA256: sum})
}

func (s *Server) restoreTestReport(w http.ResponseWriter, r *http.Request, h *service.Host) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var rep jobspec.RestoreTestReport
	if err := readJSON(r, &rep); err != nil {
		s.badJSON(w, r, err)
		return
	}
	id, err := s.jobs().ReportRestoreTest(r.Context(), h, rep)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}
