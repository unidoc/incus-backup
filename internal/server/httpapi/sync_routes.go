package httpapi

import (
	"context"
	"net/http"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// maxSyncSnapshotBytes bounds PUT /v1/sync/snapshots: an index and a manifest at their own limits,
// base64-encoded, plus the metadata.
const maxSyncSnapshotBytes = (maxIndexBytes+maxManifestBytes)/3*4 + 1<<20

// syncRoutes registers the server-to-server synchronisation endpoints. Reading
// snapshots, indexes, manifests and chunks reuses the ordinary GET endpoints
// (their scope check admits sync-read); writing needs sync-write.
func (s *Server) syncRoutes(mux *http.ServeMux, h func(func(http.ResponseWriter, *http.Request, *service.Host)) http.HandlerFunc) {
	mux.HandleFunc("GET /v1/sync/snapshots", h(s.syncList))
	mux.HandleFunc("POST /v1/sync/chunks/missing", h(s.syncMissing))
	mux.HandleFunc("PUT /v1/sync/chunks/{id}", h(s.syncPutChunk))
	mux.HandleFunc("PUT /v1/sync/snapshots/{group}/{snap}", h(s.syncPutSnapshot))
}

func (s *Server) syncList(w http.ResponseWriter, r *http.Request, h *service.Host) {
	repo, err := s.Svc.SyncHostRepo(r.Context(), h, api.ScopeSyncRead, api.ScopeSyncWrite)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	l, err := s.Svc.ListAllSnapshots(r.Context(), repo, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if l == nil {
		l = []api.SnapshotRef{}
	}
	writeJSON(w, 200, api.SnapshotList{Snapshots: l})
}

func (s *Server) syncMissing(w http.ResponseWriter, r *http.Request, h *service.Host) {
	repo, err := s.Svc.SyncHostRepo(r.Context(), h, api.ScopeSyncWrite)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req api.MissingRequest
	if err := readJSON(r, &req); err != nil {
		s.badJSON(w, r, err)
		return
	}
	m, err := s.Svc.SyncMissing(syncCtx(r, h), repo, req.IDs)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if m == nil {
		m = []string{}
	}
	writeJSON(w, 200, api.MissingResponse{Missing: m})
}

func (s *Server) syncPutChunk(w http.ResponseWriter, r *http.Request, h *service.Host) {
	repo, err := s.Svc.SyncHostRepo(r.Context(), h, api.ScopeSyncWrite)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	b, err := readBody(w, r, datastore.MaxBlobSize+1)
	if err != nil {
		s.badJSON(w, r, err)
		return
	}
	if err := s.Svc.SyncPutChunk(syncCtx(r, h), repo, r.PathValue("id"), b); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) syncPutSnapshot(w http.ResponseWriter, r *http.Request, h *service.Host) {
	repo, err := s.Svc.SyncHostRepo(r.Context(), h, api.ScopeSyncWrite)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var put api.SyncSnapshotPut
	// the index and manifest travel base64-encoded: the limit follows from theirs, not from the
	// small-JSON limit (a 4 TiB disk has a 40 MB index, 53 MB in base64)
	r.Body = http.MaxBytesReader(w, r.Body, maxSyncSnapshotBytes)
	if err := readJSONMax(r, &put, maxSyncSnapshotBytes); err != nil {
		s.badJSON(w, r, err)
		return
	}
	put.Meta.GroupID, put.Meta.SnapshotID = r.PathValue("group"), r.PathValue("snap")
	if err := s.Svc.SyncPutSnapshot(syncCtx(r, h), repo, r.Header.Get(api.HeaderDomain), put); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// syncCtx names the transfer a sync write belongs to: the calling host plus the id its client sends per
// job run, so concurrent transfers into one repository keep separate chunk pins.
func syncCtx(r *http.Request, h *service.Host) context.Context {
	id := r.Header.Get(api.HeaderSyncTransfer)
	if len(id) > 64 {
		id = id[:64]
	}
	return service.WithSyncTransfer(r.Context(), h.ID+"/"+id)
}
