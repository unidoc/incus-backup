// Package remote is the Go client for the incus-backup-server HTTP API. It is
// used by the incus-backup client and by the server-to-server sync engine.
package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/pki"
)

// APIError is a non-2xx answer from the server.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("server answered %d: %s", e.Status, e.Msg) }

// IsStatus reports whether err is an APIError with the given status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// Client talks to one server.
type Client struct {
	Base   string // https://host:port
	Domain string // key domain id sent when opening sessions
	HTTP   *http.Client
	// Reload, when set, re-reads the credentials from disk: a request refused with 401
	// is retried once with them if they changed (another process renewed the certificate).
	Reload func() (caPEM, certPEM, keyPEM []byte, err error)

	mu      sync.RWMutex
	certPEM []byte

	xferOnce sync.Once
	xfer     string
}

// transferID is the random id this client (one job run) sends with its sync writes, so the receiving
// server keeps a separate chunk pin for every concurrent transfer.
func (c *Client) transferID() string {
	c.xferOnce.Do(func() {
		var b [12]byte
		_, _ = rand.Read(b[:])
		c.xfer = hex.EncodeToString(b[:])
	})
	return c.xfer
}

func (c *Client) httpClient() *http.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.HTTP
}

// SetCredentials switches the client to new credentials. New requests use a new
// transport (so a fresh TLS handshake presents the new certificate); requests in
// flight finish on the old one, whose idle connections are closed.
func (c *Client) SetCredentials(caPEM, certPEM, keyPEM []byte) error {
	cfg, err := pki.ClientTLS(caPEM, certPEM, keyPEM)
	if err != nil {
		return err
	}
	c.mu.Lock()
	old := c.HTTP
	c.HTTP = transport(cfg)
	c.certPEM = append([]byte(nil), certPEM...)
	c.mu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}
	return nil
}

// CertPEM is the client certificate the client currently presents.
func (c *Client) CertPEM() []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.certPEM
}

// reloadCreds re-reads the credentials; true when they changed and were installed.
func (c *Client) reloadCreds() bool {
	if c.Reload == nil {
		return false
	}
	ca, crt, key, err := c.Reload()
	if err != nil || bytes.Equal(crt, c.CertPEM()) {
		return false
	}
	return c.SetCredentials(ca, crt, key) == nil
}

func transport(cfg *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:       cfg,
		MaxIdleConnsPerHost:   8,
		ResponseHeaderTimeout: 5 * time.Minute,
		ForceAttemptHTTP2:     true,
	}}
}

func baseURL(addr string) (string, error) {
	if !strings.Contains(addr, "://") {
		addr = "https://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" || u.Scheme != "https" {
		return "", fmt.Errorf("invalid server address %q (need host:port)", addr)
	}
	return "https://" + u.Host, nil
}

// New builds a client that trusts exactly caPEM and presents certPEM.
func New(addr string, caPEM, certPEM, keyPEM []byte, domain string) (*Client, error) {
	b, err := baseURL(addr)
	if err != nil {
		return nil, err
	}
	c := &Client{Base: b, Domain: domain}
	if err := c.SetCredentials(caPEM, certPEM, keyPEM); err != nil {
		return nil, err
	}
	return c, nil
}

// Enrolled is the result of enrolment.
type Enrolled struct {
	CertPEM, KeyPEM, CAPEM []byte
	Resp                   api.EnrollResponse
}

// Enroll redeems a one-time token. The server is authenticated by the CA
// fingerprint embedded in the token.
func Enroll(ctx context.Context, addr, token, commonName string, info map[string]any) (*Enrolled, error) {
	fp, err := tokenFingerprint(token)
	if err != nil {
		return nil, err
	}
	b, err := baseURL(addr)
	if err != nil {
		return nil, err
	}
	keyPEM, csrPEM, err := pki.NewKeyAndCSR(commonName)
	if err != nil {
		return nil, err
	}
	c := &Client{Base: b, HTTP: transport(pki.PinnedTLS(fp))}
	var resp api.EnrollResponse
	if err := c.do(ctx, "POST", "/v1/enroll", api.EnrollRequest{Token: token, CSR: string(csrPEM), Info: info}, &resp, nil); err != nil {
		return nil, err
	}
	// The returned CA must be the one the token pinned.
	ca, err := pki.ParseCertPEM([]byte(resp.CA))
	if err != nil || pki.Fingerprint(ca) != fp {
		return nil, errors.New("server returned a CA that does not match the token's pinned fingerprint")
	}
	return &Enrolled{CertPEM: []byte(resp.Certificate), KeyPEM: keyPEM, CAPEM: []byte(resp.CA), Resp: resp}, nil
}

func tokenFingerprint(tok string) (string, error) {
	p := strings.Split(tok, ".")
	if len(p) != 3 || p[0] != "ibt1" || len(p[1]) != 64 {
		return "", errors.New("malformed enrolment token")
	}
	return p[1], nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any, hdr map[string]string) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	return c.doRaw(ctx, method, path, body, "application/json", out, hdr, nil)
}

