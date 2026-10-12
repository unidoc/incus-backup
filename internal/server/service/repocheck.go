package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/datastore"
)

// Repository checks for the management UI: "test connection" before a repository is created or
// attached, and a deeper health check of an existing one. Every check is read-only except the S3
// probe's scratch object under `_probe/` (written and deleted again, exactly as `repo probe` does).
// Nothing here creates a directory, a credential, a datastore or an owner record.

// Check levels.
const (
	CheckPass = "pass"
	CheckWarn = "warn"
	CheckFail = "fail"
	CheckSkip = "skip" // not run because an earlier check failed
	// CheckUnknown: the setting could not be read (missing permission); not an error.
	CheckUnknown = "unknown"
)

// CheckItem is one step of a repository check. ID is stable (UI copy and advice key off it).
type CheckItem struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Level  string `json:"level"`
	Detail string `json:"detail,omitempty"`
}

// DatastoreFacts is what the check found at the location: an existing repository and who owns it.
type DatastoreFacts struct {
	Exists  bool             `json:"exists"`
	ID      string           `json:"id,omitempty"`
	Created *time.Time       `json:"created,omitempty"`
	Owner   *datastore.Owner `json:"owner,omitempty"`
	// OwnedByThisServer: owner.json names this server.
	OwnedByThisServer bool `json:"owned_by_this_server"`
	// AttachedAs names the repository of this server that already uses this datastore (or path).
	AttachedAs string `json:"attached_as,omitempty"`
}

// RepoCheckReport is the result of CheckRepositorySettings / CheckRepository.
type RepoCheckReport struct {
	Kind string `json:"kind"`
	// Purpose: "create", "attach" or "existing".
	Purpose   string         `json:"purpose"`
	Location  string         `json:"location"`
	Checks    []CheckItem    `json:"checks"`
	Datastore DatastoreFacts `json:"datastore"`
	ServerID  string         `json:"server_id"`
	// Ready: no check failed. For create/attach it predicts that the real request passes the
	// server's own checks (it is stricter: a relative path or a taken name fails here already).
	Ready bool `json:"ready"`
	// NeedsAdopt: the repository belongs to another server; attaching needs adopt=true.
	NeedsAdopt bool `json:"needs_adopt"`
	// Filesystem only: free and total bytes of the filesystem holding the path (nil when unknown).
	FreeBytes  *uint64 `json:"free_bytes,omitempty"`
	TotalBytes *uint64 `json:"total_bytes,omitempty"`
}

// RepoCheckRequest describes a repository that does not exist yet (the create/attach form).
type RepoCheckRequest struct {
	Name       string      `json:"name"`
	Kind       string      `json:"kind"`
	Path       string      `json:"path,omitempty"`
	S3         *S3Settings `json:"s3,omitempty"`
	Credential string      `json:"credential,omitempty"`
	AccessKey  string      `json:"access_key,omitempty"`
	SecretKey  string      `json:"secret_key,omitempty"`
	Attach     bool        `json:"attach,omitempty"`
	Adopt      bool        `json:"adopt,omitempty"`
}

type checkList struct{ items []CheckItem }

func (c *checkList) add(id, label, level, detail string) {
	c.items = append(c.items, CheckItem{ID: id, Label: label, Level: level, Detail: detail})
}

func (c *checkList) failed() bool {
	for _, it := range c.items {
		if it.Level == CheckFail {
			return true
		}
	}
	return false
}

