package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
)

// Destructive operations belong to the process that owns the session pins: the daemon. When one is
// running, the CLI hands the request to it over the admin socket instead of doing it itself (see
// service/daemonlock.go). Without a daemon the CLI does it directly under the same lock.

func (a *app) adminCall(ctx context.Context, method, path string, q url.Values, in, out any) error {
	sock := a.cfg.AdminSocketPath()
	if sock == "" {
		return fmt.Errorf("%w, and its admin socket is disabled (admin_socket: off): stop the daemon or enable the socket", service.ErrDaemonRunning)
	}
	cl := &http.Client{Timeout: 6 * time.Hour, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	u := "http://admin" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, _ := http.NewRequestWithContext(ctx, method, u, body)
	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("the daemon is running but its admin socket %s is not reachable (are you in the incus-backup group?): %w", sock, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = string(b)
		}
		return errors.New(e.Error)
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func repoQ(repo string) url.Values { return url.Values{"repository": {repo}} }

// waitQ asks a task route to answer when the task is done (the CLI prints the result).
func waitQ(repo string) url.Values { return url.Values{"repository": {repo}, "wait": {"1"}} }

func (a *app) doGC(ctx context.Context, repo string, grace time.Duration, dry bool) (*datastore.GCStats, error) {
	st, err := a.svc.GC(ctx, repo, grace, dry)
	if !errors.Is(err, service.ErrDaemonRunning) {
		return st, err
	}
	q := waitQ(repo)
	q.Set("grace_hours", fmt.Sprint(grace.Hours()))
	if dry {
		q.Set("dry_run", "1")
	}
	st = &datastore.GCStats{}
	return st, a.adminCall(ctx, "POST", "/api/v1/gc", q, nil, st)
}

func (a *app) doPrune(ctx context.Context, repo string, dry bool) (*service.PruneResult, error) {
	r, err := a.svc.Prune(ctx, repo, dry)
	if !errors.Is(err, service.ErrDaemonRunning) {
		return r, err
	}
	q := waitQ(repo)
	if dry {
		q.Set("dry_run", "1")
	}
	r = &service.PruneResult{}
	return r, a.adminCall(ctx, "POST", "/api/v1/prune", q, nil, r)
}

func (a *app) doVerify(ctx context.Context, repo string, req service.VerifyRequest) (*datastore.VerifyStats, error) {
	vs, err := a.svc.VerifyJob(ctx, repo, req)
	if !errors.Is(err, service.ErrDaemonRunning) {
		return vs, err
	}
	var out struct {
		Ok    bool                   `json:"ok"`
		Error string                 `json:"error"`
		Stats *datastore.VerifyStats `json:"stats"`
	}
	if err := a.adminCall(ctx, "POST", "/api/v1/verify", waitQ(repo), map[string]any{"mode": req.Mode, "stale_days": req.StaleDays, "no_quarantine": req.NoQuarantine}, &out); err != nil {
		return nil, err
	}
	if !out.Ok {
		return out.Stats, errors.New(out.Error)
	}
	return out.Stats, nil
}

func (a *app) doForget(ctx context.Context, repo, group, snap string) error {
	err := a.svc.ForgetSnapshot(ctx, repo, group, snap)
	if !errors.Is(err, service.ErrDaemonRunning) {
		return err
	}
	return a.adminCall(ctx, "DELETE", "/api/v1/snapshots/"+group+"/"+snap, repoQ(repo), nil, nil)
}

func (a *app) doRotate(ctx context.Context, name string, sec service.S3Secret) error {
	err := a.svc.RotateS3Credential(ctx, name, sec)
	if !errors.Is(err, service.ErrDaemonRunning) {
		return err
	}
	return a.adminCall(ctx, "PUT", "/api/v1/credentials/"+name, nil, map[string]any{"access_key": sec.AccessKey, "secret_key": sec.SecretKey}, nil)
}

func (a *app) doSyncRun(ctx context.Context, repo, name string) (*syncer.Result, error) {
	res, err := a.sync.Run(ctx, repo, name)
	if !errors.Is(err, service.ErrDaemonRunning) {
		return res, err
	}
	var out struct {
		Ok     *bool           `json:"ok"`
		Error  string          `json:"error"`
		Result *syncer.Result  `json:"result"`
		raw    json.RawMessage `json:"-"`
	}
	var raw json.RawMessage
	if err := a.adminCall(ctx, "POST", "/api/v1/sync/"+name+"/run", waitQ(repo), nil, &raw); err != nil {
		return nil, err
	}
	if json.Unmarshal(raw, &out) == nil && out.Ok != nil {
		return out.Result, errors.New(out.Error)
	}
	res = &syncer.Result{}
	return res, json.Unmarshal(raw, res)
}
