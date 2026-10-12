/*
 * Wire types of the management API (/api/v1).
 *
 * Source of truth: the Go types the handlers in internal/server/mgmt/*.go encode and decode, and
 * docs/api.md. Field names are the exact JSON names. Request bodies are decoded with
 * DisallowUnknownFields, so an unknown or misspelt key gives a 400.
 *
 * Conventions used here:
 *   - `?:` means the Go field has omitempty/omitzero and can be missing.
 *   - `| null` means a Go pointer, nil slice or nil map without omitempty, encoded as null.
 *   - A list endpoint whose Go code starts from `var out []T` returns `null` (not `[]`) when the list
 *     is empty. Those response types are `T[] | null`.
 *   - Times are RFC 3339 strings. time.Duration values are integers in nanoseconds.
 *
 * Routes -> request / response types (all paths are under /api/v1; "-" = no body;
 * "204" = No Content; R = route takes ?repository=NAME, optional when exactly one repository exists).
 *
 *   Session and people
 *   GET    /ping                                  -                      -> PingResponse
 *   GET    /events                                -                      -> text/event-stream of ServerEvent
 *   GET    /setup                                 -                      -> SetupStatus
 *   POST   /setup                                 SetupRequest           -> 201 User
 *   POST   /session                               LoginRequest           -> LoginResponse | 401/429 LoginError
 *   GET    /session                               -                      -> SessionResponse
 *   DELETE /session                               -                      -> 204
 *   GET    /users                                 -                      -> User[]
 *   POST   /users                                 CreateUserRequest      -> 201 User
 *   PUT    /users/{name}                          UpdateUserRequest      -> 204
 *   DELETE /users/{name}                          -                      -> 204
 *   POST   /users/{name}/password                 ResetPasswordRequest   -> 204
 *   DELETE /users/{name}/totp                     -                      -> 204
 *   POST   /me/password                           ChangePasswordRequest  -> 204
 *   POST   /me/totp                               -                      -> TOTPEnrolment
 *   POST   /me/totp/confirm                       ConfirmTOTPRequest     -> ConfirmTOTPResponse
 *   DELETE /me/totp                               DisableTOTPRequest     -> 204
 *   GET    /me/sessions                           -                      -> SessionInfo[]
 *   DELETE /me/sessions/{id}                      -                      -> 204
 *   GET    /me/tokens                             -                      -> TokenInfo[]
 *   POST   /me/tokens                             CreateTokenRequest     -> 201 CreateTokenResponse
 *   DELETE /me/tokens/{id}                        -                      -> 204
 *   GET    /audit?actor=&action=&before=&limit=   -                      -> AuditEntry[]
 *
 *   Dashboard and repositories
 *   GET    /overview                              -                      -> Overview
 *   GET    /status                     R          -                      -> Status
 *   GET    /capacity                   R          -                      -> CapacityView
 *   GET    /capacity/series?days=      R          -                      -> CapacityPoint[]
 *   GET    /repositories                          -                      -> RepoView[]
 *   POST   /repositories                          CreateRepoRequest      -> 201 RepoView
 *   POST   /repositories/attach                   CreateRepoRequest      -> 201 RepoView
 *   POST   /repositories/{name}/probe             -                      -> ProbeResult[]
 *   PUT    /repositories/{name}/mode              SetModeRequest         -> 204 (+ SSE 'repository')
 *   GET    /credentials                           -                      -> Credential[] | null
 *   POST   /credentials                           CredentialRequest      -> 201 Credential
 *   PUT    /credentials/{name}                    CredentialRequest      -> 204
 *   DELETE /credentials/{name}                    -                      -> 204
 *
 *   Hosts, onboarding, policies
 *   GET    /hosts                      R          -                      -> HostRow[] | null
 *   POST   /hosts/{name}/{scope}/{enable|disable} R -                    -> 204
 *   POST   /onboarding                 R          OnboardingRequest      -> 201 OnboardingResult
 *   POST   /tokens                     R          EnrolmentTokenRequest  -> 201 EnrolmentTokenResponse
 *   GET    /policies                   R          -                      -> Policy[] | null
 *   PUT    /policies/{name}            R          Policy                 -> 204
 *   DELETE /policies/{name}            R          -                      -> 204
 *   GET    /assignments                R          -                      -> AssignmentRow[] | null
 *   PUT    /assignments                R          AssignmentRow          -> 204
 *   DELETE /assignments                R          AssignmentRow          -> 204 (policy is ignored)
 *
 *   Objects and snapshots
 *   GET    /objects                    R          -                      -> ObjectRow[] | null
 *   PUT    /objects/{group}/retention  R          SetRetentionRequest    -> 204
 *   PUT    /objects/{group}/notes      R          SetNotesRequest        -> 204
 *   POST   /objects/{group}/transfer   R          TransferRequest        -> 204
 *   GET    /snapshots?group=           R          -                      -> SnapshotRef[] | null
 *   PUT    /snapshots/{group}/{snap}/protected R  SetProtectedRequest    -> 204
 *   PUT    /snapshots/{group}/{snap}/notes R      SetNotesRequest        -> 204
 *   DELETE /snapshots/{group}/{snap}   R          -                      -> 204 (409 when protected)
 *   GET    /snapshots/{group}/{snap}/verifications R -                   -> SnapshotVerification[] | null
 *   GET    /snapshots/{group}/{snap}/index    R   -                      -> application/octet-stream (encrypted)
 *   GET    /snapshots/{group}/{snap}/manifest R   -                      -> application/octet-stream (encrypted)
 *   GET    /chunks/{id}                R          -                      -> application/octet-stream (encrypted)
 *
 *   Maintenance and verification
 *   GET    /maintenance                R          -                      -> MaintenanceView
 *   PUT    /maintenance                R          MaintenanceUpdate      -> 204
 *   GET    /runs?limit=                R          -                      -> RunRow[] | null
 *   POST   /prune[?dry_run=1]          R          -                      -> PruneResult
 *   POST   /gc[?dry_run=1&grace_hours=] R         -                      -> GCStats
 *   POST   /verify                     R          VerifyRequest          -> VerifyResponse
 *   POST   /reconcile                  R          -                      -> ReconcileResult
 *   GET    /corrupt[?healed=1]         R          -                      -> CorruptChunk[] | null
 *   DELETE /escrow                     R          -                      -> 204
 *
 *   Sync (only registered when the server has a sync manager)
 *   GET    /sync                       R          -                      -> SyncJob[] | null
 *   POST   /sync                       R          SyncCreateRequest      -> 204
 *   GET    /sync/{name}                R          -                      -> SyncStatus
 *   PUT    /sync/{name}                R          SyncJobOptions         -> 204
 *   DELETE /sync/{name}                R          -                      -> 204
 *   POST   /sync/{name}/run            R          -                      -> SyncRunResponse
 *
 *   Settings
 *   GET    /settings                              -                      -> SettingsMap
 *   PUT    /settings                              SettingsMap            -> 204
 *
 *   Any error                                                            -> ApiErrorBody
 *
 *   Planned routes (see the DRAFT section at the end): /notifications/*, /alerts*, /silences*,
 *   /jobs*, /restore-tests*, /certificates*.
 */

