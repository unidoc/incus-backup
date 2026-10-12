package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/datastore"
)

// Repository backends.
const (
	BackendFilesystem = "filesystem"
	BackendS3         = "s3"
)

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// S3Settings are the non-secret settings of an S3 repository. They live in PostgreSQL
// (repositories.s3_settings); the access key lives in the secret store.
type S3Settings struct {
	Endpoint           string `json:"endpoint"`
	Region             string `json:"region,omitempty"`
	Bucket             string `json:"bucket"`
	Prefix             string `json:"prefix,omitempty"`
	NoTLS              bool   `json:"no_tls,omitempty"`
	CAPEM              string `json:"ca_pem,omitempty"` // a private CA certificate (public data)
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
	PathStyle          bool   `json:"path_style,omitempty"`
}

func (c S3Settings) config(sec *S3Secret) datastore.S3Config {
	return datastore.S3Config{Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket, Prefix: c.Prefix,
		AccessKey: sec.AccessKey, SecretKey: sec.SecretKey, NoTLS: c.NoTLS, CAPEM: c.CAPEM,
		InsecureSkipVerify: c.InsecureSkipVerify, PathStyle: c.PathStyle}
}

// S3Backend opens the S3 backend of a repository from its settings and credential.
func (s *Service) S3Backend(ctx context.Context, r *Repo) (*datastore.S3Backend, error) {
	if r.CredentialID == "" {
		return nil, fmt.Errorf("repository %q has no S3 credential", r.Name)
	}
	sec, err := s.credentialSecret(ctx, r.CredentialID)
	if err != nil {
		return nil, fmt.Errorf("repository %q: %w", r.Name, err)
	}
	var set S3Settings
	if err := json.Unmarshal(r.S3Raw, &set); err != nil {
		return nil, err
	}
	return datastore.NewS3Backend(set.config(sec))
}

// S3Params is what an operator supplies (CLI flags or the UI form) to add an S3 repository.
// Either Credential names an existing credential, or AccessKey/SecretKey create one named
// "<repository>-s3".
type S3Params struct {
	S3Settings
	Credential string
	AccessKey  string
	SecretKey  string
}

// CreateS3Repository creates a repository whose datastore is an S3 bucket/prefix.
// The connection is probed first: a repository on a service that cannot do conditional
// writes would silently lose its commit guarantees. (Inline credentials are the
// convenience path used by the CLI and tests.)
func (s *Service) CreateS3Repository(ctx context.Context, name string, cfg datastore.S3Config) (*Repo, error) {
	return s.AddS3Repository(ctx, name, S3Params{
		S3Settings: S3Settings{Endpoint: cfg.Endpoint, Region: cfg.Region, Bucket: cfg.Bucket, Prefix: cfg.Prefix, NoTLS: cfg.NoTLS,
			CAPEM: cfg.CAPEM, InsecureSkipVerify: cfg.InsecureSkipVerify, PathStyle: cfg.PathStyle},
		AccessKey: cfg.AccessKey, SecretKey: cfg.SecretKey})
}

// AddS3Repository creates a new S3 repository (see S3Params).
func (s *Service) AddS3Repository(ctx context.Context, name string, p S3Params) (*Repo, error) {
	return s.addS3(ctx, name, p, false, false)
}

// AttachS3Repository registers an EXISTING S3 repository (disaster recovery, or moving a
// repository to a new server). The repository must already hold a datastore. If it belongs to
// another server, adopt must be set explicitly.
func (s *Service) AttachS3Repository(ctx context.Context, name string, p S3Params, adopt bool) (*Repo, error) {
	return s.addS3(ctx, name, p, true, adopt)
}