// RequestTimeout bounds one request (a chunk is at most 80 MiB: a peer that stalls for half an hour is
// hung, and must not hold a sync run or a backup session for ever). MaxResponse bounds what is read back.
const (
	RequestTimeout = 30 * time.Minute
	MaxResponse    = 512 << 20
)

func (c *Client) doRaw(ctx context.Context, method, path string, body io.Reader, ctype string, out any, hdr map[string]string, raw *[]byte) error {
	err := c.doOnce(ctx, method, path, body, ctype, out, hdr, raw)
	if !IsStatus(err, http.StatusUnauthorized) {
		return err
	}
	// another process may have renewed the certificate (and confirmed it, revoking ours)
	if br, ok := body.(*bytes.Reader); body != nil && !ok {
		return err
	} else if ok {
		if _, serr := br.Seek(0, io.SeekStart); serr != nil {
			return err
		}
	}
	if !c.reloadCreds() {
		return err
	}
	return c.doOnce(ctx, method, path, body, ctype, out, hdr, raw)
}

func (c *Client) doOnce(ctx context.Context, method, path string, body io.Reader, ctype string, out any, hdr map[string]string, raw *[]byte) error {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if method != "GET" && strings.HasPrefix(path, "/v1/sync/") {
		req.Header.Set(api.HeaderSyncTransfer, c.transferID())
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := readSized(resp.Body, resp.ContentLength, MaxResponse)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e api.Error
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &APIError{Status: resp.StatusCode, Msg: msg}
	}
	if raw != nil {
		*raw = b
		return nil
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// RetryDelay is the base backoff between attempts (tests lower it).
var RetryDelay = 500 * time.Millisecond

// retry runs fn up to 4 times with backoff on transport errors and 5xx.
func retry(ctx context.Context, fn func() error) error {
	var err error
	for i := 0; i < 4; i++ {
		if err = fn(); err == nil {
			return nil
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.Status < 500 {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * RetryDelay):
		}
	}
	return err
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	var b []byte
	err := retry(ctx, func() error { return c.doRaw(ctx, "GET", path, nil, "", nil, nil, &b) })
	return b, err
}

// Whoami returns the identity the server sees.
func (c *Client) Whoami(ctx context.Context) (*api.Whoami, error) {
	var w api.Whoami
	return &w, retry(ctx, func() error { return c.do(ctx, "GET", "/v1/whoami", nil, &w, nil) })
}

// DesiredState fetches policies and assignments.
func (c *Client) DesiredState(ctx context.Context) (*api.DesiredState, error) {
	var d api.DesiredState
	return &d, retry(ctx, func() error { return c.do(ctx, "GET", "/v1/desired-state", nil, &d, nil) })
}

// PutInventory reports the host's storage objects.
func (c *Client) PutInventory(ctx context.Context, inv api.Inventory) error {
	return retry(ctx, func() error { return c.do(ctx, "PUT", "/v1/inventory", inv, nil, nil) })
}

// Snapshots lists committed snapshots of a group (oldest first).
func (c *Client) Snapshots(ctx context.Context, group string) (*api.SnapshotList, error) {
	var l api.SnapshotList
	return &l, retry(ctx, func() error { return c.do(ctx, "GET", "/v1/groups/"+url.PathEscape(group)+"/snapshots", nil, &l, nil) })
}

// Index fetches a snapshot's index file.
func (c *Client) Index(ctx context.Context, group, snap string) ([]byte, error) {
	return c.get(ctx, "/v1/groups/"+url.PathEscape(group)+"/snapshots/"+url.PathEscape(snap)+"/index")
}

// Manifest fetches a snapshot's encrypted manifest.
func (c *Client) Manifest(ctx context.Context, group, snap string) ([]byte, error) {
	return c.get(ctx, "/v1/groups/"+url.PathEscape(group)+"/snapshots/"+url.PathEscape(snap)+"/manifest")
}

// Chunk fetches one chunk blob.
func (c *Client) Chunk(ctx context.Context, id string) ([]byte, error) {
	return c.get(ctx, "/v1/chunks/"+url.PathEscape(id))
}

// Session is an open upload session on the server.
type Session struct {
	c  *Client
	ID string
	// Lease is the server's inactivity lease; the caller heartbeats at least every Lease/3
	// while it is busy without talking to the server.
	Lease time.Duration
}

// Begin opens an upload session.
func (c *Client) Begin(ctx context.Context, req api.SessionRequest) (*Session, error) {
	var r api.SessionResponse
	if err := c.do(ctx, "POST", "/v1/sessions", req, &r, map[string]string{api.HeaderDomain: c.Domain}); err != nil {
		return nil, err
	}
	return &Session{c: c, ID: r.Session, Lease: r.Lease}, nil
}

// Heartbeat renews the session's lease without doing anything else.
func (s *Session) Heartbeat(ctx context.Context) error {
	return s.c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(s.ID)+"/heartbeat", nil, nil, nil)
}

// Missing asks which ids the server lacks.
func (s *Session) Missing(ctx context.Context, ids []string) ([]string, error) {
	var out api.MissingResponse
	err := retry(ctx, func() error {
		return s.c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(s.ID)+"/missing", api.MissingRequest{IDs: ids}, &out, nil)
	})
	return out.Missing, err
}