/** RFC 3339 timestamp (Go time.Time). */
export type RFC3339 = string;

/** Go time.Duration as JSON: an integer number of nanoseconds. */
export type Nanoseconds = number;

// ---- common ----------------------------------------------------------------------------------

/** api.Error (internal/api/api.go): the body of every error response. */
export interface ApiErrorBody {
  error: string;
  /** Machine-readable code (agent API; the management API does not set it). */
  code?: string;
}

/** auth.Role (internal/server/auth/auth.go), least to most privileged. */
export type Role = 'readonly' | 'restore' | 'operator' | 'admin';

/** auth.Principal.Kind (internal/server/auth/auth.go). */
export type PrincipalKind = 'user' | 'token' | 'local';

/** Repository maintenance mode: datastore.Mode.String() (internal/datastore/mode.go). */
export type RepoMode = 'online' | 'read-only' | 'offline';

/** Anonymous map in GET /ping (internal/server/mgmt/routes_auth.go). */
export interface PingResponse {
  ok: true;
  /** service.Service.Version. */
  version: string;
}

// ---- session and people ----------------------------------------------------------------------

/** Anonymous map in GET /setup (internal/server/mgmt/routes_auth.go). */
export interface SetupStatus {
  /** True while no user exists. */
  needed: boolean;
}

/** Anonymous struct{ Token, Name, Password string } in POST /setup (untagged: keys match case-insensitively). */
export interface SetupRequest {
  /** One-time setup token printed at start or by `user setup-token`. */
  token: string;
  name: string;
  password: string;
}

/** auth.User (internal/server/auth/auth.go): a user without credential material. */
export interface User {
  id: string;
  name: string;
  display_name: string;
  email: string;
  role: Role;
  totp_enabled: boolean;
  disabled: boolean;
  must_change_password: boolean;
  created_at: RFC3339;
  last_login_at?: RFC3339;
  /** Set while the account is locked after failed logins. */
  locked_until?: RFC3339;
  /** false: the user signs in through single sign-on only (no local password). */
  has_password?: boolean;
  /** Registered passkeys / security keys. */
  webauthn_keys?: number;
  /** Identity providers the user is linked to. */
  identities?: string[] | null;
}

/** Anonymous struct{ Name, Password, Code string } in POST /session (untagged: keys match case-insensitively). */
export interface LoginRequest {
  name: string;
  password: string;
  /** TOTP code or recovery code; needed when the user has 2FA. */
  code?: string;
}

/** mgmt.sessionView (internal/server/mgmt/routes_auth.go). Also the user form of GET /session. */
export interface LoginResponse {
  user: User;
  /** Send as the X-CSRF-Token header on state-changing browser requests. */
  csrf: string;
  /** omitzero: set by POST /session, missing from GET /session. */
  expires_at?: RFC3339;
}