// CheckRepositorySettings checks settings for a repository that is about to be created or attached.
func (s *Service) CheckRepositorySettings(ctx context.Context, req RepoCheckRequest) (*RepoCheckReport, error) {
	serverID, err := s.ServerID(ctx)
	if err != nil {
		return nil, err
	}
	purpose := "create"
	if req.Attach {
		purpose = "attach"
	}
	rep := &RepoCheckReport{Kind: req.Kind, Purpose: purpose, ServerID: serverID}
	cl := &checkList{}
	switch {
	case !repoNameRe.MatchString(req.Name):
		cl.add("name", "Repository name", CheckFail, "Use letters, digits, dot, dash and underscore; start with a letter or digit.")
	default:
		if _, err := s.Repository(ctx, req.Name); err == nil {
			cl.add("name", "Repository name", CheckFail, fmt.Sprintf("A repository called %q exists already on this server.", req.Name))
		} else if errors.Is(err, ErrNotFound) {
			cl.add("name", "Repository name", CheckPass, fmt.Sprintf("%q is free.", req.Name))
		} else {
			return nil, err
		}
	}
	switch req.Kind {
	case BackendFilesystem:
		if err := s.checkFSPath(ctx, req.Path, req.Attach, req.Adopt, true, rep, cl); err != nil {
			return nil, err
		}
	case BackendS3:
		if err := s.checkS3Settings(ctx, req, rep, cl); err != nil {
			return nil, err
		}
	default:
		return nil, bad("kind must be filesystem or s3")
	}
	rep.Checks = cl.items
	rep.Ready = !cl.failed()
	return rep, nil
}

// CheckRepository runs the full check against an existing repository (operator: no secret leaves the server).
func (s *Service) CheckRepository(ctx context.Context, name string) (*RepoCheckReport, error) {
	repo, err := s.Repository(ctx, name)
	if err != nil {
		return nil, err
	}
	serverID, err := s.ServerID(ctx)
	if err != nil {
		return nil, err
	}
	rep := &RepoCheckReport{Kind: repo.Backend, Purpose: "existing", Location: repo.Path, ServerID: serverID}
	cl := &checkList{}
	// the maintenance mode is honoured by the check too: offline = do not touch the datastore at all,
	// read-only = no write (the probe's scratch object is skipped)
	if repo.Mode == "offline" {
		cl.add("mode", "Maintenance mode", CheckSkip, "Not checked: the repository is offline, so nothing touches its datastore. Set it online (or read-only for a read-only check) first.")
		rep.Checks = cl.items
		return rep, nil
	}
	readOnly := repo.Mode == "read-only"
	if repo.Backend == BackendS3 {
		b, err := s.S3Backend(ctx, repo)
		if err != nil {
			cl.add("credential", "S3 settings and credential", CheckFail, err.Error()+". Rotate the credential on the Credentials tab if it was lost or changed.")
		} else {
			var set S3Settings
			_ = json.Unmarshal(repo.S3Raw, &set)
			tlsChecks(set, cl)
			if readOnly {
				probeS3ReadOnly(b, cl)
			} else {
				s.probeS3(ctx, b, cl)
			}
			s.inspectBackend(ctx, b, rep, cl, "existing", false, repo.Name)
		}
	} else {
		// no write test unless online: an operator may have taken the disk out of service
		if err := s.checkFSPath(ctx, repo.Path, true, false, repo.Mode == "" || repo.Mode == "online", rep, cl); err != nil {
			return nil, err
		}
		// the path check reports "already attached as <name>": that is this repository, not a conflict
		cl.items = filterOut(cl.items, "attached")
		rep.Datastore.AttachedAs = repo.Name
	}
	if !cl.failed() {
		_, oerr := s.openStore(ctx, repo)
		if oerr != nil {
			cl.add("open", "Open the repository", CheckFail, oerr.Error())
		} else {
			cl.add("open", "Open the repository", CheckPass, "datastore.json and owner.json match this server's records.")
		}
	}
	if repo.DatastoreID != "" && rep.Datastore.ID != "" && rep.Datastore.ID != repo.DatastoreID {
		cl.add("identity", "Datastore identity", CheckFail, fmt.Sprintf("The location holds datastore %s, but this repository was created with %s: a different repository is behind the same path or bucket.", rep.Datastore.ID, repo.DatastoreID))
	}
	rep.Checks = cl.items
	rep.Ready = !cl.failed()
	return rep, nil
}

