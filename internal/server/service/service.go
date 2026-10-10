// Package service is the single service layer behind incus-backup-server. The
// HTTP API and the server CLI (and later the GUI) all call it, so every
// operation has exactly one implementation. PostgreSQL holds operational state;
// the datastore holds payload and snapshot/chunk relationships.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/server/db"
)

// Errors the HTTP layer maps to status codes.
var (
	ErrNotFound     = errors.New("not found")
	ErrForbidden    = errors.New("forbidden")
	ErrConflict     = errors.New("conflict")
	ErrBadRequest   = errors.New("bad request")
	ErrUnauthorized = errors.New("unauthorized")
)

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadRequest, fmt.Sprintf(format, a...))
}

// maxClockSkew is how far into the future a host-supplied snapshot date may lie.
const defaultMaxClockSkew = 5 * time.Minute

// Service is the server logic.
type Service struct {
	DB       *db.DB
	StateDir string
	Version  string
	Log      *slog.Logger
	Now      func() time.Time
	// MaxClockSkew bounds how far into the future a host may date a snapshot.
	MaxClockSkew time.Duration
	// Lease is the inactivity lease of upload sessions (0 = DefaultLease).
	Lease time.Duration

	mu       sync.Mutex
	serverID string
	daemon   bool       // this process owns the repositories (see daemonlock.go)
	opGuard  guardState // maintenance lock held by a non-daemon process for the length of one operation
	stores   map[string]*datastore.Store
	modes    map[string]*datastore.ModeBackend
	sessions map[string]*session
	syncPins map[string]*syncPin
	pkiState certState
	// Certs tunes the certificate lifecycle (zero values = defaults, see certs.go).
	Certs CertOptions
	// lastSeen: host id -> hostSeen last written to hosts.last_seen_at (writes are rate-limited)
	lastSeen sync.Map
}

// hostSeenEvery rate-limits the hosts.last_seen_at write that every authenticated request used to
// make: one PostgreSQL commit per chunk upload, all workers of a host on the same row.
const hostSeenEvery = time.Minute

type hostSeen struct {
	at   time.Time
	addr string
}

// New creates a service. StateDir holds the CA and server certificate.
func New(d *db.DB, stateDir, version string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Service{DB: d, StateDir: stateDir, Version: version, Log: log, Now: time.Now, MaxClockSkew: defaultMaxClockSkew,
		stores: map[string]*datastore.Store{}, modes: map[string]*datastore.ModeBackend{}, sessions: map[string]*session{}, syncPins: map[string]*syncPin{}}
}

// Repo is a repository row.
type Repo struct {
	ID       string
	Name     string
	Path     string // filesystem directory, or "s3://bucket/prefix" (descriptive only)
	Backend  string // "filesystem" or "s3"
	DomainID string
	// S3: non-secret settings (JSON) and the secret-store credential id.
	S3Raw        []byte
	CredentialID string
	DatastoreID  string
	// Mode is the maintenance mode: online, read-only or offline (see datastore.Mode).
	Mode       string
	ModeReason string
}

// Host is an enrolled certificate identity.
type Host struct {
	ID          string `json:"id"`
	RepoID      string `json:"repo_id"`
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	Fingerprint string `json:"fingerprint"`
	Disabled    bool   `json:"disabled"`
}

// ---- repositories ------------------------------------------------------------

// ServerID is the identity of this server instance (stored in PostgreSQL). It is what a
// repository's owner.json records.
func (s *Service) ServerID(ctx context.Context) (string, error) {
	s.mu.Lock()
	id := s.serverID
	s.mu.Unlock()
	if id != "" {
		return id, nil
	}
	if err := s.DB.Pool().QueryRow(ctx, `SELECT id FROM server_identity`).Scan(&id); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.serverID = id
	s.mu.Unlock()
	return id, nil
}

func hostName() string {
	h, _ := os.Hostname()
	return h
}

// CreateRepository creates a repository row and initialises its filesystem datastore.
func (s *Service) CreateRepository(ctx context.Context, name, path string) (*Repo, error) {
	return s.createFS(ctx, name, path, false, false)
}

// AttachRepository registers an EXISTING filesystem datastore (disaster recovery, or moving a
// repository to this server). If it belongs to another server, adopt must be set explicitly.
func (s *Service) AttachRepository(ctx context.Context, name, path string, adopt bool) (*Repo, error) {
	return s.createFS(ctx, name, path, true, adopt)
}

