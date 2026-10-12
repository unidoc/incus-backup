// Package api holds the wire types of the incus-backup HTTP/JSON protocol.
// The protocol version is the /v1 path prefix; fields are only ever added.
package api

import "time"

// Scopes of an enrolled certificate.
const (
	ScopeBackup    = "backup"
	ScopeSyncRead  = "sync-read"
	ScopeSyncWrite = "sync-write"
)

// Header carrying the key-domain id (non-secret) on session creation.
const HeaderDomain = "X-Incus-Backup-Domain"

// HeaderSyncTransfer carries an opaque id of one synchronisation transfer (one job run): the receiving
// server keeps one chunk pin per transfer, so two concurrent transfers cannot unpin each other's chunks.
const HeaderSyncTransfer = "X-Incus-Backup-Sync-Transfer"

// Whoami is GET /v1/whoami.
type Whoami struct {
	Host       string    `json:"host"`
	Scope      string    `json:"scope"`
	Repository string    `json:"repository"`
	DomainID   string    `json:"domain_id"`
	ServerTime time.Time `json:"server_time"`
	Version    string    `json:"version"`
	// RepositoryMode is online, read-only or offline ("" from older servers = online). A host does not
	// read data for a backup a repository in maintenance would refuse.
	RepositoryMode string `json:"repository_mode,omitempty"`
}

// EnrollRequest is POST /v1/enroll (authenticated by the one-time token).
type EnrollRequest struct {
	Token string         `json:"token"`
	CSR   string         `json:"csr"` // PEM
	Info  map[string]any `json:"info,omitempty"`
}

// EnrollResponse carries the issued certificate.
type EnrollResponse struct {
	Certificate string `json:"certificate"` // PEM
	CA          string `json:"ca"`          // PEM
	Host        string `json:"host"`
	Scope       string `json:"scope"`
	Repository  string `json:"repository"`
}

// Policy is a backup policy as stored on a server.
type Policy struct {
	Name           string   `json:"name"`
	Schedule       string   `json:"schedule"`
	Targets        []string `json:"targets"`
	CommitRequired string   `json:"commit_required"`
	KeepLast       int      `json:"keep_last"`
	KeepHourly     int      `json:"keep_hourly"`
	KeepDaily      int      `json:"keep_daily"`
	KeepWeekly     int      `json:"keep_weekly"`
	KeepMonthly    int      `json:"keep_monthly"`
	KeepYearly     int      `json:"keep_yearly"`
	// Timezone is the IANA zone in which retention periods are cut ("" = UTC).
	Timezone            string `json:"timezone,omitempty"`
	ReverifyDays        int    `json:"reverify_days,omitempty"`
	VerifySchedule      string `json:"verify_schedule"`
	RestoreTestSchedule string `json:"restore_test_schedule"`
}

// Assignment maps a scope on a host to a policy. Empty Project means the whole
// host; empty Object means the whole project.
type Assignment struct {
	Project string `json:"project"`
	Object  string `json:"object"`
	Policy  string `json:"policy"`
}

// DesiredState is GET /v1/desired-state: what this server wants from the host.
type DesiredState struct {
	Policies    []Policy     `json:"policies"`
	Assignments []Assignment `json:"assignments"`
}

// ObjectInfo is an inventory entry reported by a client.
type ObjectInfo struct {
	Project  string   `json:"project"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	GroupID  string   `json:"group_id"`
	SharedBy []string `json:"shared_by,omitempty"`
	Policy   string   `json:"policy,omitempty"`
}

// Inventory is PUT /v1/inventory.
type Inventory struct {
	Objects []ObjectInfo `json:"objects"`
}

// SnapshotRef is one committed snapshot of a group.
type SnapshotRef struct {
	GroupID     string    `json:"group_id"`
	SnapshotID  string    `json:"snapshot_id"`
	Created     time.Time `json:"created"`
	Committed   time.Time `json:"committed"`
	IndexDigest string    `json:"index_digest"`
	ChunkCount  int       `json:"chunk_count"`
	Policy      string    `json:"policy,omitempty"`
	// Control-plane metadata (operational names), present for sync.
	Project string `json:"project,omitempty"`
	Object  string `json:"object,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Host    string `json:"host,omitempty"`
	Logical int64  `json:"logical_bytes,omitempty"`
	// Repository metadata. Protected snapshots are never pruned; VerifyState is "" (never
	// verified), "ok" or "failed": a failed snapshot is never used as an incremental base and
	// is not synced when the job is verified-only.
	Protected   bool   `json:"protected,omitempty"`
	VerifyState string `json:"verify_state,omitempty"`
	Notes       string `json:"notes,omitempty"`
}