func filterOut(items []CheckItem, id string) []CheckItem {
	out := items[:0]
	for _, it := range items {
		if it.ID != id {
			out = append(out, it)
		}
	}
	return out
}

// ---- filesystem ---------------------------------------------------------------------------

func (s *Service) checkFSPath(ctx context.Context, path string, attach, adopt, writeTest bool, rep *RepoCheckReport, cl *checkList) error {
	const lbl = "Directory"
	if strings.TrimSpace(path) == "" {
		cl.add("path", lbl, CheckFail, "Enter the directory on the server that holds (or will hold) the repository.")
		return nil
	}
	if !filepath.IsAbs(path) {
		cl.add("path", lbl, CheckFail, "Use an absolute path (starting with /). A relative path would depend on the server's working directory.")
		return nil
	}
	abs := filepath.Clean(path)
	rep.Location = abs
	// another repository of this server on the same directory
	rs, err := s.Repositories(ctx)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Backend == BackendFilesystem && filepath.Clean(r.Path) == abs {
			cl.add("attached", "Not used by another repository", CheckFail, fmt.Sprintf("This directory is already the datastore of repository %q.", r.Name))
			rep.Datastore.AttachedAs = r.Name
		}
	}
	fi, err := os.Stat(abs)
	exists := err == nil
	switch {
	case err != nil && !errors.Is(err, os.ErrNotExist):
		cl.add("path", lbl, CheckFail, fmt.Sprintf("The server cannot look at %s: %v.", abs, err))
		return nil
	case exists && !fi.IsDir():
		cl.add("path", lbl, CheckFail, abs+" exists and is not a directory.")
		return nil
	case !exists && attach:
		cl.add("path", lbl, CheckFail, abs+" does not exist. Attach needs the directory of an existing repository; to start a new one, choose Create.")
		return nil
	case !exists:
		parent := nearestDir(abs)
		if parent == "" {
			cl.add("path", lbl, CheckFail, "No parent directory of "+abs+" exists.")
			return nil
		}
		if err := syscall.Access(parent, 0x2|0x1); err != nil { // W_OK|X_OK for the server's user
			cl.add("path", lbl, CheckFail, fmt.Sprintf("%s does not exist and the server's user cannot create it in %s (%v). Create it and give the service account ownership: mkdir -p %s && chown incus-backup: %s", abs, parent, err, abs, abs))
		} else {
			cl.add("path", lbl, CheckPass, fmt.Sprintf("%s does not exist yet; the server will create it in %s.", abs, parent))
		}
		statfs(parent, rep, cl)
		return nil
	}
	// the directory exists
	if err := syscall.Access(abs, 0x4|0x2|0x1); err != nil {
		cl.add("path", lbl, CheckFail, fmt.Sprintf("The server's user cannot read and write %s (%v). Fix the ownership: chown -R incus-backup: %s", abs, err, abs))
		return nil
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		cl.add("path", lbl, CheckFail, fmt.Sprintf("Cannot list %s: %v.", abs, err))
		return nil
	}
	meta, mErr := os.ReadFile(filepath.Join(abs, "datastore.json"))
	hasMeta := mErr == nil
	switch {
	case !attach && len(entries) == 0:
		cl.add("path", lbl, CheckPass, abs+" exists, is empty and writable.")
	case !attach && hasMeta:
		cl.add("path", lbl, CheckFail, abs+" already holds a repository (datastore.json). To use it on this server, choose Attach instead of Create.")
	case !attach:
		cl.add("path", lbl, CheckFail, fmt.Sprintf("%s is not empty (%d entries). A new repository needs an empty or missing directory.", abs, len(entries)))
	case !hasMeta:
		cl.add("path", lbl, CheckFail, abs+" is not a repository (no datastore.json). To start a new repository, choose Create.")
	default:
		cl.add("path", lbl, CheckPass, abs+" holds a repository and is writable.")
	}
	if hasMeta {
		var m datastore.Meta
		if err := json.Unmarshal(meta, &m); err != nil || m.Format != 1 {
			cl.add("datastore", "Repository format", CheckFail, "datastore.json is unreadable or has an unknown format.")
		} else {
			rep.Datastore.Exists, rep.Datastore.ID = true, m.ID
			c := m.Created
			rep.Datastore.Created = &c
			if attach {
				cl.add("datastore", "Repository format", CheckPass, "Repository format 1, id "+m.ID+".")
			}
		}
		if raw, err := os.ReadFile(filepath.Join(abs, "owner.json")); err == nil {
			var o datastore.Owner
			if json.Unmarshal(raw, &o) == nil {
				rep.Datastore.Owner = &o
			}
		}
		if attach {
			s.ownerChecks(ctx, rep, cl, adopt, "")
		}
	}
	statfs(abs, rep, cl)
	if writeTest && fsWriteTest(abs) != nil {
		cl.add("write", "Write test", CheckFail, "Writing a test file into "+abs+" failed: the filesystem may be read-only or full.")
	}
	return nil
}