/** Anonymous map in GET /session for a non-user principal (API token or the local socket). */
export interface PrincipalSessionResponse {
  /** Token name, or "local-admin" for the socket. */
  user: { name: string; role: Role };
  kind: Exclude<PrincipalKind, 'user'>;
}

/** GET /session: a signed-in user gets a sessionView, a token or the local socket gets the principal form. */
export type SessionResponse = LoginResponse | PrincipalSessionResponse;

/**
 * Error body of POST /session (mgmt.authFail). `totp_required: true` (with status 401) means the
 * password was right and a code is needed. A 429 means the account is locked or the address is throttled.
 */
export interface LoginError {
  error: string;
  totp_required?: true;
}

/**
 * Anonymous struct in POST /users. Name, Password, Display, Email and Role are untagged (keys match
 * case-insensitively); note `display` here but `display_name` in UpdateUserRequest and User.
 */
export interface CreateUserRequest {
  name: string;
  password: string;
  display?: string;
  email?: string;
  role: Role;
  /** Note: `must_change` here, `must_change_password` in User. */
  must_change?: boolean;
}

/** Anonymous struct of pointers in PUT /users/{name}: a missing field is left unchanged. */
export interface UpdateUserRequest {
  role?: Role;
  display_name?: string;
  email?: string;
  disabled?: boolean;
}

/** Anonymous struct in POST /users/{name}/password (admin reset; ends the user's sessions). */
export interface ResetPasswordRequest {
  password: string;
  must_change?: boolean;
}

/** Anonymous struct{ Current, New string } in POST /me/password (untagged). */
export interface ChangePasswordRequest {
  current: string;
  new: string;
}

/** auth.TOTPEnrolment (internal/server/auth/totp.go). */
export interface TOTPEnrolment {
  /** Base32 seed. */
  secret: string;
  /** otpauth:// URI; render as a QR code. */
  uri: string;
}

/** Anonymous struct{ Code string } in POST /me/totp/confirm (untagged). */
export interface ConfirmTOTPRequest {
  code: string;
  /** Needed when the sign-in is older than 10 minutes (the answer is 400 password_required). */
  password?: string;
}

/** Anonymous map in POST /me/totp/confirm. */
export interface ConfirmTOTPResponse {
  /** Ten recovery codes, shown only now. */
  recovery_codes: string[];
  note: string;
}

/** Anonymous struct{ Password string } in DELETE /me/totp (untagged). */
export interface DisableTOTPRequest {
  password: string;
}

/** auth.SessionInfo (internal/server/auth/auth.go). */
export interface SessionInfo {
  /** Prefix of the stored token hash; enough to revoke the session. */
  id: string;
  created_at: RFC3339;
  last_seen: RFC3339;
  expires_at: RFC3339;
  ip: string;
  user_agent: string;
  /** True for the session making the request. */
  current: boolean;
}

/** auth.TokenInfo (internal/server/auth/auth.go): an API token, never the secret. */
export interface TokenInfo {
  id: string;
  name: string;
  role: Role;
  created_at: RFC3339;
  /** Missing: never expires (only tokens created before every token got a lifetime). */
  expires_at?: RFC3339;
  last_used_at?: RFC3339;
}

/** Anonymous struct in POST /me/tokens. Name and Role are untagged (keys match case-insensitively). */
export interface CreateTokenRequest {
  name: string;
  /** At most the user's own role; empty or missing means the user's role. */
  role?: Role;
  /** Hours; 0 or missing means 90 days; at most 365 days (8760 hours). Fractions allowed. */
  ttl_hours?: number;
  /** Needed when the sign-in is older than 10 minutes (the answer is 400 password_required). */
  password?: string;
}

/** Anonymous map in POST /me/tokens. */
export interface CreateTokenResponse {
  /** The secret (`ibt_…`), shown only now. */
  token: string;
  info: TokenInfo;
  note: string;
}

/** auth.AuditEntry (internal/server/auth/auth.go). */
export interface AuditEntry {
  /** Use as `before=` to page backwards. */
  id: number;
  at: RFC3339;
  actor: string;
  actor_kind: PrincipalKind;
  /** e.g. "login", "setup", "put /policies/{name}". */
  action: string;
  /** Request path. */
  target: string;
  /** json.RawMessage, `{}` when nothing was recorded. */
  detail: unknown;
  ip: string;
  ok: boolean;
}

// ---- dashboard -------------------------------------------------------------------------------

/** service.Overview (internal/server/service/overview.go): the dashboard read model. */
export interface Overview {
  generated_at: RFC3339;
  repositories: RepoOverview[];
  hosts: HostOverview[];
  protection: Protection;
  runs_24h: RunCounts;
  problems: Problem[];
}