func (s *Service) createFS(ctx context.Context, name, path string, attach, adopt bool) (*Repo, error) {
	if !repoNameRe.MatchString(name) {
		return nil, bad("invalid repository name")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	serverID, err := s.ServerID(ctx)
	if err != nil {
		return nil, err
	}
	var dsID string
	if attach {
		b, err := datastore.NewFSBackend(abs)
		if err != nil {
			return nil, err
		}
		st, err := datastore.OpenOwned(b, serverID, hostName(), adopt)
		if err != nil {
			return nil, err
		}
		m, err := st.Meta()
		if err != nil {
			return nil, err
		}
		dsID = m.ID
	} else {
		if err := datastore.EnsureEmptyDir(abs); err != nil {
			return nil, err
		}
		b, err := datastore.NewFSBackend(abs)
		if err != nil {
			return nil, err
		}
		m, err := datastore.InitOwned(b, serverID, hostName())
		if err != nil {
			return nil, err
		}
		dsID = m.ID
	}
	r := &Repo{Name: name, Path: abs, Backend: BackendFilesystem, DatastoreID: dsID}
	err = s.DB.Pool().QueryRow(ctx,
		`INSERT INTO repositories (name, datastore_path, backend, datastore_id) VALUES ($1,$2,'filesystem',$3) RETURNING id`, name, abs, dsID).Scan(&r.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: this datastore is already attached as another repository (or that name is taken)", ErrConflict)
		}
		return nil, fmt.Errorf("creating repository: %w", err)
	}
	return r, nil
}

const repoCols = `id, name, datastore_path, backend, s3_settings, COALESCE(credential_id::text,''), datastore_id, COALESCE(domain_id,''), mode, mode_reason`

func scanRepo(row pgx.Row) (*Repo, error) {
	r := &Repo{}
	err := row.Scan(&r.ID, &r.Name, &r.Path, &r.Backend, &r.S3Raw, &r.CredentialID, &r.DatastoreID, &r.DomainID, &r.Mode, &r.ModeReason)
	return r, err
}