func nearestDir(p string) string {
	for d := filepath.Dir(p); ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
		if d == filepath.Dir(d) {
			return ""
		}
	}
}

// fsWriteTest writes and removes a small file (the same kind of temporary file every datastore write makes).
func fsWriteTest(dir string) error {
	f, err := os.CreateTemp(dir, ".ib-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_, werr := f.Write([]byte("incus-backup check"))
	cerr := f.Close()
	rerr := os.Remove(name)
	return errors.Join(werr, cerr, rerr)
}

func statfs(dir string, rep *RepoCheckReport, cl *checkList) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return
	}
	free := st.Bavail * uint64(st.Bsize)
	total := st.Blocks * uint64(st.Bsize)
	rep.FreeBytes, rep.TotalBytes = &free, &total
	const gib = 1 << 30
	switch {
	case free < gib:
		cl.add("space", "Free space", CheckWarn, fmt.Sprintf("Only %.1f GiB free on this filesystem. Backups stop when it is full.", float64(free)/gib))
	case total > 0 && float64(free)/float64(total) < 0.1:
		cl.add("space", "Free space", CheckWarn, fmt.Sprintf("%.1f GiB free (%.0f %% of %.1f GiB): the filesystem is nearly full.", float64(free)/gib, 100*float64(free)/float64(total), float64(total)/gib))
	default:
		cl.add("space", "Free space", CheckPass, fmt.Sprintf("%.1f GiB free of %.1f GiB.", float64(free)/gib, float64(total)/gib))
	}
}

// ---- S3 -------------------------------------------------------------------------------------

func tlsChecks(set S3Settings, cl *checkList) {
	switch {
	case set.NoTLS || strings.HasPrefix(set.Endpoint, "http://"):
		cl.add("tls", "Transport security", CheckWarn, "Plain HTTP: requests are signed and backup data is encrypted, but object names, sizes and timing travel in clear. Use HTTPS outside a trusted network.")
	case set.InsecureSkipVerify:
		cl.add("tls", "Transport security", CheckWarn, "HTTPS without certificate verification: anyone between this server and the endpoint can pose as it. Prefer a private CA (CA certificate field).")
	case set.CAPEM != "":
		cl.add("tls", "Transport security", CheckPass, "HTTPS, verified against the private CA you gave.")
	default:
		cl.add("tls", "Transport security", CheckPass, "HTTPS, verified against the system's trusted CAs.")
	}
}