func (s *Service) addS3(ctx context.Context, name string, p S3Params, attach, adopt bool) (*Repo, error) {
	if !repoNameRe.MatchString(name) {
		return nil, bad("invalid repository name")
	}
	credID, createdCred, err := s.resolveCredential(ctx, name, p)
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		if createdCred {
			_ = s.DeleteCredential(ctx, name+"-s3")
		}
	}
	sec, err := s.credentialSecret(ctx, credID)
	if err != nil {
		cleanup()
		return nil, err
	}
	b, err := datastore.NewS3Backend(p.S3Settings.config(sec))
	if err != nil {
		cleanup()
		return nil, bad("%v", err)
	}
	if bad := firstProbeFailure(b.Probe()); bad != "" {
		cleanup()
		return nil, fmt.Errorf("S3 target is not usable: %s", bad)
	}
	serverID, err := s.ServerID(ctx)
	if err != nil {
		cleanup()
		return nil, err
	}
	var dsID string
	if attach {
		st, err := datastore.OpenOwned(b, serverID, hostName(), adopt)
		if err != nil {
			cleanup()
			return nil, err
		}
		m, err := st.Meta()
		if err != nil {
			cleanup()
			return nil, err
		}
		dsID = m.ID
	} else {
		m, err := datastore.InitOwned(b, serverID, hostName())
		if err != nil {
			cleanup()
			return nil, err
		}
		dsID = m.ID
	}
	raw, _ := json.Marshal(p.S3Settings)
	ep := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(p.Endpoint, "https://"), "http://"), "/")
	desc := "s3://" + ep + "/" + p.Bucket + "/" + strings.Trim(p.Prefix, "/")
	r := &Repo{Name: name, Path: desc, Backend: BackendS3, CredentialID: credID, S3Raw: raw, DatastoreID: dsID}
	err = s.DB.Pool().QueryRow(ctx,
		`INSERT INTO repositories (name, datastore_path, backend, s3_settings, credential_id, datastore_id) VALUES ($1,$2,'s3',$3,$4::uuid,$5) RETURNING id`,
		name, desc, raw, credID, dsID).Scan(&r.ID)
	if err != nil {
		cleanup()
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: this datastore is already attached as another repository (or that name is taken)", ErrConflict)
		}
		return nil, fmt.Errorf("creating repository: %w", err)
	}
	return r, nil
}

func (s *Service) resolveCredential(ctx context.Context, repoName string, p S3Params) (id string, created bool, err error) {
	if p.Credential != "" {
		err = s.DB.Pool().QueryRow(ctx, `SELECT id::text FROM credentials WHERE name=$1 AND kind='s3'`, p.Credential).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, fmt.Errorf("%w: credential %q", ErrNotFound, p.Credential)
		}
		return id, false, err
	}
	c, err := s.CreateS3Credential(ctx, repoName+"-s3", S3Secret{AccessKey: p.AccessKey, SecretKey: p.SecretKey})
	if err != nil {
		return "", false, err
	}
	return c.ID, true, nil
}

func firstProbeFailure(rs []datastore.ProbeResult) string {
	for _, r := range rs {
		if !r.OK {
			return r.Op + ": " + r.Err
		}
	}
	return ""
}

// ProbeRepository runs the doctor checks against a repository's datastore. Filesystem
// repositories report one check (open + metadata read).
func (s *Service) ProbeRepository(ctx context.Context, name string) ([]datastore.ProbeResult, error) {
	repo, err := s.Repository(ctx, name)
	if err != nil {
		return nil, err
	}
	// the maintenance mode binds the probe too: offline = nothing touches the datastore, read-only = no
	// write (the S3 probe's scratch object, the filesystem open's directory creation)
	switch s.persistedMode(ctx, repo.ID) {
	case datastore.ModeOffline:
		return []datastore.ProbeResult{{Op: "maintenance mode", Err: "the repository is offline: nothing touches its datastore (set it read-only or online to probe it)"}}, nil
	case datastore.ModeReadOnly:
		_, oerr := s.openStore(ctx, repo)
		pr := datastore.ProbeResult{Op: "open repository read-only (datastore.json, owner); write probe skipped: the repository is read-only", OK: oerr == nil}
		if oerr != nil {
			pr.Err = oerr.Error()
		}
		return []datastore.ProbeResult{pr}, nil
	}
	if repo.Backend != BackendS3 {
		_, err := datastore.Open(repo.Path)
		r := datastore.ProbeResult{Op: "open filesystem datastore", OK: err == nil}
		if err != nil {
			r.Err = err.Error()
		}
		return []datastore.ProbeResult{r}, nil
	}
	b, err := s.S3Backend(ctx, repo)
	if err != nil {
		return []datastore.ProbeResult{{Op: "load S3 settings and credential", Err: err.Error()}}, nil
	}
	rs := b.Probe()
	_, oerr := s.openStore(ctx, repo)
	pr := datastore.ProbeResult{Op: "open repository (datastore.json, owner)", OK: oerr == nil}
	if oerr != nil {
		pr.Err = oerr.Error()
	}
	return append(rs, pr), nil
}
