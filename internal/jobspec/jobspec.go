// Package jobspec is the contract of host jobs, shared by the server (which validates what an
// operator asks for) and the host executor (which validates again before doing anything: the server
// is not trusted to have done it). It holds the job types, their parameters with validation, the
// per-type limits and the wire types of the host protocol (/v1/jobs/...).
//
// The server never holds a key. A job is a REQUEST to a host; the host decides whether it accepts
// that type at all (client.yaml `jobs.allow`), runs it with its own key and reports back. Results,
// messages and log lines never carry file names from inside volumes; the only in-volume names the
// server ever sees are the paths an operator explicitly typed into a guest_files/fs_extract request.
package jobspec

import (
	"bytes"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/unidoc/incus-backup/internal/archive"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/fscat"
	"github.com/unidoc/incus-backup/internal/recovery"
)

// Job types.
const (
	TypeBackupNow       = "backup_now"
	TypeRestoreTest     = "restore_test"
	TypeRestoreInstance = "restore_instance"
	TypeRestoreVolume   = "restore_volume"
	TypeGuestFiles      = "guest_files"
	TypeFSExtract       = "fs_extract"
	TypeDeepVerify      = "deep_verify"
)

// Types lists every job type.
var Types = []string{TypeBackupNow, TypeRestoreTest, TypeRestoreInstance, TypeRestoreVolume, TypeGuestFiles, TypeFSExtract, TypeDeepVerify}

// Job states.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
	StateExpired   = "expired"
)

// Terminal reports whether a state is final.
func Terminal(state string) bool {
	switch state {
	case StateSucceeded, StateFailed, StateCancelled, StateExpired:
		return true
	}
	return false
}

// Restore-test methods.
const (
	ModeDeep    = "deep"    // download, decrypt and recompute the id of every chunk of the snapshot
	ModeExtract = "extract" // filesystem snapshot: extract into a scratch directory and check every file
	ModeFull    = "full"    // restore into a scratch Incus volume and compare its content
)

// Limits.
const (
	MaxDeliveryBytes = 4 << 30 // a sealed delivery
	// MaxEntriesCeiling is the hard ceiling of guest_files' max_entries (the default is archive.DefaultMaxEntries).
	MaxEntriesCeiling = archive.MaxEntriesCeiling
	DeliveryTTL       = time.Hour
	MaxPaths          = 256
	MaxPathLen        = 4096
	MaxNameLen        = 256
	MaxLogLines       = 1000 // per job, server side
	MaxLogLineLen     = 1000
	MaxMessageLen     = 2000
	MaxLinesPerCall   = 200
	Lease             = 5 * time.Minute // a running job whose host stays silent this long has failed
	QueueTTL          = 24 * time.Hour  // a queued job nobody picked up by then expires
	MaxWait           = 60 * time.Second
)

// Info is what the system knows about a job type.
type Info struct {
	// Permission is the authorisation the management plane must check before creating it.
	Permission string
	// Timeout bounds one attempt on the host.
	Timeout time.Duration
	// MaxAttempts: a job whose host stopped reporting is re-queued until it had this many attempts.
	MaxAttempts int
	// Heavy jobs run one at a time on a host (and never next to a scheduled backup).
	Heavy bool
	// DefaultAllowed: a host accepts the type unless its configuration says otherwise. Types that
	// restore objects into Incus or hand out file contents need the operator to allow them on the host.
	DefaultAllowed bool
	// Delivery: the job uploads a sealed file.
	Delivery bool
}

var infos = map[string]Info{
	TypeBackupNow:       {Permission: "jobs.backup", Timeout: 24 * time.Hour, MaxAttempts: 2, Heavy: true, DefaultAllowed: true},
	TypeRestoreTest:     {Permission: "jobs.restore_test", Timeout: 12 * time.Hour, MaxAttempts: 2, Heavy: true, DefaultAllowed: true},
	TypeDeepVerify:      {Permission: "jobs.verify", Timeout: 24 * time.Hour, MaxAttempts: 2, Heavy: true, DefaultAllowed: true},
	TypeRestoreInstance: {Permission: "jobs.restore", Timeout: 24 * time.Hour, MaxAttempts: 1, Heavy: true},
	TypeRestoreVolume:   {Permission: "jobs.restore", Timeout: 24 * time.Hour, MaxAttempts: 1, Heavy: true},
	TypeGuestFiles:      {Permission: "jobs.files", Timeout: 2 * time.Hour, MaxAttempts: 2, Heavy: true, Delivery: true},
	TypeFSExtract:       {Permission: "jobs.files", Timeout: 2 * time.Hour, MaxAttempts: 2, Delivery: true},
}