// SnapshotList is a list of snapshots of one group.
type SnapshotList struct {
	Snapshots []SnapshotRef `json:"snapshots"`
}

// SessionRequest is POST /v1/sessions.
type SessionRequest struct {
	GroupID    string    `json:"group_id"`
	SnapshotID string    `json:"snapshot_id"`
	Created    time.Time `json:"created"`
	Policy     string    `json:"policy"`
	Project    string    `json:"project"`
	Object     string    `json:"object"`
	Kind       string    `json:"kind"`
	PreviousID string    `json:"previous_id,omitempty"`
}

// SessionResponse identifies the session.
type SessionResponse struct {
	Session string    `json:"session"`
	Expires time.Time `json:"expires"`
	// Lease is the inactivity lease: any request on the session (or POST .../heartbeat)
	// renews it. A client that is busy without talking to the server must heartbeat at
	// least every Lease/3.
	Lease time.Duration `json:"lease"`
}

// MissingRequest asks which chunk ids the server lacks.
type MissingRequest struct {
	IDs []string `json:"ids"`
}

// MissingResponse lists the ids the server lacks.
type MissingResponse struct {
	Missing []string `json:"missing"`
}

// CommitRequest is POST /v1/sessions/{id}/commit.
type CommitRequest struct {
	LogicalBytes  int64 `json:"logical_bytes"`
	DirtyBytes    int64 `json:"dirty_bytes"`
	ReadBytes     int64 `json:"read_bytes"`
	UploadedBytes int64 `json:"uploaded_bytes"`
	FullRead      bool  `json:"full_read"`
	DurationMs    int64 `json:"duration_ms"`
}

// CommitResponse is the server's receipt.
type CommitResponse struct {
	SnapshotID string    `json:"snapshot_id"`
	Committed  time.Time `json:"committed"`
	ChunkCount int       `json:"chunk_count"`
}

// RunStart is POST /v1/runs.
type RunStart struct {
	Started      time.Time `json:"started"`
	ObjectsTotal int       `json:"objects_total"`
}

// RunStartResponse returns the run id.
type RunStartResponse struct {
	Run string `json:"run"`
}

// RunFinish is POST /v1/runs/{id}/finish.
type RunFinish struct {
	Status        string `json:"status"` // ok, partial, failed, deferred (refused by repository maintenance only)
	ObjectsOK     int    `json:"objects_ok"`
	ObjectsFailed int    `json:"objects_failed"`
	// ObjectsDeferred were refused because the repository was in maintenance (503); not failures.
	ObjectsDeferred int    `json:"objects_deferred,omitempty"`
	LogicalBytes    int64  `json:"logical_bytes"`
	UploadedBytes   int64  `json:"uploaded_bytes"`
	Error           string `json:"error,omitempty"`
}

// SyncSnapshotPut is the body of PUT /v1/sync/snapshots/{group}/{snapshot}.
type SyncSnapshotPut struct {
	Meta     SnapshotRef `json:"meta"`
	Index    []byte      `json:"index"`    // base64 in JSON
	Manifest []byte      `json:"manifest"` // base64 in JSON
}

// Error is the JSON error body.
type Error struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// RepoObject is a storage object known to a repository (control-plane names).
type RepoObject struct {
	GroupID   string       `json:"group_id"`
	Project   string       `json:"project"`
	Name      string       `json:"name"`
	Kind      string       `json:"kind"`
	Host      string       `json:"host"`
	Policy    string       `json:"policy"`
	SharedBy  []string     `json:"shared_by,omitempty"`
	Snapshots int          `json:"snapshots"`
	Latest    *SnapshotRef `json:"latest,omitempty"`
}

// ObjectList is GET /v1/objects.
type ObjectList struct {
	Objects []RepoObject `json:"objects"`
}