// Repository looks a repository up by name.
func (s *Service) Repository(ctx context.Context, name string) (*Repo, error) {
	r, err := scanRepo(s.DB.Pool().QueryRow(ctx, `SELECT `+repoCols+` FROM repositories WHERE name=$1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: repository %q", ErrNotFound, name)
	}
	return r, err
}

// RepositoryByID looks a repository up by id.
func (s *Service) RepositoryByID(ctx context.Context, id string) (*Repo, error) {
	r, err := scanRepo(s.DB.Pool().QueryRow(ctx, `SELECT `+repoCols+` FROM repositories WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: repository", ErrNotFound)
	}
	return r, err
}

// Repositories lists all repositories.
func (s *Service) Repositories(ctx context.Context) ([]Repo, error) {
	rows, err := s.DB.Pool().Query(ctx, `SELECT `+repoCols+` FROM repositories ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// Store returns the opened datastore of a repository.
func (s *Service) Store(r *Repo) (*datastore.Store, error) {
	return s.openStore(context.Background(), r)
}

func (s *Service) openStore(ctx context.Context, r *Repo) (*datastore.Store, error) {
	s.mu.Lock()
	if st, ok := s.stores[r.ID]; ok {
		s.mu.Unlock()
		return st, nil
	}
	s.mu.Unlock()
	// The handshake runs in the persisted mode: an offline repository is not touched at all (its storage
	// may be gone; NewFSBackend would even create the missing directory), a read-only one is read but
	// never written (no ownership claim, only the check).
	mode := s.persistedMode(ctx, r.ID)
	if mode == datastore.ModeOffline {
		return nil, fmt.Errorf("repository %q: %w", r.Name, datastore.ErrOffline)
	}
	serverID, err := s.ServerID(ctx)
	if err != nil {
		return nil, err
	}
	var b datastore.Backend
	if r.Backend == BackendS3 {
		if b, err = s.S3Backend(ctx, r); err != nil {
			return nil, err
		}
	} else if b, err = datastore.NewFSBackend(r.Path); err != nil {
		return nil, err
	}
	mb := datastore.NewModeBackend(b, mode)
	b = mb
	var st *datastore.Store
	if mode == datastore.ModeOnline {
		st, err = datastore.OpenOwned(b, serverID, hostName(), false)
	} else if st, err = datastore.OpenBackend(b); err == nil {
		if o, oerr := datastore.ReadOwner(b); oerr != nil {
			err = oerr
		} else if o != nil && o.ServerID != serverID {
			err = fmt.Errorf("%w (owner %s)", datastore.ErrForeignOwner, o.ServerID)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("repository %q: %w", r.Name, err)
	}
	if m, err := st.Meta(); err != nil {
		return nil, err
	} else if r.DatastoreID != "" && m.ID != r.DatastoreID {
		return nil, fmt.Errorf("repository %q: the datastore at %s is a different repository (id %s, expected %s); refusing to use it",
			r.Name, b.Describe(), m.ID, r.DatastoreID)
	}
	s.mu.Lock()
	if cur, ok := s.stores[r.ID]; ok { // lost a race: keep the first so its session pins stay authoritative
		s.mu.Unlock()
		return cur, nil
	}
	s.stores[r.ID] = st
	s.modes[r.ID] = mb
	s.mu.Unlock()
	// A mode change committed while this store was being opened found no store to apply itself to:
	// read the mode again now that the store is registered (any later change finds it).
	s.applyMode(ctx, r.ID)
	return st, nil
}

// rebindStores gives every open S3 store that uses the credential its new secret IN PLACE: a store
// with running sessions is never replaced, because its pins would be lost.
func (s *Service) rebindStores(ctx context.Context, credID string, sec S3Secret) {
	repos, err := s.Repositories(ctx)
	if err != nil {
		return
	}
	for _, r := range repos {
		if r.CredentialID != credID {
			continue
		}
		s.mu.Lock()
		st := s.stores[r.ID]
		s.mu.Unlock()
		if st == nil {
			continue
		}
		var set S3Settings
		if json.Unmarshal(r.S3Raw, &set) != nil {
			continue
		}
		if b, ok := datastore.Unwrap(st.Backend()).(*datastore.S3Backend); ok {
			if err := b.Rebind(set.config(&sec)); err != nil {
				s.Log.Error("rebinding an S3 store after a credential rotation failed", "repository", r.Name, "err", err)
			}
		}
	}
}

// dropStores forgets opened datastores (after a credential rotation); running sessions keep theirs.
func (s *Service) dropStores() {
	s.mu.Lock()
	s.stores = map[string]*datastore.Store{}
	s.modes = map[string]*datastore.ModeBackend{}
	s.mu.Unlock()
}

// pinDomain pins the key-domain id on first use and rejects a different one.
func (s *Service) pinDomain(ctx context.Context, repoID, domain string) error {
	if domain == "" {
		return bad("missing %s header", api.HeaderDomain)
	}
	if len(domain) != 16 || strings.Trim(domain, "0123456789abcdef") != "" {
		return bad("malformed key domain id")
	}
	ct, err := s.DB.Pool().Exec(ctx, `UPDATE repositories SET domain_id=$2 WHERE id=$1 AND domain_id IS NULL`, repoID, domain)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 1 {
		return nil
	}
	var cur string
	if err := s.DB.Pool().QueryRow(ctx, `SELECT COALESCE(domain_id,'') FROM repositories WHERE id=$1`, repoID).Scan(&cur); err != nil {
		return err
	}
	if cur != domain {
		return fmt.Errorf("%w: this repository belongs to key domain %s, the client uses %s (different master key)", ErrForbidden, cur, domain)
	}
	return nil
}

// ---- PKI / state dir -----------------------------------------------------------

func (s *Service) stateFile(n string) string { return filepath.Join(s.StateDir, n) }

// ---- enrolment -----------------------------------------------------------------

func tokenHash(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// CreateToken creates a one-time enrolment token for a host (or sync peer).
// The token embeds the CA fingerprint so the client can pin the server.
func (s *Service) CreateToken(ctx context.Context, repoName, hostName, scope string, ttl time.Duration) (string, error) {
	if hostName == "" || strings.ContainsAny(hostName, " /\t") {
		return "", bad("invalid host name")
	}
	switch scope {
	case api.ScopeBackup, api.ScopeSyncRead, api.ScopeSyncWrite:
	default:
		return "", bad("invalid scope %q", scope)
	}
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return "", err
	}
	fp, err := s.CAFingerprint()
	if err != nil {
		return "", err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(raw[:])
	_, err = s.DB.Pool().Exec(ctx,
		`INSERT INTO enrollment_tokens (repository_id, host_name, scope, token_hash, expires_at) VALUES ($1,$2,$3,$4,$5)`,
		repo.ID, hostName, scope, tokenHash(secret), s.Now().Add(ttl))
	if err != nil {
		return "", err
	}
	return "ibt1." + fp + "." + secret, nil
}

// ParseToken splits a token into the CA fingerprint and secret.
func ParseToken(tok string) (fingerprint, secret string, err error) {
	p := strings.Split(tok, ".")
	if len(p) != 3 || p[0] != "ibt1" || len(p[1]) != 64 || p[2] == "" {
		return "", "", errors.New("malformed enrolment token")
	}
	return p[1], p[2], nil
}

// Hosts lists the hosts of a repository.
func (s *Service) Hosts(ctx context.Context, repoName string) ([]HostRow, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool().Query(ctx, `SELECT name, scope, cert_fingerprint, disabled, enrolled_at, last_seen_at, COALESCE(last_addr,'')
		FROM hosts WHERE repository_id=$1 ORDER BY name, scope`, repo.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostRow
	for rows.Next() {
		var h HostRow
		if err := rows.Scan(&h.Name, &h.Scope, &h.Fingerprint, &h.Disabled, &h.Enrolled, &h.LastSeen, &h.LastAddr); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HostRow is a host listing entry.
type HostRow struct {
	Name        string     `json:"name"`
	Scope       string     `json:"scope"`
	Fingerprint string     `json:"fingerprint"`
	LastAddr    string     `json:"last_addr"`
	Disabled    bool       `json:"disabled"`
	Enrolled    time.Time  `json:"enrolled"`
	LastSeen    *time.Time `json:"last_seen"`
}

// SetHostDisabled disables or re-enables a host (revocation).
func (s *Service) SetHostDisabled(ctx context.Context, repoName, hostName, scope string, disabled bool) error {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	ct, err := s.DB.Pool().Exec(ctx, `UPDATE hosts SET disabled=$4 WHERE repository_id=$1 AND name=$2 AND scope=$3`, repo.ID, hostName, scope, disabled)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: host %s/%s", ErrNotFound, hostName, scope)
	}
	return nil
}

func hexID(b []byte) string { return hex.EncodeToString(b) }