// Lookup returns the description of a type.
func Lookup(typ string) (Info, bool) {
	i, ok := infos[typ]
	return i, ok
}

// RequiredPermission is the authorisation hint for the management plane ("" for an unknown type).
func RequiredPermission(typ string) string { return infos[typ].Permission }

// ---- parameters -------------------------------------------------------------------------------

// BackupNow backs up a selection now: one object (owner name like "vm1" or logical name like
// "volume/biggi"), every object of a policy, or every object of a project. Fields combine (AND).
type BackupNow struct {
	Project string `json:"project,omitempty"`
	Object  string `json:"object,omitempty"`
	Policy  string `json:"policy,omitempty"`
}

// RestoreTest tests one storage object. Object is the logical name ("instance/vm1/root",
// "volume/biggi", "instance/c1/root"); Snapshot "" means the latest.
type RestoreTest struct {
	Project  string `json:"project"`
	Object   string `json:"object"`
	Snapshot string `json:"snapshot,omitempty"`
	Mode     string `json:"mode"`
}

// RestoreInstance recreates an instance (never over an existing one).
type RestoreInstance struct {
	Project     string `json:"project"`
	Instance    string `json:"instance"`
	Snapshot    string `json:"snapshot,omitempty"`
	AsProject   string `json:"as_project,omitempty"`
	AsName      string `json:"as_name,omitempty"`
	Volumes     bool   `json:"volumes,omitempty"`
	Start       bool   `json:"start,omitempty"`
	KeepNetwork bool   `json:"keep_network,omitempty"`
}

// RestoreVolume restores one storage object into a NEW custom volume.
type RestoreVolume struct {
	Project   string `json:"project"`
	Object    string `json:"object"`
	Snapshot  string `json:"snapshot,omitempty"`
	Pool      string `json:"pool"`
	AsProject string `json:"as_project,omitempty"`
	AsName    string `json:"as_name"`
}

// GuestFiles recovers paths from a VM disk backup through a disposable VM and delivers them as a
// tar sealed to RecipientPublicKey (X25519, 32 bytes, standard base64).
type GuestFiles struct {
	Project   string   `json:"project"`
	Object    string   `json:"object"`
	Snapshot  string   `json:"snapshot,omitempty"`
	Paths     []string `json:"paths"`
	Partition int      `json:"partition,omitempty"`
	// MaxEntries raises the cap on the number of files/directories recovered (0 = archive.DefaultMaxEntries,
	// at most MaxEntriesCeiling).
	MaxEntries         int    `json:"max_entries,omitempty"`
	RecipientPublicKey string `json:"recipient_public_key"`
}

// FSExtract extracts paths of a filesystem (catalog) snapshot and delivers them sealed.
type FSExtract struct {
	Project            string   `json:"project"`
	Object             string   `json:"object"`
	Snapshot           string   `json:"snapshot,omitempty"`
	Paths              []string `json:"paths"`
	RecipientPublicKey string   `json:"recipient_public_key"`
}

// DeepVerify decrypts and checks the last N snapshots of every object (or of one object).
type DeepVerify struct {
	Last    int    `json:"last"`
	Project string `json:"project,omitempty"`
	Object  string `json:"object,omitempty"`
}

// ErrInvalid wraps every validation failure.
var ErrInvalid = errors.New("invalid job parameters")

func invalid(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(f, a...)) }

func strictDecode(raw []byte, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return invalid("%v", err)
	}
	if d.More() {
		return invalid("trailing data")
	}
	return nil
}

// name checks an Incus-level name (project, instance, volume, pool, policy): non-empty when
// required, bounded, printable, no slash.
func name(field, v string, required bool) error {
	if v == "" {
		if required {
			return invalid("%s is required", field)
		}
		return nil
	}
	if len(v) > MaxNameLen {
		return invalid("%s is too long", field)
	}
	for _, r := range v {
		if r == '/' || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return invalid("%s %q contains an invalid character", field, v)
		}
	}
	return nil
}

// object checks a logical storage-object name: "instance/<i>/root", "instance/<i>/<dev>",
// "instance/<i>" or "volume/<v>".
func object(field, v string, required bool) error {
	if v == "" {
		if required {
			return invalid("%s is required", field)
		}
		return nil
	}
	if len(v) > 3*MaxNameLen {
		return invalid("%s is too long", field)
	}
	parts := strings.Split(v, "/")
	switch {
	case parts[0] == "instance" && (len(parts) == 2 || len(parts) == 3):
	case parts[0] == "volume" && len(parts) == 2:
	default:
		return invalid("%s %q is not a storage object name (instance/<name>[/<device>] or volume/<name>)", field, v)
	}
	for _, p := range parts[1:] {
		if err := name(field, p, true); err != nil {
			return err
		}
		if p == "." || p == ".." {
			return invalid("%s %q is invalid", field, v)
		}
	}
	return nil
}

