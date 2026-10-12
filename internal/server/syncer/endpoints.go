package syncer

import (
	"context"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// LocalRepo adapts a repository of this server to Source and Dest.
type LocalRepo struct {
	Svc    *service.Service
	Repo   *service.Repo
	Groups []string // optional listing filter
}

func (l *LocalRepo) Domain(ctx context.Context) (string, error) {
	r, err := l.Svc.RepositoryByID(ctx, l.Repo.ID)
	if err != nil {
		return "", err
	}
	return r.DomainID, nil
}
func (l *LocalRepo) List(ctx context.Context) ([]api.SnapshotRef, error) {
	return l.Svc.ListAllSnapshots(ctx, l.Repo, l.Groups)
}
func (l *LocalRepo) Index(ctx context.Context, g, s string) ([]byte, error) {
	return l.Svc.RepoIndex(ctx, l.Repo, g, s)
}
func (l *LocalRepo) Manifest(ctx context.Context, g, s string) ([]byte, error) {
	return l.Svc.RepoManifest(ctx, l.Repo, g, s)
}
func (l *LocalRepo) Chunk(ctx context.Context, id string) ([]byte, error) {
	return l.Svc.RepoChunk(ctx, l.Repo, id)
}
func (l *LocalRepo) Missing(ctx context.Context, ids []string) ([]string, error) {
	return l.Svc.SyncMissing(ctx, l.Repo, ids)
}
func (l *LocalRepo) PutChunk(ctx context.Context, id string, blob []byte) error {
	return l.Svc.SyncPutChunk(ctx, l.Repo, id, blob)
}
func (l *LocalRepo) PutSnapshot(ctx context.Context, domain string, put api.SyncSnapshotPut) error {
	return l.Svc.SyncPutSnapshot(ctx, l.Repo, domain, put)
}

// RemoteRepo adapts a remote server (reached with this server's sync
// credentials) to Source and Dest.
type RemoteRepo struct{ C *remote.Client }

func (r *RemoteRepo) Domain(ctx context.Context) (string, error) {
	w, err := r.C.Whoami(ctx)
	if err != nil {
		return "", err
	}
	return w.DomainID, nil
}
func (r *RemoteRepo) List(ctx context.Context) ([]api.SnapshotRef, error) { return r.C.SyncList(ctx) }
func (r *RemoteRepo) Index(ctx context.Context, g, s string) ([]byte, error) {
	return r.C.Index(ctx, g, s)
}
func (r *RemoteRepo) Manifest(ctx context.Context, g, s string) ([]byte, error) {
	return r.C.Manifest(ctx, g, s)
}
func (r *RemoteRepo) Chunk(ctx context.Context, id string) ([]byte, error) { return r.C.Chunk(ctx, id) }
func (r *RemoteRepo) Missing(ctx context.Context, ids []string) ([]string, error) {
	return r.C.SyncMissing(ctx, ids)
}
func (r *RemoteRepo) PutChunk(ctx context.Context, id string, blob []byte) error {
	return r.C.SyncPutChunk(ctx, id, blob)
}
func (r *RemoteRepo) PutSnapshot(ctx context.Context, domain string, put api.SyncSnapshotPut) error {
	return r.C.SyncPutSnapshot(ctx, domain, put)
}