/** service.RepoOverview (internal/server/service/overview.go). */
export interface RepoOverview {
  name: string;
  /** "filesystem" | "s3". */
  backend: string;
  mode: RepoMode;
  hosts: number;
  objects: number;
  snapshots: number;
  /** From the last capacity sample. */
  logical_bytes: number;
  /** From the last capacity sample. */
  physical_bytes: number;
  dedup_ratio: number;
  /** 0 for object storage (unknown). */
  fs_total: number;
  fs_free: number;
  chunks: number;
  corrupt_chunks: number;
  failed_verify_snapshots: number;
  last_backup: RFC3339 | null;
  last_verify: RFC3339 | null;
  last_gc: RFC3339 | null;
  last_retention: RFC3339 | null;
  sync_jobs: number;
  sync_failing: number;
}

/** service.HostOverview (internal/server/service/overview.go). */
export interface HostOverview {
  repository: string;
  name: string;
  disabled: boolean;
  last_seen: RFC3339 | null;
  online: boolean;
  /** Storage objects of kind "instance". */
  instances: number;
  /** Block objects. */
  disks: number;
  filesystems: number;
  objects: number;
  /** Objects overdue by their policy schedule. */
  stale: number;
  never_backed_up: number;
  last_backup: RFC3339 | null;
  /** Status of the last backup run ("ok", "partial", "failed", …); null when the host never ran. */
  last_run_status: string | null;
}

/** service.Protection (internal/server/service/overview.go). */
export interface Protection {
  objects: number;
  /** Has a snapshot and is not overdue. */
  protected: number;
  /** Overdue by its policy schedule. */
  stale: number;
  never_backed_up: number;
  /** No policy schedule known: freshness cannot be judged. */
  unscheduled: number;
}

/** service.RunCounts (internal/server/service/overview.go): backup runs in the last 24 h. */
export interface RunCounts {
  ok: number;
  partial: number;
  failed: number;
}

/** service.Problem (internal/server/service/overview.go): something on the dashboard to look at. */
export interface Problem {
  severity: 'warning' | 'critical';
  /** stale_backup | host_offline | corrupt_chunks | verify_failed | sync_failing | no_backup. */
  kind: string;
  repository: string;
  subject: string;
  detail: string;
}

/** service.HostStatus (internal/server/service/maintenance.go). */
export interface HostStatus {
  name: string;
  last_seen: RFC3339 | null;
  objects: number;
  protected: number;
  last_run_at: RFC3339 | null;
  /** Status string of the last finished backup run ("" when none), despite the name. */
  last_run_ok: string;
  /** "project/name" of objects without any snapshot; null when there are none. */
  unprotected: string[] | null;
}

/** service.Status (internal/server/service/maintenance.go). */
export interface Status {
  repository: string;
  snapshots: number;
  objects: number;
  /** null when the repository has no backup hosts. */
  hosts: HostStatus[] | null;
  /** Note the spelling: no underscore before 24h. */
  runs_ok24h: number;
  runs_fail24h: number;
  last_verify: RFC3339 | null;
  last_gc: RFC3339 | null;
  open_sessions: number;
}

/** service.Capacity (internal/server/service/maintenance.go), as embedded in CapacityView. */
export interface Capacity {
  physical_bytes: number;
  logical_bytes: number;
  chunks: number;
  snapshots: number;
  /** 0 for object storage (no fill forecast). */
  fs_total: number;
  fs_free: number;
  dedup_ratio: number;
  /** Go map[int]float64; never seen on the wire as-is (CapacityView replaces it). */
  growth_per_day: Record<string, number>;
  /** Projected date of 80 % fill (note: no underscore). */
  projected80: RFC3339 | null;
  projected_full: RFC3339 | null;
}

/**
 * mgmt.capacityView (internal/server/mgmt/routes_core.go): GET /capacity. Embeds *service.Capacity;
 * its own growth_per_day shadows the embedded one.
 */
export interface CapacityView extends Omit<Capacity, 'growth_per_day'> {
  /** Window in days ("7", "30", "90") -> bytes/day; null: not enough data. */
  growth_per_day: Record<string, number | null>;
  /** Set when free space and a forecast are not available (object store). */
  note?: string;
}

/** service.CapacityPoint (internal/server/service/overview.go): one chart sample. */
export interface CapacityPoint {
  at: RFC3339;
  physical_bytes: number;
  logical_bytes: number;
  fs_free: number;
  fs_total: number;
  chunks: number;
}

// ---- repositories and credentials ------------------------------------------------------------

/** service.S3Settings (internal/server/service/s3repo.go): non-secret S3 settings. */
export interface S3Settings {
  endpoint: string;
  region?: string;
  bucket: string;
  prefix?: string;
  no_tls?: boolean;
  /** PEM of a private CA (public data). */
  ca_pem?: string;
  insecure_skip_verify?: boolean;
  path_style?: boolean;
}

/** mgmt.RepoView (internal/server/mgmt/routes_core.go): a repository without credential material. */
export interface RepoView {
  name: string;
  backend: 'filesystem' | 's3';
  /** Filesystem directory, or "s3://bucket/prefix" (descriptive only). */
  location: string;
  domain_id?: string;
  /** S3 repositories only. */
  s3?: S3Settings;
  /** Credential name (S3 repositories only; missing in the POST responses). */
  credential?: string;
  mode: RepoMode;
  mode_reason?: string;
}