// PutChunk uploads one blob (idempotent, retried).
func (s *Session) PutChunk(ctx context.Context, id string, blob []byte) error {
	return retry(ctx, func() error {
		return s.c.doRaw(ctx, "PUT", "/v1/sessions/"+url.PathEscape(s.ID)+"/chunks/"+url.PathEscape(id), bytes.NewReader(blob), "application/octet-stream", nil, nil, nil)
	})
}

// PutIndex uploads the index file.
func (s *Session) PutIndex(ctx context.Context, b []byte) error {
	return retry(ctx, func() error {
		return s.c.doRaw(ctx, "PUT", "/v1/sessions/"+url.PathEscape(s.ID)+"/index", bytes.NewReader(b), "application/octet-stream", nil, nil, nil)
	})
}

// PutManifest uploads the encrypted manifest.
func (s *Session) PutManifest(ctx context.Context, b []byte) error {
	return retry(ctx, func() error {
		return s.c.doRaw(ctx, "PUT", "/v1/sessions/"+url.PathEscape(s.ID)+"/manifest", bytes.NewReader(b), "application/octet-stream", nil, nil, nil)
	})
}

// Commit makes the snapshot visible. It is not retried blindly: a retry after
// a lost response would see a conflict, which the caller resolves by listing.
func (s *Session) Commit(ctx context.Context, cr api.CommitRequest) (*api.CommitResponse, error) {
	var r api.CommitResponse
	return &r, s.c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(s.ID)+"/commit", cr, &r, nil)
}

// Abort discards the session.
func (s *Session) Abort(ctx context.Context) error {
	return s.c.do(ctx, "DELETE", "/v1/sessions/"+url.PathEscape(s.ID), nil, nil, nil)
}