func snapshot(v string) error {
	if v != "" && !format.ValidSnapshotID(v) {
		return invalid("snapshot %q is not a snapshot id", v)
	}
	return nil
}

// RecipientKey decodes and checks an X25519 public key.
func RecipientKey(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, invalid("recipient_public_key is required")
	}
	k, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		if k, err = base64.RawURLEncoding.DecodeString(b64); err != nil {
			return nil, invalid("recipient_public_key is not base64")
		}
	}
	if len(k) != 32 {
		return nil, invalid("recipient_public_key must be 32 bytes (X25519), got %d", len(k))
	}
	if bytes.Equal(k, make([]byte, 32)) {
		return nil, invalid("recipient_public_key is all zero")
	}
	if _, err := ecdh.X25519().NewPublicKey(k); err != nil {
		return nil, invalid("recipient_public_key: %v", err)
	}
	return k, nil
}

func paths(in []string, clean func([]string) ([]string, error)) ([]string, error) {
	if len(in) == 0 {
		return nil, invalid("paths are required")
	}
	if len(in) > MaxPaths {
		return nil, invalid("at most %d paths per job", MaxPaths)
	}
	for _, p := range in {
		if len(p) > MaxPathLen {
			return nil, invalid("a path is longer than %d bytes", MaxPathLen)
		}
	}
	out, err := clean(in)
	if err != nil {
		return nil, invalid("%v", err)
	}
	return out, nil
}