/** mgmt createRepo (internal/server/mgmt/routes_core.go): POST /repositories and /repositories/attach. */
export interface CreateRepoRequest {
  name: string;
  kind: 'filesystem' | 's3';
  /** Filesystem directory (kind filesystem). */
  path?: string;
  /** Required for kind s3. */
  s3?: S3Settings;
  /** Name of a stored credential, or give access_key + secret_key inline. */
  credential?: string;
  access_key?: string;
  secret_key?: string;
  /** Attach only: take over a repository that belongs to another server. */
  adopt?: boolean;
}

/** datastore.ProbeResult (internal/datastore/backend_s3.go): one probe step. */
export interface ProbeResult {
  /** e.g. "connect + bucket exists", "put if-absent (conditional write)". */
  op: string;
  ok: boolean;
  /** Error text, "" when ok (note: `err`, not `error`). */
  err: string;
}

/** Anonymous struct in PUT /repositories/{name}/mode. */
export interface SetModeRequest {
  mode: RepoMode;
  reason?: string;
}

/** service.Credential (internal/server/service/secrets.go): never the secret. */
export interface Credential {
  id: string;
  name: string;
  /** e.g. "s3". */
  kind: string;
  /** The access key id (not secret). */
  access_key: string;
  created_at: RFC3339;
  rotated_at: RFC3339 | null;
  /** Names of repositories using it. */
  used_by: string[];
}

/** mgmt credReq (internal/server/mgmt/routes_core.go): POST /credentials and PUT /credentials/{name} (name ignored on PUT). */
export interface CredentialRequest {
  name?: string;
  access_key: string;
  secret_key: string;
}

// ---- hosts, onboarding, policies -------------------------------------------------------------

/** service.HostRow (internal/server/service/service.go): an enrolled certificate identity. */
export interface HostRow {
  name: string;
  /** e.g. "backup", "sync-read", "sync-write". */
  scope: string;
  fingerprint: string;
  last_addr: string;
  disabled: boolean;
  enrolled: RFC3339;
  last_seen: RFC3339 | null;
}

/** Anonymous struct in POST /onboarding. */
export interface OnboardingRequest {
  host: string;
  /** Default "backup". */
  scope?: string;
  /** Default 24 h when 0 or missing. */
  ttl_hours?: number;
}

/** Anonymous map in POST /onboarding (internal/server/mgmt/routes_more.go). */
export interface OnboardingResult {
  /** One-time enrolment token, shown only now. */
  token: string;
  /** host:port of the agent API the new host should use. */
  server: string;
  /** The exact command to run on the new host. */
  command: string;
  install: string;
  expires_in_hours: number;
  note: string;
}

/** Anonymous struct in POST /tokens (raw enrolment token, CLI parity). */
export interface EnrolmentTokenRequest {
  host: string;
  scope?: string;
  /** Default 1 h when 0 or missing. */
  ttl_hours?: number;
}

/** Anonymous map in POST /tokens. */
export interface EnrolmentTokenResponse {
  token: string;
  expires_in_hours: number;
  note: string;
}

/** api.Policy (internal/api/api.go). Also the PUT /policies/{name} body (name is taken from the path). */
export interface Policy {
  name: string;
  schedule: string;
  targets: string[] | null;
  commit_required: string;
  keep_last: number;
  keep_hourly: number;
  keep_daily: number;
  keep_weekly: number;
  keep_monthly: number;
  keep_yearly: number;
  /** IANA zone in which retention periods are cut (missing = UTC). */
  timezone?: string;
  reverify_days?: number;
  verify_schedule: string;
  restore_test_schedule: string;
}

/** service.AssignmentRow (internal/server/service/policy.go): host default -> project -> object. */
export interface AssignmentRow {
  host: string;
  /** "" = the whole host. */
  project: string;
  /** "" = the whole project. */
  object: string;
  policy: string;
}

/** Alias kept for readers who look for "Assignment". */
export type Assignment = AssignmentRow;

// ---- objects and snapshots -------------------------------------------------------------------

/** service.ObjectRow (internal/server/service/objects_admin.go). */
export interface ObjectRow {
  project: string;
  name: string;
  kind: string;
  host: string;
  /** Policy the host declared. */
  declared_policy: string;
  /** Retention policy override set by an operator. */
  retention_policy: string;
  snapshots: number;
  notes: string;
  /** The {group} path parameter of the object routes. */
  group_id: string;
}

/** Anonymous struct in PUT /objects/{group}/retention. */
export interface SetRetentionRequest {
  policy: string;
}

/** Anonymous struct in PUT /objects/{group}/notes and PUT /snapshots/{group}/{snap}/notes. */
export interface SetNotesRequest {
  notes: string;
}

