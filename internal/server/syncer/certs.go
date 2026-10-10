package syncer

import (
	"context"

	"github.com/unidoc/incus-backup/internal/remote"
)

// RenewPeer renews this server's credential at the peer of a job when less than
// 45 days remain (or the peer rotated its CA), with the same flow and backoff as a
// client host (remote.Renewer). Run calls it before every run; a failure is logged
// and the run goes on with the current certificate, which is still valid.
func (m *Manager) RenewPeer(ctx context.Context, repoName, job string, rc *remote.Client) *remote.RenewOutcome {
	r := &remote.Renewer{Dir: m.credDir(repoName, job), Client: rc, Now: m.Svc.Now, Log: m.Svc.Log.With("sync_job", job, "repository", repoName)}
	out, err := r.Check(ctx, false)
	if err != nil {
		m.Svc.Log.Error("renewing the sync peer certificate failed (the current one is used)", "repository", repoName, "job", job, "err", err)
		return out
	}
	if out.Renewed {
		m.Svc.Log.Info("sync peer certificate renewed", "repository", repoName, "job", job, "reason", out.Reason, "not_after", out.NotAfter)
	}
	return out
}

// RenewPeerNow renews a job's peer credential right away (CLI).
func (m *Manager) RenewPeerNow(ctx context.Context, repoName, job string) (*remote.RenewOutcome, error) {
	j, _, err := m.job(ctx, repoName, job)
	if err != nil {
		return nil, err
	}
	rc, err := m.remoteClient(repoName, j)
	if err != nil {
		return nil, err
	}
	r := &remote.Renewer{Dir: m.credDir(repoName, job), Client: rc, Now: m.Svc.Now, Log: m.Svc.Log}
	return r.Check(ctx, true)
}