func (s *Service) checkS3Settings(ctx context.Context, req RepoCheckRequest, rep *RepoCheckReport, cl *checkList) error {
	if req.S3 == nil {
		cl.add("settings", "S3 settings", CheckFail, "Enter the endpoint and the bucket.")
		return nil
	}
	set := *req.S3
	ep := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(set.Endpoint, "https://"), "http://"), "/")
	rep.Location = "s3://" + ep + "/" + set.Bucket + "/" + strings.Trim(set.Prefix, "/")
	switch {
	case strings.TrimSpace(set.Endpoint) == "":
		cl.add("settings", "S3 settings", CheckFail, "Enter the endpoint (host:port or a URL), for example s3.eu-central-1.amazonaws.com.")
		return nil
	case strings.TrimSpace(set.Bucket) == "":
		cl.add("settings", "S3 settings", CheckFail, "Enter the bucket name. The bucket must exist already; the server does not create buckets.")
		return nil
	}
	var sec *S3Secret
	switch {
	case req.Credential != "":
		var id string
		err := s.DB.Pool().QueryRow(ctx, `SELECT id::text FROM credentials WHERE name=$1 AND kind='s3'`, req.Credential).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			cl.add("credential", "Credential", CheckFail, fmt.Sprintf("No stored S3 credential called %q.", req.Credential))
			return nil
		} else if err != nil {
			return err
		}
		if sec, err = s.credentialSecret(ctx, id); err != nil {
			cl.add("credential", "Credential", CheckFail, "The stored credential cannot be opened: "+err.Error())
			return nil
		}
		cl.add("credential", "Credential", CheckPass, fmt.Sprintf("Stored credential %q (access key %s).", req.Credential, sec.AccessKey))
	case req.AccessKey == "" || req.SecretKey == "":
		cl.add("credential", "Credential", CheckFail, "Enter the access key and the secret key, or choose a stored credential.")
		return nil
	default:
		sec = &S3Secret{AccessKey: req.AccessKey, SecretKey: req.SecretKey}
		cl.add("credential", "Credential", CheckPass, "New access key "+req.AccessKey+" (stored sealed when the repository is created; never shown again).")
	}
	tlsChecks(set, cl)
	b, err := datastore.NewS3Backend(set.config(sec))
	if err != nil {
		cl.add("connect", "Reach the endpoint and the bucket", CheckFail, err.Error())
		return nil
	}
	if !s.probeS3(ctx, b, cl) {
		return nil
	}
	s.inspectBackend(ctx, b, rep, cl, rep.Purpose, req.Adopt, "")
	return nil
}

var probeIDs = map[string][2]string{
	"connect + bucket exists":           {"connect", "Reach the endpoint and the bucket"},
	"put":                               {"put", "Write an object"},
	"put if-absent (conditional write)": {"conditional", "Conditional write (If-None-Match)"},
	"get":                               {"get", "Read it back"},
	"stat":                              {"stat", "Read its metadata"},
	"list":                              {"list", "List objects"},
	"delete":                            {"delete", "Delete an object"},
}

// probeS3 runs the datastore probe (the same steps `repo probe` and repository creation run).
// It reports whether every step passed.
func (s *Service) probeS3(ctx context.Context, b *datastore.S3Backend, cl *checkList) bool {
	type res struct{ rs []datastore.ProbeResult }
	ch := make(chan res, 1)
	go func() { ch <- res{b.Probe()} }()
	var rs []datastore.ProbeResult
	select {
	case r := <-ch:
		rs = r.rs
	case <-ctx.Done():
		cl.add("connect", "Reach the endpoint and the bucket", CheckFail, "The check was cancelled: "+ctx.Err().Error())
		return false
	}
	ok := true
	done := map[string]bool{}
	for _, r := range rs {
		id := probeIDs[r.Op]
		if id[0] == "" {
			id = [2]string{r.Op, r.Op}
		}
		done[id[0]] = true
		if r.OK {
			cl.add(id[0], id[1], CheckPass, "")
			continue
		}
		ok = false
		cl.add(id[0], id[1], CheckFail, r.Err)
	}
	// steps the probe did not reach after a failure
	for _, op := range []string{"connect + bucket exists", "put", "put if-absent (conditional write)", "get", "stat", "list", "delete"} {
		id := probeIDs[op]
		if !done[id[0]] {
			cl.add(id[0], id[1], CheckSkip, "Not run: an earlier step failed.")
		}
	}
	return ok
}