/** Anonymous struct in POST /objects/{group}/transfer. */
export interface TransferRequest {
  host: string;
}

/** Anonymous struct in PUT /snapshots/{group}/{snap}/protected. */
export interface SetProtectedRequest {
  protected: boolean;
}

/** api.SnapshotRef (internal/api/api.go): one committed snapshot. */
export interface SnapshotRef {
  group_id: string;
  snapshot_id: string;
  created: RFC3339;
  committed: RFC3339;
  index_digest: string;
  chunk_count: number;
  policy?: string;
  project?: string;
  object?: string;
  kind?: string;
  host?: string;
  logical_bytes?: number;
  /** Protected snapshots are never pruned. */
  protected?: boolean;
  /** Missing = never verified. */
  verify_state?: 'ok' | 'failed';
  notes?: string;
}

/** service.SnapshotVerification (internal/server/service/snapshots_admin.go): one history entry. */
export interface SnapshotVerification {
  at: RFC3339;
  state: string;
  chunks: number;
  problems: string[] | null;
}

// ---- maintenance and verification ------------------------------------------------------------

/** service.MaintenanceSettings (internal/server/service/settings.go): schedules of one repository. */
export interface MaintenanceSettings {
  capacity: string;
  reconcile: string;
  prune: string;
  gc: string;
  /** Verify new snapshots and re-verify stale ones. */
  verify: string;
  /** Full chunk scrub. */
  scrub: string;
  /** Nanoseconds. */
  gc_grace: Nanoseconds;
  default_reverify_days: number;
}

/** Anonymous struct{ *service.MaintenanceSettings; GCGraceHours } in GET /maintenance. */
export interface MaintenanceView extends MaintenanceSettings {
  /** gc_grace in hours. */
  gc_grace_hours: number;
}

/**
 * Anonymous struct of pointers in PUT /maintenance: a missing field is left unchanged. The schedule
 * fields are untagged (keys match case-insensitively). `gc_grace` is NOT accepted (400): do not send
 * the GET /maintenance body back unchanged, send gc_grace_hours.
 */
export interface MaintenanceUpdate {
  capacity?: string;
  reconcile?: string;
  prune?: string;
  gc?: string;
  verify?: string;
  scrub?: string;
  gc_grace_hours?: number;
  default_reverify_days?: number;
}

/** service.RunRow (internal/server/service/objects_admin.go): one housekeeping run. */
export interface RunRow {
  /** e.g. "verify", "gc", "retention", "reconcile". */
  kind: string;
  started: RFC3339;
  finished: RFC3339 | null;
  status: string;
  error: string;
  /** The run's stats as JSON TEXT (a string to JSON.parse), not an object. */
  stats: string;
}

/** service.PrunePlanEntry (internal/server/service/maintenance.go): one snapshot's decision. */
export interface PrunePlanEntry {
  group: string;
  snapshot: string;
  reason: string;
  keep: boolean;
  protected: boolean;
  /** "project/name". */
  object: string;
  policy: string;
  timezone: string;
}

/** service.PruneResult (internal/server/service/maintenance.go). */
export interface PruneResult {
  groups: number;
  kept: number;
  removed: number;
  protected: number;
  plan: PrunePlanEntry[] | null;
  dry_run: boolean;
}

/** datastore.GCStats (internal/datastore/maintenance.go). */
export interface GCStats {
  scanned: number;
  live: number;
  kept_young: number;
  kept_pinned: number;
  removed: number;
  bytes_reclaimed: number;
  quarantine_purged: number;
  orphans_removed: number;
  dry_run: boolean;
}

/** Anonymous struct in POST /verify. */
export interface VerifyRequest {
  /** Default "all" (every snapshot and a full chunk scrub). */
  mode?: 'all' | 'new' | 'stale';
  stale_days?: number;
  /** Only report corrupt chunks, do not move them aside. */
  no_quarantine?: boolean;
}

/** datastore.Problem (internal/datastore/maintenance.go): something verification found wrong. */
export interface VerifyProblem {
  /** "chunk-corrupt" | "chunk-missing" | "index-corrupt" | "manifest-corrupt" | "partial-snapshot". */
  kind: string;
  object: string;
  detail: string;
}

/** datastore.SnapshotResult (internal/datastore/maintenance.go). */
export interface SnapshotResult {
  group: string;
  snapshot: string;
  state: string;
  chunks: number;
  problems: string[] | null;
}

/** datastore.VerifyStats (internal/datastore/maintenance.go). */
export interface VerifyStats {
  /** Distinct chunk objects read. */
  chunks: number;
  snapshots: number;
  /** Snapshots not selected by the mode. */
  skipped: number;
  quarantined: number;
  problems: VerifyProblem[] | null;
  results: SnapshotResult[] | null;
}

/** Anonymous map in POST /verify: a finding is `ok: false` with status 200, not an error. */
export interface VerifyResponse {
  ok: boolean;
  error?: string;
  stats: VerifyStats;
}

/** service.ReconcileResult (internal/server/service/maintenance.go). */
export interface ReconcileResult {
  inserted: number;
  removed: number;
}