// StartRun records the start of a run.
func (c *Client) StartRun(ctx context.Context, r api.RunStart) (string, error) {
	var out api.RunStartResponse
	err := c.do(ctx, "POST", "/v1/runs", r, &out, nil)
	return out.Run, err
}

// FinishRun records the end of a run.
func (c *Client) FinishRun(ctx context.Context, id string, f api.RunFinish) error {
	return c.do(ctx, "POST", "/v1/runs/"+url.PathEscape(id)+"/finish", f, nil, nil)
}

// ---- server-to-server synchronisation ---------------------------------------

// SyncList lists every committed snapshot of the remote repository.
func (c *Client) SyncList(ctx context.Context) ([]api.SnapshotRef, error) {
	var l api.SnapshotList
	err := retry(ctx, func() error { return c.do(ctx, "GET", "/v1/sync/snapshots", nil, &l, nil) })
	return l.Snapshots, err
}

// SyncMissing asks a destination which chunk ids it lacks.
func (c *Client) SyncMissing(ctx context.Context, ids []string) ([]string, error) {
	var out api.MissingResponse
	err := retry(ctx, func() error {
		return c.do(ctx, "POST", "/v1/sync/chunks/missing", api.MissingRequest{IDs: ids}, &out, nil)
	})
	return out.Missing, err
}

// SyncPutChunk uploads one blob to a destination.
func (c *Client) SyncPutChunk(ctx context.Context, id string, blob []byte) error {
	return retry(ctx, func() error {
		return c.doRaw(ctx, "PUT", "/v1/sync/chunks/"+url.PathEscape(id), bytes.NewReader(blob), "application/octet-stream", nil, nil, nil)
	})
}

// SyncPutSnapshot commits a snapshot on a destination.
func (c *Client) SyncPutSnapshot(ctx context.Context, domain string, put api.SyncSnapshotPut) error {
	return retry(ctx, func() error {
		return c.do(ctx, "PUT", "/v1/sync/snapshots/"+url.PathEscape(put.Meta.GroupID)+"/"+url.PathEscape(put.Meta.SnapshotID), put, nil, map[string]string{api.HeaderDomain: domain})
	})
}

// Objects lists the storage objects the repository holds.
func (c *Client) Objects(ctx context.Context) ([]api.RepoObject, error) {
	var l api.ObjectList
	err := retry(ctx, func() error { return c.do(ctx, "GET", "/v1/objects", nil, &l, nil) })
	return l.Objects, err
}

// PutEscrow stores the wrapped master key (create-only).
func (c *Client) PutEscrow(ctx context.Context, blob []byte) error {
	return c.doRaw(ctx, "PUT", "/v1/escrow", bytes.NewReader(blob), "application/octet-stream", nil, map[string]string{api.HeaderDomain: c.Domain}, nil)
}

// GetEscrow fetches the wrapped master key of the repository.
func (c *Client) GetEscrow(ctx context.Context) ([]byte, error) { return c.get(ctx, "/v1/escrow") }

// readSized reads at most max bytes of r. When the length is announced (a chunk, an index) the
// buffer is allocated once at that size instead of growing by doubling (which for a 4 MiB chunk
// allocates about twice that and leaves the garbage to the collector). The announcement is the
// peer's word: at most presizeMax is allocated up front, the rest only as bytes really arrive.
func readSized(r io.Reader, length, max int64) ([]byte, error) {
	if length <= 0 || length > max {
		return io.ReadAll(io.LimitReader(r, max))
	}
	buf := bytes.NewBuffer(make([]byte, 0, min(length, presizeMax)+bytes.MinRead)) // +MinRead: the final EOF read never regrows
	n, err := buf.ReadFrom(io.LimitReader(r, length))
	if err != nil {
		return nil, err
	}
	if n != length {
		return nil, io.ErrUnexpectedEOF // shorter than announced: a broken transfer, never a short success
	}
	return buf.Bytes(), nil
}

// presizeMax bounds what an announced length can make readSized allocate before data arrives.
const presizeMax = 16 << 20