// probeS3ReadOnly checks reachability and read access without writing anything (read-only mode):
// the steps that write are reported as skipped.
func probeS3ReadOnly(b *datastore.S3Backend, cl *checkList) {
	if _, err := b.Stat("datastore.json"); err != nil {
		cl.add("connect", "Reach the endpoint and the bucket", CheckFail, err.Error())
	} else {
		cl.add("connect", "Reach the endpoint and the bucket", CheckPass, "datastore.json is reachable.")
	}
	if _, err := b.Get("datastore.json"); err != nil {
		cl.add("get", "Read it back", CheckFail, err.Error())
	} else {
		cl.add("get", "Read an object", CheckPass, "Read datastore.json.")
	}
	for _, op := range []string{"put", "put if-absent (conditional write)", "stat", "list", "delete"} {
		id := probeIDs[op]
		cl.add(id[0], id[1], CheckSkip, "Not run: the repository is read-only and this step writes a scratch object.")
	}
}

// inspectBackend reads datastore.json / owner.json (never writes them) and the bucket configuration.
func (s *Service) inspectBackend(ctx context.Context, b *datastore.S3Backend, rep *RepoCheckReport, cl *checkList, purpose string, adopt bool, self string) {
	meta, err := b.Get("datastore.json")
	switch {
	case err == nil:
		var m datastore.Meta
		if json.Unmarshal(meta, &m) == nil && m.Format == 1 {
			rep.Datastore.Exists, rep.Datastore.ID = true, m.ID
			c := m.Created
			rep.Datastore.Created = &c
		}
	case !errors.Is(err, datastore.ErrNotFound):
		cl.add("datastore", "Repository at this prefix", CheckFail, "Reading datastore.json failed: "+err.Error())
		return
	}
	if o, err := datastore.ReadOwner(b); err == nil {
		rep.Datastore.Owner = o
	}
	switch purpose {
	case "create":
		if rep.Datastore.Exists {
			cl.add("datastore", "Repository at this prefix", CheckFail, "The bucket already holds a repository at this prefix (id "+rep.Datastore.ID+"). Choose Attach to use it, or another prefix for a new one.")
		} else {
			cl.add("datastore", "Repository at this prefix", CheckPass, "No repository at this prefix yet: a new one will be created.")
		}
	case "attach":
		if !rep.Datastore.Exists {
			cl.add("datastore", "Repository at this prefix", CheckFail, "No repository (datastore.json) at this bucket and prefix. Check the prefix, or choose Create to start a new repository.")
		} else {
			cl.add("datastore", "Repository at this prefix", CheckPass, "Repository id "+rep.Datastore.ID+".")
			s.ownerChecks(ctx, rep, cl, adopt, self)
		}
	case "existing":
		if o := rep.Datastore.Owner; o != nil {
			s.ownerChecks(ctx, rep, cl, false, self)
		}
	}
	for _, f := range b.BucketConfig(ctx) {
		label := map[string]string{"versioning": "Bucket versioning", "lifecycle": "Lifecycle rules", "object-lock": "Object lock"}[f.ID]
		cl.add(f.ID, label, f.Level, f.Detail)
	}
}