/** service.CorruptChunk (internal/server/service/maintenance.go). */
export interface CorruptChunk {
  chunk_id: string;
  detected_at: RFC3339;
  /** "quarantined" | "healed". */
  state: string;
  healed_at: RFC3339 | null;
  detail: string;
  /** "group/snapshot" ids affected. */
  affected: string[] | null;
}

// ---- sync ------------------------------------------------------------------------------------

/** syncer.Job (internal/server/syncer/manager.go). */
export interface SyncJob {
  id: string;
  name: string;
  direction: 'push' | 'pull';
  /** Address of the other server. */
  remote: string;
  remote_repository: string;
  schedule: string;
  /** Group ids (32 hex characters); empty or null = all. */
  group_filter: string[] | null;
  enabled: boolean;
  last_success: RFC3339 | null;
  verified_only: boolean;
  resync_corrupt: boolean;
  transfer_last: number;
  /** Bytes per second; 0 = unlimited. */
  rate_limit_bps: number;
}

/**
 * Anonymous struct{ syncer.Job; Token } in POST /sync: the Job is embedded untagged, so its fields are
 * flattened (id and last_success are accepted but meaningless).
 */
export interface SyncCreateRequest extends Partial<SyncJob> {
  name: string;
  direction: 'push' | 'pull';
  remote: string;
  /** One-time enrolment token from the remote (sync-write scope for push, sync-read for pull). */
  token: string;
}

/** syncer.JobOptions (internal/server/syncer/manager.go): PUT /sync/{name}; a missing field is left unchanged. */
export interface SyncJobOptions {
  verified_only?: boolean;
  resync_corrupt?: boolean;
  transfer_last?: number;
  rate_limit_bps?: number;
  /** Replaces group_filter (note the different name). */
  groups?: string[];
}

/** syncer.JobStatus (internal/server/syncer/manager.go): the embedded Job is tagged "job", so it is nested. */
export interface SyncStatus {
  job: SyncJob;
  last_run_at: RFC3339 | null;
  last_run_status: string;
  last_error: string;
  snapshots_pending: number;
  snapshots_synced: number;
  bytes_transferred: number;
  chunks_transferred: number;
  chunks_reused: number;
  /** Age of the oldest unsynchronised snapshot, nanoseconds (0 if none). */
  lag_ns: Nanoseconds;
  failed: number;
  /** Unverified or corrupt on the source, not transferred. */
  snapshots_skipped: number;
  snapshots_repaired: number;
}

/** syncer.Result (internal/server/syncer/engine.go): summary of one run. */
export interface SyncResult {
  pending: number;
  synced: number;
  failed: number;
  skipped_unverified: number;
  skipped_corrupt: number;
  repaired: number;
  /** "group/snapshot" of each repaired snapshot. */
  repaired_list: string[] | null;
  bytes_transferred: number;
  chunks_transferred: number;
  chunks_reused: number;
  errors: string[] | null;
}

/** POST /sync/{name}/run with a partial failure (status 200). */
export interface SyncRunFailure {
  ok: false;
  error: string;
  result: SyncResult;
}

/** POST /sync/{name}/run: a bare SyncResult on success, SyncRunFailure when some snapshots failed. */
export type SyncRunResponse = SyncResult | SyncRunFailure;

// ---- settings --------------------------------------------------------------------------------

/** Known server setting keys (service.settingValidators, internal/server/service/settings_kv.go). */
export type SettingKey = 'base_url' | 'advertise_address' | 'instance_name';

/** map[string]string of GET/PUT /settings. GET always returns every known key ("" = unset). */
export type SettingsMap = Partial<Record<SettingKey, string>> & Record<string, string>;

// ---- live events (GET /events) ---------------------------------------------------------------

/** Event types named in docs/api.md. */
export type EventType = 'job' | 'alert' | 'run' | 'repository' | 'host';

/**
 * mgmt.Event (internal/server/mgmt/hub.go). Each SSE frame is `event: <type>` plus a `data:` line holding
 * this whole object. Because frames are named, EventSource.onmessage does not fire: use
 * addEventListener(type, …). After a reconnect the UI should refetch.
 */
export interface ServerEvent<T = unknown> {
  type: EventType;
  data: T;
}

/** Payload of the "repository" event (published by PUT /repositories/{name}/mode). */
export interface RepositoryModeEvent {
  name: string;
  mode: RepoMode;
}

/** Events with a publisher today. job, alert, run and host are declared but nothing publishes them yet. */
export type KnownServerEvent = ServerEvent<RepositoryModeEvent> & { type: 'repository' };

// ---- DRAFT: planned routes (docs/api.md "Planned routes"); shapes not yet in Go, may change ----

/** @draft Alert / problem severity. */
export type DraftSeverity = 'ok' | 'info' | 'warn' | 'critical';