func fsPaths(in []string) ([]string, error) {
	var out []string
	for _, p := range in {
		if p == "" || strings.ContainsAny(p, "\x00\n\r") {
			return nil, fmt.Errorf("invalid path %q", p)
		}
		c, err := fscat.CleanPath(p)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Validate decodes params of a job type strictly (unknown fields are refused), checks them and
// returns the parsed value plus its canonical JSON (paths cleaned, defaults applied). Unknown types
// are refused.
func Validate(typ string, raw []byte) (any, []byte, error) {
	var v any
	var err error
	switch typ {
	case TypeBackupNow:
		var p BackupNow
		if err = strictDecode(raw, &p); err == nil {
			err = errors.Join(name("project", p.Project, false), name("policy", p.Policy, false))
			if err == nil && p.Object != "" && strings.Contains(p.Object, "/") {
				err = object("object", p.Object, true)
			} else if err == nil {
				err = name("object", p.Object, false)
			}
			if err == nil && p.Project == "" && p.Object == "" && p.Policy == "" {
				err = invalid("backup_now needs an object, a policy or a project")
			}
		}
		v = p
	case TypeRestoreTest:
		var p RestoreTest
		if err = strictDecode(raw, &p); err == nil {
			err = errors.Join(name("project", p.Project, true), object("object", p.Object, true), snapshot(p.Snapshot))
			if err == nil {
				switch p.Mode {
				case ModeDeep, ModeExtract, ModeFull:
				case "":
					p.Mode = ModeDeep
				default:
					err = invalid("mode must be deep, extract or full")
				}
			}
		}
		v = p
	case TypeRestoreInstance:
		var p RestoreInstance
		if err = strictDecode(raw, &p); err == nil {
			err = errors.Join(name("project", p.Project, true), name("instance", p.Instance, true), snapshot(p.Snapshot),
				name("as_project", p.AsProject, false), name("as_name", p.AsName, false))
		}
		v = p
	case TypeRestoreVolume:
		var p RestoreVolume
		if err = strictDecode(raw, &p); err == nil {
			err = errors.Join(name("project", p.Project, true), object("object", p.Object, true), snapshot(p.Snapshot),
				name("pool", p.Pool, true), name("as_project", p.AsProject, false), name("as_name", p.AsName, true))
		}
		v = p
	case TypeGuestFiles:
		var p GuestFiles
		if err = strictDecode(raw, &p); err == nil {
			err = errors.Join(name("project", p.Project, true), object("object", p.Object, true), snapshot(p.Snapshot))
			if err == nil && (p.Partition < 0 || p.Partition > 128) {
				err = invalid("partition must be 0..128")
			}
			if err == nil && (p.MaxEntries < 0 || p.MaxEntries > MaxEntriesCeiling) {
				err = invalid("max_entries must be 0 (default %d) .. %d", archive.DefaultMaxEntries, MaxEntriesCeiling)
			}
			if err == nil {
				p.Paths, err = paths(p.Paths, recovery.CleanPaths)
			}
			if err == nil {
				_, err = RecipientKey(p.RecipientPublicKey)
			}
		}
		v = p
	case TypeFSExtract:
		var p FSExtract
		if err = strictDecode(raw, &p); err == nil {
			err = errors.Join(name("project", p.Project, true), object("object", p.Object, true), snapshot(p.Snapshot))
			if err == nil {
				p.Paths, err = paths(p.Paths, fsPaths)
			}
			if err == nil {
				_, err = RecipientKey(p.RecipientPublicKey)
			}
		}
		v = p
	case TypeDeepVerify:
		var p DeepVerify
		if err = strictDecode(raw, &p); err == nil {
			if p.Last == 0 {
				p.Last = 1
			}
			if p.Last < 1 || p.Last > 1000 {
				err = invalid("last must be 1..1000")
			}
			if err == nil {
				err = errors.Join(name("project", p.Project, p.Object != ""), object("object", p.Object, false))
			}
		}
		v = p
	default:
		return nil, nil, invalid("unknown job type %q", typ)
	}
	if err != nil {
		return nil, nil, err
	}
	canon, mErr := json.Marshal(v)
	if mErr != nil {
		return nil, nil, mErr
	}
	return v, canon, nil
}

// ---- results ----------------------------------------------------------------------------------

// RestoreTestResult is the result of a restore_test job (and what a scheduled test reports). It
// never contains a file name from inside the volume: problems are counts, chunk ids and kinds.
type RestoreTestResult struct {
	Project    string         `json:"project"`
	Object     string         `json:"object"`
	Snapshot   string         `json:"snapshot"`
	Method     string         `json:"method"`
	Passed     bool           `json:"passed"`
	Bytes      int64          `json:"bytes"`
	Files      int64          `json:"files"`
	Chunks     int            `json:"chunks"`
	DurationMs int64          `json:"duration_ms"`
	Details    map[string]any `json:"details,omitempty"`
}

// ObjectKey is "<project>/<object>".
func (r RestoreTestResult) ObjectKey() string { return r.Project + "/" + r.Object }

// DeliveryResult is the result of a guest_files / fs_extract job.
type DeliveryResult struct {
	Size   int64  `json:"size"`   // bytes of the sealed stream
	SHA256 string `json:"sha256"` // of the sealed stream
	Files  int    `json:"files"`  // regular files in the tar
	Bytes  int64  `json:"bytes"`  // plaintext bytes of those files
	Format string `json:"format"` // "tar+IBS2" (format.NewSealWriter)
	// Paths: guest_files only, the status the recovery VM reported per REQUESTED path ("ok",
	// "missing", ...). Only the paths the operator asked for appear; nothing else from the guest.
	Paths map[string]string `json:"paths,omitempty"`
}

// ---- host protocol ------------------------------------------------------------------------------

// Job is what a host receives from GET /v1/jobs/next.
type Job struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Params    json.RawMessage `json:"params"`
	Attempt   int             `json:"attempt"`
	CreatedBy string          `json:"created_by"`
	Created   time.Time       `json:"created"`
	// LeaseSeconds: report progress (or nothing new) at least this often/3.
	LeaseSeconds   int `json:"lease_seconds"`
	TimeoutSeconds int `json:"timeout_seconds"`
}

// LogLine is one line of job log.
type LogLine struct {
	Level string `json:"level"` // debug|info|warn|error
	Line  string `json:"line"`
}

// Progress is POST /v1/jobs/{id}/progress (it also renews the lease).
type Progress struct {
	Attempt    int       `json:"attempt"`
	Progress   int       `json:"progress"` // 0..100
	BytesDone  int64     `json:"bytes_done,omitempty"`
	BytesTotal int64     `json:"bytes_total,omitempty"`
	Message    string    `json:"message,omitempty"`
	Log        []LogLine `json:"log,omitempty"`
}

// ProgressResponse tells the host whether to stop.
type ProgressResponse struct {
	CancelRequested bool      `json:"cancel_requested"`
	LeaseUntil      time.Time `json:"lease_until"`
}

// Finish is POST /v1/jobs/{id}/finish.
type Finish struct {
	Attempt int             `json:"attempt"`
	State   string          `json:"state"` // succeeded|failed|cancelled
	Message string          `json:"message,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Log     []LogLine       `json:"log,omitempty"`
}

// CancelState is GET /v1/jobs/{id}/cancel-requested.
type CancelState struct {
	CancelRequested bool   `json:"cancel_requested"`
	State           string `json:"state"`
}

// RestoreTestReport is POST /v1/restore-tests: a restore test the host ran on its own schedule.
type RestoreTestReport struct {
	Result   RestoreTestResult `json:"result"`
	Started  time.Time         `json:"started"`
	Finished time.Time         `json:"finished"`
}

// HeaderAttempt carries the attempt number on a delivery upload.
const HeaderAttempt = "X-Incus-Backup-Attempt"