// ownerChecks explains who owns the repository and whether this server already uses it.
func (s *Service) ownerChecks(ctx context.Context, rep *RepoCheckReport, cl *checkList, adopt bool, self string) {
	if rep.Datastore.ID != "" && self == "" {
		var name string
		err := s.DB.Pool().QueryRow(ctx, `SELECT name FROM repositories WHERE datastore_id=$1`, rep.Datastore.ID).Scan(&name)
		if err == nil {
			rep.Datastore.AttachedAs = name
			cl.add("attached", "Not used by another repository", CheckFail, fmt.Sprintf("This datastore is already attached on this server as repository %q.", name))
		}
	}
	o := rep.Datastore.Owner
	switch {
	case o == nil:
		cl.add("owner", "Owner", CheckPass, "No owner recorded: this server will claim it.")
	case o.ServerID == rep.ServerID:
		rep.Datastore.OwnedByThisServer = true
		cl.add("owner", "Owner", CheckPass, "Owned by this server.")
	case adopt:
		rep.NeedsAdopt = true
		cl.add("owner", "Owner", CheckWarn, fmt.Sprintf("Owned by another server (%s, host %q, since %s). Adopt takes it over: that server must never run GC or retention on it again.", o.ServerID, o.Host, o.Since.Format(time.RFC3339)))
	default:
		rep.NeedsAdopt = true
		cl.add("owner", "Owner", CheckFail, fmt.Sprintf("Owned by another server (%s, host %q, since %s). Two servers running GC and retention on one repository delete each other's data. Attach with adopt only if that server is gone for good.", o.ServerID, o.Host, o.Since.Format(time.RFC3339)))
	}
}

// RepoInfo is the identity of a repository for its detail page.
type RepoInfo struct {
	Name        string `json:"name"`
	Backend     string `json:"backend"`
	Location    string `json:"location"`
	DomainID    string `json:"domain_id"`
	DatastoreID string `json:"datastore_id"`
	Mode        string `json:"mode"`
	ModeReason  string `json:"mode_reason,omitempty"`
	// ServerID is this server's identity (the value owner.json must hold).
	ServerID string `json:"server_id"`
	// Owner is owner.json as read from the datastore just now (nil with OwnerError when unreadable).
	Owner      *datastore.Owner `json:"owner"`
	OwnerError string           `json:"owner_error,omitempty"`
	Created    *time.Time       `json:"datastore_created,omitempty"`
}

// RepositoryInfo reads a repository's identity, including owner.json from the datastore. The read
// bypasses the maintenance mode on purpose (it is a read of two small metadata objects).
func (s *Service) RepositoryInfo(ctx context.Context, name string) (*RepoInfo, error) {
	repo, err := s.Repository(ctx, name)
	if err != nil {
		return nil, err
	}
	sid, err := s.ServerID(ctx)
	if err != nil {
		return nil, err
	}
	info := &RepoInfo{Name: repo.Name, Backend: repo.Backend, Location: repo.Path, DomainID: repo.DomainID, DatastoreID: repo.DatastoreID,
		Mode: repo.Mode, ModeReason: repo.ModeReason, ServerID: sid}
	if repo.Mode == "offline" {
		info.OwnerError = "not read: the repository is offline"
		return info, nil
	}
	var b datastore.Backend
	if repo.Backend == BackendS3 {
		sb, err := s.S3Backend(ctx, repo)
		if err != nil {
			info.OwnerError = err.Error()
			return info, nil
		}
		b = sb
	} else {
		// read the files directly: NewFSBackend would create a missing directory
		if raw, err := os.ReadFile(filepath.Join(repo.Path, "owner.json")); err == nil {
			var o datastore.Owner
			if json.Unmarshal(raw, &o) == nil {
				info.Owner = &o
			}
		} else {
			info.OwnerError = err.Error()
		}
		if raw, err := os.ReadFile(filepath.Join(repo.Path, "datastore.json")); err == nil {
			var m datastore.Meta
			if json.Unmarshal(raw, &m) == nil {
				info.Created = &m.Created
			}
		}
		return info, nil
	}
	if o, err := datastore.ReadOwner(b); err != nil {
		info.OwnerError = err.Error()
	} else {
		info.Owner = o
	}
	if raw, err := b.Get("datastore.json"); err == nil {
		var m datastore.Meta
		if json.Unmarshal(raw, &m) == nil {
			info.Created = &m.Created
		}
	}
	return info, nil
}