/** @draft A notification channel (GET/POST /notifications/channels, PUT/DELETE …/{id}). */
export interface DraftNotificationChannel {
  id: string;
  name: string;
  /** e.g. "email", "webhook", "slack", "ntfy". */
  kind: string;
  enabled: boolean;
  /** Kind-specific, non-secret settings. Secrets are accepted once and never returned. */
  config: Record<string, unknown>;
  created_at: RFC3339;
}

/** @draft POST /notifications/channels/{id}/test. */
export interface DraftChannelTestResult {
  ok: boolean;
  error?: string;
}

/** @draft A routing rule (GET/POST /notifications/rules, PUT/DELETE …/{id}). */
export interface DraftNotificationRule {
  id: string;
  name: string;
  /** Minimum severity that matches. */
  min_severity: DraftSeverity;
  /** Problem kinds that match; empty = all. */
  kinds: string[];
  repositories?: string[];
  hosts?: string[];
  channels: string[];
  /** How often an unresolved alert is sent again, minutes (0 = never). */
  repeat_minutes?: number;
  enabled: boolean;
}

/** @draft GET /alerts?state=&severity=, GET /alerts/{id}, POST /alerts/{id}/ack. */
export interface DraftAlert {
  id: string;
  state: 'firing' | 'acknowledged' | 'resolved';
  severity: DraftSeverity;
  kind: string;
  repository?: string;
  subject: string;
  detail: string;
  first_seen: RFC3339;
  last_seen: RFC3339;
  resolved_at?: RFC3339;
  acked_by?: string;
  acked_at?: RFC3339;
  silenced?: boolean;
}

/** @draft GET/POST /silences, DELETE /silences/{id}. */
export interface DraftSilence {
  id: string;
  kind?: string;
  repository?: string;
  subject?: string;
  reason: string;
  created_by: string;
  starts_at: RFC3339;
  ends_at: RFC3339;
}

/** @draft GET /notifications/deliveries. */
export interface DraftDelivery {
  id: string;
  alert_id: string;
  channel_id: string;
  at: RFC3339;
  ok: boolean;
  error?: string;
  attempt: number;
}

/** @draft Job type. */
export type DraftJobType = 'backup' | 'restore' | 'file-restore' | 'verify' | 'sync' | 'restore-test';

/** @draft Job state. */
export type DraftJobState = 'queued' | 'running' | 'ok' | 'failed' | 'cancelled';

/** @draft POST /jobs body. */
export interface DraftJobRequest {
  type: DraftJobType;
  host: string;
  repository: string;
  params: Record<string, unknown>;
}

/** @draft GET /jobs?state=&host=, GET /jobs/{id} (log only on the single-job route), POST /jobs/{id}/cancel. */
export interface DraftJob {
  id: string;
  type: DraftJobType;
  state: DraftJobState;
  host: string;
  repository: string;
  params: Record<string, unknown>;
  created_by: string;
  created_at: RFC3339;
  started_at?: RFC3339;
  finished_at?: RFC3339;
  error?: string;
  /** Present on GET /jobs/{id}. */
  log?: DraftJobLogLine[];
}

/** @draft One job log line. */
export interface DraftJobLogLine {
  at: RFC3339;
  level: 'debug' | 'info' | 'warn' | 'error';
  msg: string;
}

/** @draft GET /restore-tests. */
export interface DraftRestoreTest {
  id: string;
  /** Object group id or "project/name". */
  object: string;
  mode: string;
  snapshot_id?: string;
  state: DraftJobState;
  started_at: RFC3339;
  finished_at?: RFC3339;
  error?: string;
  job_id?: string;
}

/** @draft POST /restore-tests/run body. */
export interface DraftRestoreTestRequest {
  object: string;
  mode: string;
}

/** @draft Certificate expiry state. */
export type DraftExpiryState = 'ok' | 'expiring' | 'expired';

/** webtls.Info (internal/server/webtls/webtls.go): the web certificate in use (exists in Go today). */
export interface WebCertInfo {
  /** "self" | "files" | "acme". */
  mode: 'self' | 'files' | 'acme';
  subject: string;
  dns_names: string[] | null;
  /** Zero time ("0001-01-01T00:00:00Z") in acme mode before the first handshake. */
  not_after: RFC3339;
  fingerprint: string;
  issuer: string;
  days_left: number;
  auto_renew: boolean;
}

/** @draft A CA or server certificate summary. */
export interface DraftCertSummary {
  subject: string;
  fingerprint: string;
  not_after: RFC3339;
  days_left: number;
  state: DraftExpiryState;
}

/** @draft A host certificate (POST /certificates/hosts/{name}/renew-now asks the host to renew). */
export interface DraftHostCert {
  repository: string;
  name: string;
  scope: string;
  fingerprint: string;
  not_after: RFC3339;
  days_left: number;
  state: DraftExpiryState;
}

/** @draft GET /certificates. */
export interface DraftCertificateInfo {
  ca: DraftCertSummary;
  server: DraftCertSummary;
  web: WebCertInfo & { state?: DraftExpiryState };
  hosts: DraftHostCert[];
}
