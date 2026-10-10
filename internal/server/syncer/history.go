package syncer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/pki"
	"github.com/unidoc/incus-backup/internal/schedule"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// RunRecord is one row of a job's run history (sync_runs). A run that is still going has status
// "running" and no FinishedAt.
type RunRecord struct {
	ID                string     `json:"id"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
	Status            string     `json:"status"`
	SnapshotsPending  int        `json:"snapshots_pending"`
	SnapshotsSynced   int        `json:"snapshots_synced"`
	BytesTransferred  int64      `json:"bytes_transferred"`
	ChunksTransferred int64      `json:"chunks_transferred"`
	ChunksReused      int64      `json:"chunks_reused"`
	SnapshotsSkipped  int        `json:"snapshots_skipped"`
	SnapshotsRepaired int        `json:"snapshots_repaired"`
	Error             string     `json:"error"`
}

// Runs returns the newest runs of a job first (limit 1..500, default 50).
func (m *Manager) Runs(ctx context.Context, repoName, name string, limit int) ([]RunRecord, error) {
	j, _, err := m.job(ctx, repoName, name)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := m.Svc.DB.Pool().Query(ctx, `SELECT id::text, started_at, finished_at, status, snapshots_pending, snapshots_synced, bytes_transferred,
		chunks_transferred, chunks_reused, snapshots_skipped, snapshots_repaired, error FROM sync_runs WHERE job_id=$1 ORDER BY started_at DESC LIMIT $2`, j.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunRecord{}
	for rows.Next() {
		var r RunRecord
		if err := rows.Scan(&r.ID, &r.StartedAt, &r.FinishedAt, &r.Status, &r.SnapshotsPending, &r.SnapshotsSynced, &r.BytesTransferred,
			&r.ChunksTransferred, &r.ChunksReused, &r.SnapshotsSkipped, &r.SnapshotsRepaired, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ScheduleUpdate changes when a job runs; a nil field is left unchanged.
type ScheduleUpdate struct {
	Schedule *string `json:"schedule"`
	Enabled  *bool   `json:"enabled"`
}

// SetSchedule changes a job's schedule and/or pauses or resumes it.
func (m *Manager) SetSchedule(ctx context.Context, repoName, name string, u ScheduleUpdate) error {
	j, repo, err := m.job(ctx, repoName, name)
	if err != nil {
		return err
	}
	if u.Schedule != nil {
		if _, err := schedule.Parse(*u.Schedule); err != nil {
			return fmt.Errorf("%w: schedule: %v", service.ErrBadRequest, err)
		}
		j.Schedule = strings.TrimSpace(*u.Schedule)
	}
	if u.Enabled != nil {
		j.Enabled = *u.Enabled
	}
	_, err = m.Svc.DB.Pool().Exec(ctx, `UPDATE sync_jobs SET schedule=$3, enabled=$4 WHERE repository_id=$1 AND name=$2`, repo.ID, name, j.Schedule, j.Enabled)
	return err
}

// RemoteCheck is one step of CheckRemote.
type RemoteCheck struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Level  string `json:"level"` // pass | warn | fail | skip
	Detail string `json:"detail,omitempty"`
}

// RemoteCheckResult is what CheckRemote found.
type RemoteCheckResult struct {
	Checks []RemoteCheck `json:"checks"`
	OK     bool          `json:"ok"`
	// Address is the normalised host:port.
	Address string `json:"address"`
	// CAFingerprint is the SHA-256 fingerprint the token pins (empty without a token).
	CAFingerprint string `json:"ca_fingerprint,omitempty"`
}

// CheckRemote tests a sync peer before a job is added, WITHOUT redeeming the one-time token:
// address, DNS, TCP, a TLS 1.3 handshake, that the peer's certificate chains to the CA the token
// pins (so it is the server that issued the token), and that the agent API answers. Whether the
// token is still valid and has the right scope is only known when the job is added.
func CheckRemote(ctx context.Context, addr, token string) *RemoteCheckResult {
	res := &RemoteCheckResult{}
	add := func(id, label, level, detail string) {
		res.Checks = append(res.Checks, RemoteCheck{id, label, level, detail})
	}
	skipRest := func(ids ...[2]string) {
		for _, x := range ids {
			add(x[0], x[1], "skip", "Not run: an earlier step failed.")
		}
	}
	stDNS, stTCP, stTLS, stPin, stAPI := [2]string{"dns", "Resolve the name"}, [2]string{"tcp", "Open a connection"},
		[2]string{"tls", "TLS 1.3 handshake"}, [2]string{"identity", "Server identity matches the token"}, [2]string{"api", "Agent API answers"}

	hostport, err := normaliseAddr(addr)
	if err != nil {
		add("address", "Address", "fail", err.Error())
		skipRest(stDNS, stTCP, stTLS, stPin, stAPI)
		return res
	}
	res.Address = hostport
	add("address", "Address", "pass", hostport)
	fp := ""
	if strings.TrimSpace(token) != "" {
		parts := strings.Split(strings.TrimSpace(token), ".")
		if len(parts) != 3 || parts[0] != "ibt1" || len(parts[1]) != 64 {
			add("token", "Token format", "fail", "This is not an enrolment token (it looks like ibt1.<64 hex>.<secret>). Copy the whole token printed by `incus-backup-server token create` on the remote.")
		} else {
			fp = parts[1]
			res.CAFingerprint = fp
			add("token", "Token format", "pass", "The token pins the remote's CA "+fp[:16]+"…; it is not used up by this test.")
		}
	}
	host, _, _ := net.SplitHostPort(hostport)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if net.ParseIP(host) == nil {
		ips, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil || len(ips) == 0 {
			add(stDNS[0], stDNS[1], "fail", fmt.Sprintf("%s does not resolve (%v). Check the name, or use the IP address.", host, err))
			skipRest(stTCP, stTLS, stPin, stAPI)
			return res
		}
		add(stDNS[0], stDNS[1], "pass", strings.Join(ips, ", "))
	} else {
		add(stDNS[0], stDNS[1], "pass", "An IP address: nothing to resolve.")
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", hostport)
	if err != nil {
		add(stTCP[0], stTCP[1], "fail", fmt.Sprintf("%v. Is the remote server running, does it listen on this port (the agent API, default 8007, not the web console), and does a firewall allow this server?", err))
		skipRest(stTLS, stPin, stAPI)
		return res
	}
	conn.Close()
	add(stTCP[0], stTCP[1], "pass", "")

	// handshake without verification first, to tell "not TLS / not TLS 1.3" apart from "wrong server"
	raw, err := (&tls.Dialer{NetDialer: d, Config: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}}).DialContext(ctx, "tcp", hostport) //nolint:gosec // identity is checked below against the pinned CA
	if err != nil {
		add(stTLS[0], stTLS[1], "fail", fmt.Sprintf("%v. The address answers but not with TLS 1.3: it may be another service (the web console listens on another port).", err))
		skipRest(stPin, stAPI)
		return res
	}
	raw.Close()
	add(stTLS[0], stTLS[1], "pass", "")

	cfg := &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true} //nolint:gosec // no token: reachability only
	if fp != "" {
		cfg = pki.PinnedTLS(fp)
		pc, err := (&tls.Dialer{NetDialer: d, Config: cfg}).DialContext(ctx, "tcp", hostport)
		if err != nil {
			add(stPin[0], stPin[1], "fail", fmt.Sprintf("The server at %s is not the one that issued this token (%v). Check the address, or create a new token on the right server.", hostport, err))
			skipRest(stAPI)
			return res
		}
		pc.Close()
		add(stPin[0], stPin[1], "pass", "The remote's certificate is signed by the CA the token names.")
	} else {
		add(stPin[0], stPin[1], "skip", "Paste the token to check that this is the server that issued it.")
	}
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+hostport+"/v1/whoami", nil)
	resp, err := hc.Do(req)
	if err != nil {
		add(stAPI[0], stAPI[1], "fail", err.Error())
		return res
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		add(stAPI[0], stAPI[1], "pass", "An incus-backup agent API (it asks for a client certificate, as it should).")
	case http.StatusOK:
		add(stAPI[0], stAPI[1], "warn", "The address answered without asking for a client certificate: it does not look like an incus-backup agent API.")
	default:
		add(stAPI[0], stAPI[1], "warn", fmt.Sprintf("Unexpected answer %d: it may not be an incus-backup agent API.", resp.StatusCode))
	}
	res.OK = true
	for _, c := range res.Checks {
		if c.Level == "fail" {
			res.OK = false
		}
	}
	return res
}

func normaliseAddr(addr string) (string, error) {
	a := strings.TrimSpace(addr)
	if a == "" {
		return "", errors.New("Enter the remote server's agent address, host:port (default port 8007).")
	}
	if !strings.Contains(a, "://") {
		a = "https://" + a
	}
	u, err := url.Parse(a)
	if err != nil || u.Host == "" || u.Scheme != "https" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%q is not host:port.", addr)
	}
	if u.Port() == "" {
		return "", fmt.Errorf("%q has no port. The agent API listens on 8007 by default: %s:8007.", addr, u.Hostname())
	}
	return u.Host, nil
}
