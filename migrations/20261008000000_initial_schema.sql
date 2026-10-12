-- incus-backup server control plane: the complete schema of the first release.
--
-- Before the first release the schema was built up by several migrations; they were consolidated
-- into this one file ("one migration per released version", docs/design.md section 3.1). Later
-- releases add new timestamp-named files next to it and never edit it.
--
-- PostgreSQL is the permanent source of truth for operational state (hosts, policies, assignments,
-- run history, sync jobs, statistics, people, notifications, tasks). It is NOT the source of truth
-- for backup payload: which chunks belong to which snapshot lives in the datastore's index files,
-- so losing this database never loses backups and the snapshot tables can be rebuilt from the
-- datastore (reconcile).
--
-- schema_migrations is created by the migration runner (internal/server/db), not here.
--
-- Sections:
--   1. Server identity, secret store, repositories
--   2. Hosts, enrolment tokens, host certificates
--   3. Policies and assignments
--   4. Storage objects, snapshots, sessions, backup runs
--   5. Verification and corruption
--   6. Maintenance: settings, tasks, maintenance runs, capacity, system samples
--   7. Server-to-server sync
--   8. People: users, sessions, API tokens, per-repository scopes, audit log, first-run setup
--   9. Server settings
--  10. Alerting and notifications
--  11. Host jobs, restore tests, sealed deliveries
--  12. Traffic control
--  13. Performance indexes
--  14. Passkeys / security keys (WebAuthn) and single sign-on (OIDC)

------------------------------------------------------------------------------------------------
-- 1. Server identity, secret store, repositories
------------------------------------------------------------------------------------------------

-- Identity of this server instance. A repository records which server owns it
-- (owner.json inside the datastore), so a second server cannot adopt it by accident.
CREATE TABLE server_identity (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    id        text NOT NULL DEFAULT substr(replace(gen_random_uuid()::text, '-', ''), 1, 24),
    created_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO server_identity DEFAULT VALUES;

-- Secret store for infrastructure credentials: S3 access keys, notification secrets (SMTP
-- passwords, API tokens, webhook URLs and signing secrets) and OIDC client secrets. The secret is sealed
-- (XChaCha20-Poly1305) with a key that lives in a 0600 file in the server state directory
-- (secrets.key), never in this database; PostgreSQL only ever holds ciphertext. This is unrelated
-- to the client-side backup encryption keys, which the server never has.
CREATE TABLE credentials (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL UNIQUE,
    kind       text NOT NULL,
    sealed     bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    rotated_at timestamptz,
    CONSTRAINT credentials_kind_check CHECK (kind IN ('s3', 'smtp', 'token', 'webhook', 'oidc'))
);

CREATE TABLE repositories (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name           text NOT NULL UNIQUE,
    datastore_path text NOT NULL,
    backend        text NOT NULL DEFAULT 'filesystem' CHECK (backend IN ('filesystem', 's3')),
    -- Non-secret S3 settings (endpoint, region, bucket, prefix, TLS options); the access key
    -- lives in the secret store, referenced by credential_id.
    s3_settings    jsonb NOT NULL DEFAULT '{}',
    credential_id  uuid REFERENCES credentials(id) ON DELETE RESTRICT,
    -- datastore.json id, pinned at creation/attach: a different datastore behind the same
    -- path or bucket is refused instead of silently adopted.
    datastore_id   text NOT NULL DEFAULT '',
    -- Non-secret key domain id, pinned by the first backup committed. Hosts
    -- presenting another domain id are refused.
    domain_id      text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- Maintenance mode: online (normal), read-only (restores and reads only), offline (no access).
    mode            text NOT NULL DEFAULT 'online' CHECK (mode IN ('online','read-only','offline')),
    mode_reason     text NOT NULL DEFAULT '',
    mode_changed_at timestamptz
);

-- One repository per datastore: the same directory or bucket must never be attached twice (two
-- entries would have separate session pins, retention and key-domain pins over the same chunks).
CREATE UNIQUE INDEX repositories_one_datastore ON repositories (datastore_id) WHERE datastore_id <> '';
CREATE UNIQUE INDEX repositories_one_location ON repositories (datastore_path);

------------------------------------------------------------------------------------------------
-- 2. Hosts, enrolment tokens, host certificates
------------------------------------------------------------------------------------------------

-- Hosts (backup scope) and sync peers (sync-read / sync-write) authenticate with mutual TLS.
CREATE TABLE hosts (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id    uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    name             text NOT NULL,
    scope            text NOT NULL CHECK (scope IN ('backup', 'sync-read', 'sync-write')),
    -- the CURRENT certificate; host_certs is the set the server accepts
    cert_fingerprint text NOT NULL UNIQUE,
    enrolled_at      timestamptz NOT NULL DEFAULT now(),
    last_seen_at     timestamptz,
    last_addr        text,
    disabled         boolean NOT NULL DEFAULT false,
    info             jsonb NOT NULL DEFAULT '{}',
    last_renewed_at  timestamptz,
    UNIQUE (repository_id, name, scope)
);

CREATE TABLE enrollment_tokens (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    host_name     text NOT NULL,
    scope         text NOT NULL CHECK (scope IN ('backup', 'sync-read', 'sync-write')),
    token_hash    bytea NOT NULL UNIQUE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz
);

-- Certificate lifecycle (docs/certificates.md).
--
-- A host (or sync peer) identity may hold several certificates at once while it renews: the new
-- one is issued, the old one stays valid until it expires or the client confirms that the new one
-- works. hosts.cert_fingerprint keeps pointing at the CURRENT certificate; host_certs is the set
-- the server accepts. The SAN list of the server certificate is stored in server_settings ('sans').
CREATE TABLE host_certs (
    fingerprint        text PRIMARY KEY,
    host_id            uuid NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    not_before         timestamptz NOT NULL,
    not_after          timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    revoked            boolean NOT NULL DEFAULT false,
    revoked_at         timestamptz,
    -- the certificate this one was renewed from ('' = issued at enrolment)
    parent_fingerprint text NOT NULL DEFAULT '',
    -- fingerprint of the CA that signed it ('' = the CA of a server that never rotated)
    issuer_fingerprint text NOT NULL DEFAULT '',
    -- first authenticated request made with it (a renewal whose answer was lost is never "used")
    first_used_at      timestamptz,
    confirmed_at       timestamptz
);
CREATE INDEX host_certs_host ON host_certs (host_id);

------------------------------------------------------------------------------------------------
-- 3. Policies and assignments
------------------------------------------------------------------------------------------------

CREATE TABLE policies (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id         uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    name                  text NOT NULL,
    schedule              text NOT NULL DEFAULT '03:00',
    targets               text[] NOT NULL DEFAULT '{}',
    commit_required       text NOT NULL DEFAULT 'all' CHECK (commit_required IN ('all', 'any')),
    keep_last             integer CHECK (keep_last >= 0),
    keep_hourly           integer CHECK (keep_hourly >= 0),
    keep_daily            integer CHECK (keep_daily >= 0),
    keep_weekly           integer CHECK (keep_weekly >= 0),
    keep_monthly          integer CHECK (keep_monthly >= 0),
    keep_yearly           integer CHECK (keep_yearly >= 0),
    -- IANA time zone in which hourly/daily/weekly/monthly/yearly retention periods are cut.
    timezone              text NOT NULL DEFAULT 'UTC',
    verify_schedule       text NOT NULL DEFAULT 'weekly',
    -- verify snapshots whose last verification is older than this many days (0 = only new ones)
    reverify_days         integer NOT NULL DEFAULT 30 CHECK (reverify_days >= 0),
    restore_test_schedule text NOT NULL DEFAULT 'monthly',
    UNIQUE (repository_id, name)
);

-- Assignment scope: ('' , '') host default; (project, '') project; (project, object) object.
CREATE TABLE policy_assignments (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    host_id   uuid NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    project   text NOT NULL DEFAULT '',
    object    text NOT NULL DEFAULT '',
    policy_id uuid NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    CHECK (project <> '' OR object = ''),
    UNIQUE (host_id, project, object)
);

------------------------------------------------------------------------------------------------
-- 4. Storage objects, snapshots, sessions, backup runs
------------------------------------------------------------------------------------------------

CREATE TABLE storage_objects (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    host_id       uuid REFERENCES hosts(id) ON DELETE SET NULL,
    host_name     text NOT NULL DEFAULT '',
    project       text NOT NULL DEFAULT '',
    name          text NOT NULL DEFAULT '',
    kind          text NOT NULL DEFAULT 'block' CHECK (kind IN ('block', 'filesystem', 'instance')),
    group_id      text NOT NULL,
    policy_name   text NOT NULL DEFAULT '',        -- policy the host last declared (informational)
    -- Retention authority: set when the object is first seen and never changed by a
    -- backup-scope host, so a compromised host cannot point an object at an aggressive
    -- policy to make the server prune its history. Admin assignments override it.
    retention_policy text NOT NULL DEFAULT '',
    shared_by     jsonb NOT NULL DEFAULT '[]',
    notes         text NOT NULL DEFAULT '',
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repository_id, group_id)
);

CREATE TABLE snapshots (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id        uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    object_id            uuid REFERENCES storage_objects(id) ON DELETE SET NULL,
    group_id             text NOT NULL,
    snapshot_id          text NOT NULL,
    created_at           timestamptz NOT NULL,
    committed_at         timestamptz NOT NULL DEFAULT now(),
    policy_name          text NOT NULL DEFAULT '',
    source               text NOT NULL DEFAULT 'client' CHECK (source IN ('client', 'sync', 'reconcile')),
    logical_bytes        bigint NOT NULL DEFAULT 0,
    dirty_bytes          bigint NOT NULL DEFAULT 0,
    read_bytes           bigint NOT NULL DEFAULT 0,
    uploaded_bytes       bigint NOT NULL DEFAULT 0,
    full_read            boolean NOT NULL DEFAULT false,
    duration_ms          bigint NOT NULL DEFAULT 0,
    index_digest         text NOT NULL,
    previous_snapshot_id text NOT NULL DEFAULT '',
    chunk_count          integer NOT NULL DEFAULT 0,
    -- Cache of repository metadata (the datastore is authoritative; reconcile re-reads these).
    protected            boolean NOT NULL DEFAULT false,
    notes                text NOT NULL DEFAULT '',
    verify_state         text NOT NULL DEFAULT '' CHECK (verify_state IN ('', 'ok', 'failed')),
    verified_at          timestamptz,
    UNIQUE (repository_id, group_id, snapshot_id)
);
CREATE INDEX snapshots_group_time ON snapshots (repository_id, group_id, created_at DESC);

CREATE TABLE sessions (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id  uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    host_id        uuid REFERENCES hosts(id) ON DELETE SET NULL,
    group_id       text NOT NULL,
    snapshot_id    text NOT NULL,
    state          text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'committed', 'aborted', 'expired')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- Activity lease: every API call (and explicit heartbeat) pushes it forward, so a
    -- legitimately long backup never expires while a dead client is reclaimed after one lease.
    expires_at     timestamptz NOT NULL,
    last_activity_at timestamptz NOT NULL DEFAULT now(),
    -- Snapshot the client builds on (its previous backup): pinned against prune/forget while
    -- this session is open (this is what the design called gc_pins).
    base_snapshot_id text NOT NULL DEFAULT '',
    chunks_checked bigint NOT NULL DEFAULT 0,
    chunks_new     bigint NOT NULL DEFAULT 0,
    bytes_uploaded bigint NOT NULL DEFAULT 0,
    meta           jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX sessions_open ON sessions (state, expires_at);
-- Exactly one active writer per storage-object group, enforced by the database itself.
CREATE UNIQUE INDEX sessions_one_writer ON sessions (repository_id, group_id) WHERE state = 'open';

CREATE TABLE backup_runs (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id  uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    host_id        uuid REFERENCES hosts(id) ON DELETE SET NULL,
    started_at     timestamptz NOT NULL,
    finished_at    timestamptz,
    -- 'deferred': the objects were refused because the repository was in maintenance (read-only or
    -- offline); the host retries at its next run; it is not a failure to alert about.
    status         text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'ok', 'partial', 'failed', 'deferred')),
    objects_total  integer NOT NULL DEFAULT 0,
    objects_ok     integer NOT NULL DEFAULT 0,
    objects_failed integer NOT NULL DEFAULT 0,
    logical_bytes  bigint NOT NULL DEFAULT 0,
    uploaded_bytes bigint NOT NULL DEFAULT 0,
    error          text NOT NULL DEFAULT '',
    objects_deferred integer NOT NULL DEFAULT 0
);

------------------------------------------------------------------------------------------------
-- 5. Verification and corruption
------------------------------------------------------------------------------------------------

-- Corruption found by verify: the chunk is quarantined in the datastore; this records it and
-- which snapshots it affects until a backup or sync heals it.
CREATE TABLE corrupt_chunks (
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    chunk_id      text NOT NULL,
    detected_at   timestamptz NOT NULL DEFAULT now(),
    state         text NOT NULL DEFAULT 'quarantined' CHECK (state IN ('quarantined', 'reported', 'healed')),
    healed_at     timestamptz,
    detail        text NOT NULL DEFAULT '',
    affected      jsonb NOT NULL DEFAULT '[]',   -- ["<group>/<snapshot>", ...]
    PRIMARY KEY (repository_id, chunk_id)
);

-- History of verification runs per snapshot (the latest state is also on snapshots).
CREATE TABLE snapshot_verifications (
    id            bigserial PRIMARY KEY,
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    group_id      text NOT NULL,
    snapshot_id   text NOT NULL,
    verified_at   timestamptz NOT NULL,
    state         text NOT NULL CHECK (state IN ('ok', 'failed')),
    chunks        integer NOT NULL DEFAULT 0,
    problems      jsonb NOT NULL DEFAULT '[]'
);
CREATE INDEX snapshot_verifications_snap ON snapshot_verifications (repository_id, group_id, snapshot_id, verified_at DESC);

------------------------------------------------------------------------------------------------
-- 6. Maintenance: settings, tasks, maintenance runs, capacity, system samples
------------------------------------------------------------------------------------------------

-- Housekeeping schedules per repository (managed through the API/CLI/UI, not a config file).
CREATE TABLE maintenance_settings (
    repository_id         uuid PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    capacity              text NOT NULL DEFAULT 'every 1h',
    reconcile             text NOT NULL DEFAULT 'daily',
    prune                 text NOT NULL DEFAULT '04:00',
    gc                    text NOT NULL DEFAULT '05:00',
    -- verify new snapshots and re-verify those older than their policy's reverify_days
    verify                text NOT NULL DEFAULT 'daily',
    -- full scrub: read every chunk object, including unreferenced ones
    scrub                 text NOT NULL DEFAULT 'monthly',
    gc_grace_hours        integer NOT NULL DEFAULT 24 CHECK (gc_grace_hours >= 12),
    default_reverify_days integer NOT NULL DEFAULT 30 CHECK (default_reverify_days >= 0)
);

-- A task is one long server-side operation (GC, verify, retention, reconcile, capacity sample, sync run,
-- repository probe or destroy, group delete) started by a person, a token or the scheduler. It has a
-- persisted log, progress, a result and can be cancelled. Tasks of a repository that is destroyed keep
-- their row (repository_id becomes NULL, repository_name stays).
CREATE TABLE tasks (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq              bigserial NOT NULL,
    kind             text NOT NULL,
    repository_id    uuid REFERENCES repositories(id) ON DELETE SET NULL,
    repository_name  text NOT NULL DEFAULT '',
    started_by       text NOT NULL DEFAULT '',       -- user or token name, "local" (admin socket), "system" (scheduler)
    actor_kind       text NOT NULL DEFAULT 'system' CHECK (actor_kind IN ('user', 'token', 'local', 'system')),
    state            text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    progress         integer NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    message          text NOT NULL DEFAULT '',
    params           jsonb NOT NULL DEFAULT '{}',
    result           jsonb,
    error            text NOT NULL DEFAULT '',
    -- destructive tasks hold the repository's maintenance slot (see tasks_one_destructive)
    exclusive        boolean NOT NULL DEFAULT false,
    cancel_requested boolean NOT NULL DEFAULT false,
    log_lines        integer NOT NULL DEFAULT 0,
    log_dropped      integer NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL,
    started_at       timestamptz,
    finished_at      timestamptz
);
CREATE INDEX tasks_recent ON tasks (created_at DESC, seq DESC);
CREATE INDEX tasks_active ON tasks (state) WHERE state IN ('queued', 'running');
CREATE INDEX tasks_repo ON tasks (repository_id, created_at DESC);
-- One destructive maintenance task per repository at a time, for the scheduler and people alike.
CREATE UNIQUE INDEX tasks_one_destructive ON tasks (repository_id) WHERE exclusive AND state IN ('queued', 'running');

CREATE TABLE task_log (
    task_id uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    seq     integer NOT NULL,
    at      timestamptz NOT NULL,
    level   text NOT NULL DEFAULT 'info' CHECK (level IN ('debug', 'info', 'warn', 'error')),
    line    text NOT NULL,
    PRIMARY KEY (task_id, seq)
);

-- The scheduled-run history (the scheduler reads it); each run links the task that executed it.
-- A run a person cancelled is 'cancelled', not 'failed' (no maintenance_failed alert).
CREATE TABLE maintenance_runs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    kind          text NOT NULL CHECK (kind IN ('gc', 'verify', 'scrub', 'retention', 'reconcile', 'heal')),
    started_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz,
    status        text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'ok', 'failed', 'cancelled')),
    stats         jsonb NOT NULL DEFAULT '{}',
    error         text NOT NULL DEFAULT '',
    task_id       uuid REFERENCES tasks(id) ON DELETE SET NULL
);
CREATE INDEX maintenance_runs_kind ON maintenance_runs (repository_id, kind, started_at DESC);

CREATE TABLE capacity_samples (
    id             bigserial PRIMARY KEY,
    repository_id  uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    taken_at       timestamptz NOT NULL DEFAULT now(),
    physical_bytes bigint NOT NULL,
    logical_bytes  bigint NOT NULL DEFAULT 0,
    chunk_count    bigint NOT NULL,
    snapshot_count bigint NOT NULL,
    fs_total_bytes bigint NOT NULL DEFAULT 0,
    fs_free_bytes  bigint NOT NULL DEFAULT 0
);
CREATE INDEX capacity_samples_time ON capacity_samples (repository_id, taken_at);

-- Server health samples (one a minute, kept 7 days) for the status page charts.
CREATE TABLE system_samples (
    taken_at        timestamptz PRIMARY KEY,
    cpu_percent     real NOT NULL DEFAULT 0,
    load1           real NOT NULL DEFAULT 0,
    mem_used_bytes  bigint NOT NULL DEFAULT 0,
    mem_total_bytes bigint NOT NULL DEFAULT 0,
    disk_used_bytes bigint NOT NULL DEFAULT 0,   -- the state directory's filesystem
    disk_total_bytes bigint NOT NULL DEFAULT 0,
    db_bytes        bigint NOT NULL DEFAULT 0,
    process_rss_bytes bigint NOT NULL DEFAULT 0,
    goroutines      integer NOT NULL DEFAULT 0
);

------------------------------------------------------------------------------------------------
-- 7. Server-to-server sync
------------------------------------------------------------------------------------------------

CREATE TABLE sync_jobs (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id     uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    name              text NOT NULL,
    direction         text NOT NULL CHECK (direction IN ('push', 'pull')),
    remote            text NOT NULL,
    remote_repository text NOT NULL,
    remote_fingerprint text NOT NULL DEFAULT '',
    schedule          text NOT NULL DEFAULT '',
    group_filter      text[] NOT NULL DEFAULT '{}',
    -- PBS-class controls
    verified_only     boolean NOT NULL DEFAULT false,   -- only snapshots whose last verification is ok
    resync_corrupt    boolean NOT NULL DEFAULT false,   -- also repair corrupt/quarantined chunks on the destination from the source
    transfer_last     integer NOT NULL DEFAULT 0 CHECK (transfer_last >= 0), -- only the newest N snapshots per group (0 = all)
    rate_limit_bps    bigint NOT NULL DEFAULT 0 CHECK (rate_limit_bps >= 0), -- bytes/second of blob traffic (0 = unlimited)
    enabled           boolean NOT NULL DEFAULT true,
    -- Reserved: a mirror mode that propagates retention is not implemented in v1.
    mirror            boolean NOT NULL DEFAULT false CHECK (NOT mirror),
    last_success_at   timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repository_id, name)
);

CREATE TABLE sync_runs (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id             uuid NOT NULL REFERENCES sync_jobs(id) ON DELETE CASCADE,
    started_at         timestamptz NOT NULL DEFAULT now(),
    finished_at        timestamptz,
    status             text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'ok', 'partial', 'failed')),
    snapshots_pending  integer NOT NULL DEFAULT 0,
    snapshots_synced   integer NOT NULL DEFAULT 0,
    bytes_transferred  bigint NOT NULL DEFAULT 0,
    chunks_transferred bigint NOT NULL DEFAULT 0,
    chunks_reused      bigint NOT NULL DEFAULT 0,
    retries            integer NOT NULL DEFAULT 0,
    snapshots_skipped  integer NOT NULL DEFAULT 0,   -- unverified or corrupt on the source
    snapshots_repaired integer NOT NULL DEFAULT 0,   -- destination snapshots healed by resync-corrupt
    error              text NOT NULL DEFAULT ''
);
CREATE INDEX sync_runs_job ON sync_runs (job_id, started_at DESC);

CREATE TABLE sync_snapshot_state (
    job_id      uuid NOT NULL REFERENCES sync_jobs(id) ON DELETE CASCADE,
    group_id    text NOT NULL,
    snapshot_id text NOT NULL,
    state       text NOT NULL CHECK (state IN ('pending', 'synced', 'verified', 'failed')),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    error       text NOT NULL DEFAULT '',
    PRIMARY KEY (job_id, group_id, snapshot_id)
);

------------------------------------------------------------------------------------------------
-- 8. People: users, sessions, API tokens, per-repository scopes, audit log, first-run setup
------------------------------------------------------------------------------------------------

-- Management plane authentication. Host (agent) authentication is separate and stays on mutual
-- TLS; these tables are for people and automation using the web UI and the management API.
--
-- A user has one global role for routes that are not bound to a repository; 'none' is a person who
-- only has per-repository access. A user with repo_scoped=true gets, on a repository-bound route,
-- the role of their user_repo_scopes row for that repository and NOTHING on other repositories. The
-- explicit flag (instead of "has rows") makes the rule fail closed: deleting a repository cascades
-- its scope rows away, and that must never widen access (docs/permissions.md).
CREATE TABLE users (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name             text NOT NULL,
    display_name     text NOT NULL DEFAULT '',
    email            text NOT NULL DEFAULT '',
    role             text NOT NULL,
    password_hash    text NOT NULL,               -- argon2id; '' = no local password (a user created by single sign-on)
    must_change      boolean NOT NULL DEFAULT false,
    totp_sealed      bytea,                       -- sealed TOTP secret (secret store key), NULL = no 2FA enrolled
    totp_enabled     boolean NOT NULL DEFAULT false,
    totp_last_step   bigint NOT NULL DEFAULT 0,   -- last accepted time step: a code cannot be replayed
    recovery_hashes  text[] NOT NULL DEFAULT '{}',
    disabled         boolean NOT NULL DEFAULT false,
    failed_logins    integer NOT NULL DEFAULT 0,
    locked_until     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    password_changed_at timestamptz NOT NULL DEFAULT now(),
    last_login_at    timestamptz,
    repo_scoped      boolean NOT NULL DEFAULT false,
    -- WebAuthn user handle (user.id in the ceremonies, section 14): random, never the user name, set at the
    -- first registration. A discoverable ("passkey") sign-in names the user only by this handle.
    webauthn_handle  bytea UNIQUE,
    CONSTRAINT users_role_check CHECK (role IN ('admin','operator','restore','readonly','none'))
);
CREATE UNIQUE INDEX users_name_lower ON users (lower(name));

CREATE TABLE user_repo_scopes (
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    role          text NOT NULL CHECK (role IN ('admin','operator','restore','readonly')),
    PRIMARY KEY (user_id, repository_id)
);
CREATE INDEX user_repo_scopes_repo ON user_repo_scopes (repository_id);

CREATE TABLE user_sessions (
    token_hash  bytea PRIMARY KEY,                -- SHA-256 of the cookie value; the cookie itself is never stored
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf        text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    last_seen   timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,             -- absolute lifetime
    ip          text NOT NULL DEFAULT '',
    user_agent  text NOT NULL DEFAULT '',
    -- how the session was opened: 'password' (with or without a second factor), 'passkey' or 'oidc'
    method      text NOT NULL DEFAULT 'password' CHECK (method IN ('password','passkey','oidc'))
);
CREATE INDEX user_sessions_user ON user_sessions (user_id);

-- An API token may be restricted to a list of repositories (repo_restricted=true). A restricted token
-- has no global rights at all, and on each listed repository the lower of its own role and its owner's
-- role there. Same fail-closed flag as for users.
CREATE TABLE api_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        text NOT NULL,
    token_hash  bytea NOT NULL UNIQUE,
    role        text NOT NULL CHECK (role IN ('admin','operator','restore','readonly')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz,
    last_used_at timestamptz,
    repo_restricted boolean NOT NULL DEFAULT false,
    UNIQUE (user_id, name)
);

CREATE TABLE api_token_repos (
    token_id      uuid NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    PRIMARY KEY (token_id, repository_id)
);
CREATE INDEX api_token_repos_repo ON api_token_repos (repository_id);

CREATE TABLE audit_log (
    id        bigserial PRIMARY KEY,
    at        timestamptz NOT NULL DEFAULT now(),
    actor     text NOT NULL,                      -- user name, "token:<name>", "local-admin", "system"
    actor_kind text NOT NULL DEFAULT 'user',
    action    text NOT NULL,                      -- e.g. "login", "repository.create", "snapshot.forget"
    target    text NOT NULL DEFAULT '',
    detail    jsonb NOT NULL DEFAULT '{}',
    ip        text NOT NULL DEFAULT '',
    ok        boolean NOT NULL DEFAULT true
);
CREATE INDEX audit_log_at ON audit_log (at DESC);

-- First-run setup: while no user exists the server prints a one-time setup token; the web UI asks for it
-- before it creates the first administrator.
CREATE TABLE auth_setup (
    singleton   boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    token_hash  bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

------------------------------------------------------------------------------------------------
-- 9. Server settings
------------------------------------------------------------------------------------------------

-- Operational server settings that used to live in a config file: everything an administrator may change
-- at run time is data (edited in the web UI or through /api/v1/settings). Values are text (JSON where
-- structured, e.g. 'sans' = the subject alternative names of the server certificate; when absent, the
-- SANs of the current certificate, the bootstrap value given to `init`, apply).
CREATE TABLE server_settings (
    key        text PRIMARY KEY,
    value      text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);

------------------------------------------------------------------------------------------------
-- 10. Alerting and notifications (internal/server/notify, docs/notifications.md)
------------------------------------------------------------------------------------------------

-- Alerts are a state machine (firing -> acknowledged/silenced -> resolved), not one mail per event.
-- Every threshold lives in this database (notify_settings, notify_conditions), never in a file.

-- Global behaviour of the alert engine (one row). Conservative defaults: see docs/notifications.md.
CREATE TABLE notify_settings (
    singleton                boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    -- the same notification (alert, rule, kind) is never sent twice within this window
    dedupe_minutes           integer NOT NULL DEFAULT 10 CHECK (dedupe_minutes >= 0),
    -- an alert that (re)fires flap_threshold times within flap_window_minutes is "flapping": held,
    -- and released once it stayed in one state for flap_window_minutes
    flap_window_minutes      integer NOT NULL DEFAULT 60 CHECK (flap_window_minutes >= 1),
    flap_threshold           integer NOT NULL DEFAULT 3 CHECK (flap_threshold >= 2),
    -- delivery queue
    max_attempts             integer NOT NULL DEFAULT 8 CHECK (max_attempts >= 1),
    backoff_base_seconds     integer NOT NULL DEFAULT 60 CHECK (backoff_base_seconds >= 1),
    backoff_max_seconds      integer NOT NULL DEFAULT 21600 CHECK (backoff_max_seconds >= 1),
    -- events fed through Emit have no clearing condition: they resolve (silently) this long after last_seen
    event_expire_minutes     integer NOT NULL DEFAULT 1440 CHECK (event_expire_minutes >= 1),
    -- housekeeping
    alert_retention_days     integer NOT NULL DEFAULT 90 CHECK (alert_retention_days >= 1),
    delivery_retention_days  integer NOT NULL DEFAULT 30 CHECK (delivery_retention_days >= 1)
);
INSERT INTO notify_settings DEFAULT VALUES;

-- One row per alert condition: enabled, base severity and its thresholds (params).
CREATE TABLE notify_conditions (
    type     text PRIMARY KEY,
    enabled  boolean NOT NULL DEFAULT true,
    severity text NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    params   jsonb NOT NULL DEFAULT '{}'
);
INSERT INTO notify_conditions (type, severity, params) VALUES
    ('backup_failed',          'warning',  '{"failed_severity": "critical"}'),
    ('backup_missed',          'warning',  '{"interval_factor": 1.5, "grace_minutes": 60, "critical_factor": 3, "stale_object_days": 7}'),
    ('host_offline',           'warning',  '{"after_minutes": 120, "critical_after_minutes": 1440}'),
    ('maintenance_failed',     'warning',  '{}'),
    ('corruption',             'critical', '{}'),
    ('sync_failed',            'warning',  '{}'),
    ('sync_lag',               'warning',  '{"lag_hours": 48}'),
    ('capacity_warning',       'warning',  '{"full_within_days": 14, "critical_within_days": 3, "used_percent": 90}'),
    ('repository_unreachable', 'critical', '{"probe_minutes": 10, "failures": 2}'),
    ('cert_expiry',            'warning',  '{"warn_days": 21, "critical_days": 7}'),
    ('restore_test_failed',    'warning',  '{}'),
    ('login_security',         'warning',  '{}'),
    ('job_failed',             'warning',  '{}'),
    -- the opt-in update check raises an informational alert while a newer release exists
    ('update_available',       'info',     '{}');

CREATE TABLE notify_channels (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name            text NOT NULL UNIQUE,
    kind            text NOT NULL CHECK (kind IN ('smtp', 'webhook', 'ntfy', 'gotify')),
    config          jsonb NOT NULL DEFAULT '{}',      -- never a secret
    secret_id       uuid REFERENCES credentials(id) ON DELETE RESTRICT,
    enabled         boolean NOT NULL DEFAULT true,
    rate_per_hour   integer NOT NULL DEFAULT 30 CHECK (rate_per_hour >= 0),  -- 0 = unlimited
    timeout_seconds integer NOT NULL DEFAULT 15 CHECK (timeout_seconds BETWEEN 1 AND 300),
    created         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE notify_rules (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name                 text NOT NULL UNIQUE,
    kind                 text NOT NULL DEFAULT 'alert' CHECK (kind IN ('alert', 'digest')),
    event_types          text[] NOT NULL DEFAULT '{}',   -- empty = every type
    min_severity         text NOT NULL DEFAULT 'warning' CHECK (min_severity IN ('info', 'warning', 'critical')),
    host_glob            text NOT NULL DEFAULT '',
    repo_glob            text NOT NULL DEFAULT '',
    object_glob          text NOT NULL DEFAULT '',
    channel_ids          uuid[] NOT NULL DEFAULT '{}',
    repeat_minutes       integer NOT NULL DEFAULT 1440 CHECK (repeat_minutes >= 0),  -- 0 = never remind
    quiet_start          text NOT NULL DEFAULT '',      -- 'HH:MM' local to timezone; '' = no quiet hours
    quiet_end            text NOT NULL DEFAULT '',
    timezone             text NOT NULL DEFAULT 'UTC',   -- quiet hours and digest time
    critical_bypass      boolean NOT NULL DEFAULT true, -- critical alerts ignore quiet hours
    send_resolved        boolean NOT NULL DEFAULT true,
    digest_at            text NOT NULL DEFAULT '08:00', -- digest rules: local send time
    last_digest_at       timestamptz,
    enabled              boolean NOT NULL DEFAULT true,
    created              timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE notify_alerts (
    id           bigserial PRIMARY KEY,
    key          text NOT NULL UNIQUE,
    type         text NOT NULL,
    severity     text NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    title        text NOT NULL,
    detail       text NOT NULL DEFAULT '',
    host         text NOT NULL DEFAULT '',
    repository   text NOT NULL DEFAULT '',
    object       text NOT NULL DEFAULT '',
    source       text NOT NULL DEFAULT 'condition' CHECK (source IN ('condition', 'event')),
    state        text NOT NULL CHECK (state IN ('firing', 'acknowledged', 'silenced', 'resolved')),
    episode      integer NOT NULL DEFAULT 1,    -- incremented each time a resolved alert fires again
    first_seen   timestamptz NOT NULL,          -- start of the current episode
    last_seen    timestamptz NOT NULL,
    resolved_at  timestamptz,
    -- resolved without telling anyone (an emitted event that expired, a disabled condition)
    resolved_silently boolean NOT NULL DEFAULT false,
    count        integer NOT NULL DEFAULT 1,    -- observations (events) / episodes (conditions)
    flapping     boolean NOT NULL DEFAULT false,
    last_change  timestamptz NOT NULL,          -- last fire/resolve transition
    acked_by     text NOT NULL DEFAULT '',
    acked_at     timestamptz,
    ack_comment  text NOT NULL DEFAULT '',
    silence_id   bigint
);
CREATE INDEX notify_alerts_state ON notify_alerts (state, last_seen DESC);

-- Fire/resolve/ack/silence history (flap detection and the UI's timeline).
CREATE TABLE notify_alert_events (
    id       bigserial PRIMARY KEY,
    alert_id bigint NOT NULL REFERENCES notify_alerts(id) ON DELETE CASCADE,
    at       timestamptz NOT NULL,
    kind     text NOT NULL,   -- fired, resolved, escalated, acknowledged, unacknowledged, silenced, unsilenced, flapping, stable, expired
    detail   text NOT NULL DEFAULT ''
);
CREATE INDEX notify_alert_events_alert ON notify_alert_events (alert_id, at DESC);

-- What a rule has told its channels about an alert (reminders, resolved, escalation).
CREATE TABLE notify_rule_state (
    alert_id          bigint NOT NULL REFERENCES notify_alerts(id) ON DELETE CASCADE,
    rule_id           uuid NOT NULL REFERENCES notify_rules(id) ON DELETE CASCADE,
    fired_episode     integer NOT NULL DEFAULT 0,  -- last episode announced as firing
    resolved_episode  integer NOT NULL DEFAULT 0,  -- last episode announced as resolved
    last_sent_at      timestamptz,
    sent_severity     text NOT NULL DEFAULT '',
    seq               integer NOT NULL DEFAULT 0,  -- notifications sent for this alert by this rule
    PRIMARY KEY (alert_id, rule_id)
);

CREATE TABLE notify_silences (
    id          bigserial PRIMARY KEY,
    type_glob   text NOT NULL DEFAULT '',
    key_glob    text NOT NULL DEFAULT '',
    host_glob   text NOT NULL DEFAULT '',
    repo_glob   text NOT NULL DEFAULT '',
    object_glob text NOT NULL DEFAULT '',
    starts_at   timestamptz NOT NULL,
    ends_at     timestamptz NOT NULL,
    reason      text NOT NULL,
    author      text NOT NULL,
    created     timestamptz NOT NULL
);

CREATE TABLE notify_deliveries (
    id              bigserial PRIMARY KEY,
    channel_id      uuid NOT NULL REFERENCES notify_channels(id) ON DELETE CASCADE,
    rule_id         uuid REFERENCES notify_rules(id) ON DELETE SET NULL,
    alert_id        bigint REFERENCES notify_alerts(id) ON DELETE SET NULL,
    kind            text NOT NULL CHECK (kind IN ('firing', 'reminder', 'escalation', 'resolved', 'digest', 'test')),
    -- structural duplicate guard: the same (alert, rule, channel, notification number) is enqueued once
    dedupe_key      text NOT NULL UNIQUE,
    severity        text NOT NULL DEFAULT 'info',
    subject         text NOT NULL,
    body_text       text NOT NULL,
    body_html       text NOT NULL DEFAULT '',
    link            text NOT NULL DEFAULT '',
    payload         jsonb NOT NULL DEFAULT '{}',   -- structured alert snapshot (webhook/JSON channels)
    state           text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'sending', 'sent', 'dead')),
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL,
    claimed_until   timestamptz,
    last_error      text NOT NULL DEFAULT '',
    created         timestamptz NOT NULL,
    sent_at         timestamptz
);
CREATE INDEX notify_deliveries_due ON notify_deliveries (state, next_attempt_at);
CREATE INDEX notify_deliveries_channel_sent ON notify_deliveries (channel_id, sent_at);

-- Repository probe results (repository_unreachable), recorded with the service clock.
CREATE TABLE notify_probe_state (
    repository_id        uuid PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    last_probe_at        timestamptz NOT NULL,
    ok                   boolean NOT NULL,
    consecutive_failures integer NOT NULL DEFAULT 0,
    error                text NOT NULL DEFAULT ''
);

------------------------------------------------------------------------------------------------
-- 11. Host jobs, restore tests, sealed deliveries (internal/server/jobs, docs/jobs.md)
------------------------------------------------------------------------------------------------

-- Host jobs: the management plane ASKS a host to do work that needs the payload key (restore tests,
-- restores, sealed single-file deliveries, deep verification, an immediate backup). The server never
-- holds a key: it queues a job, the host pulls it over mutual TLS, executes it with its own key and
-- reports progress and a result. A host only ever sees its own jobs.
CREATE TABLE host_jobs (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq              bigserial NOT NULL,                 -- creation order (FIFO among equal timestamps)
    repository_id    uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    host_name        text NOT NULL,                      -- target host (hosts.name, backup scope)
    type             text NOT NULL CHECK (type IN ('backup_now', 'restore_test', 'restore_instance', 'restore_volume',
                                                   'guest_files', 'fs_extract', 'deep_verify')),
    params           jsonb NOT NULL DEFAULT '{}',
    state            text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled', 'expired')),
    progress         integer NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    message          text NOT NULL DEFAULT '',
    result           jsonb,
    created_by       text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL,
    started_at       timestamptz,
    finished_at      timestamptz,
    cancel_requested boolean NOT NULL DEFAULT false,
    -- attempt fences every host write: a host that was declared dead and comes back after the job was
    -- re-queued cannot overwrite the new attempt (its attempt number is stale).
    attempt          integer NOT NULL DEFAULT 0,
    max_attempts     integer NOT NULL DEFAULT 1 CHECK (max_attempts >= 1),
    -- running jobs: renewed by every progress report; past it the job is failed (and maybe re-queued).
    lease_until      timestamptz,
    -- queued jobs no host picked up by then expire.
    queue_until      timestamptz NOT NULL,
    bytes_done       bigint NOT NULL DEFAULT 0,
    bytes_total      bigint NOT NULL DEFAULT 0
);
CREATE INDEX host_jobs_queue ON host_jobs (repository_id, host_name, created_at, seq) WHERE state = 'queued';
CREATE INDEX host_jobs_running ON host_jobs (lease_until) WHERE state = 'running';
CREATE INDEX host_jobs_repo_time ON host_jobs (repository_id, created_at DESC);

-- Log lines a host reported for a job (capped per job by the service).
CREATE TABLE host_job_log (
    job_id uuid NOT NULL REFERENCES host_jobs(id) ON DELETE CASCADE,
    seq    integer NOT NULL,
    at     timestamptz NOT NULL,
    level  text NOT NULL DEFAULT 'info' CHECK (level IN ('debug', 'info', 'warn', 'error')),
    line   text NOT NULL,
    PRIMARY KEY (job_id, seq)
);

-- Restore tests (scheduled by the host daemon, or requested as a job). "A backup that was never
-- restored is not a backup": this is the evidence, per object.
CREATE TABLE restore_tests (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq           bigserial NOT NULL,     -- insertion order (newest first among equal start times)
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    host_name     text NOT NULL,
    object        text NOT NULL,          -- "<project>/<logical name>", e.g. "default/instance/vm1/root"
    snapshot_id   text NOT NULL DEFAULT '',
    method        text NOT NULL CHECK (method IN ('deep', 'extract', 'full')),
    state         text NOT NULL CHECK (state IN ('passed', 'failed', 'running')),
    started       timestamptz NOT NULL,
    finished      timestamptz,
    duration_ms   bigint NOT NULL DEFAULT 0,
    bytes         bigint NOT NULL DEFAULT 0,
    files         bigint NOT NULL DEFAULT 0,
    details       jsonb NOT NULL DEFAULT '{}',
    job_id        uuid REFERENCES host_jobs(id) ON DELETE SET NULL
);
CREATE INDEX restore_tests_object ON restore_tests (repository_id, object, started DESC);
CREATE UNIQUE INDEX restore_tests_one_per_job ON restore_tests (job_id) WHERE job_id IS NOT NULL;

-- Sealed files a host uploaded for a job (encrypted to a recipient key the server does not have).
-- The bytes live in the repository datastore under deliveries/<job>/<attempt>/ (outside chunks/ and
-- groups/, so GC, verify and reconcile never look at them) and are deleted after expires_at.
CREATE TABLE job_deliveries (
    job_id      uuid PRIMARY KEY REFERENCES host_jobs(id) ON DELETE CASCADE,
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    size        bigint NOT NULL,
    sha256      text NOT NULL,
    parts       integer NOT NULL,
    storage_key text NOT NULL,            -- key prefix in the datastore, "deliveries/<job>/<attempt>/"
    created_at  timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL
);
CREATE INDEX job_deliveries_expiry ON job_deliveries (expires_at);

------------------------------------------------------------------------------------------------
-- 12. Traffic control (docs/traffic.md)
------------------------------------------------------------------------------------------------

-- Upload and download rate limits of the agent API per host or shared by a set of hosts, optionally
-- only inside weekly time windows of an IANA time zone.
CREATE TABLE traffic_rules (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    enabled     boolean NOT NULL DEFAULT true,
    scope       text NOT NULL CHECK (scope IN ('host', 'global')),  -- host: a bucket per matching host; global: one shared bucket
    hosts       text NOT NULL DEFAULT '*',                          -- comma-separated globs on the host name (or "repository/host")
    direction   text NOT NULL CHECK (direction IN ('upload', 'download')),
    rate_bps    bigint NOT NULL CHECK (rate_bps >= 65536),          -- bytes per second (64 KiB/s: an 80 MiB blob fits the hosts' 30 min request timeout)
    burst_bytes bigint NOT NULL DEFAULT 0 CHECK (burst_bytes >= 0), -- 0 = one second of rate
    schedule    text NOT NULL DEFAULT '',                           -- '' = always; e.g. 'mon-fri 08:00-18:00'
    timezone    text NOT NULL DEFAULT 'UTC',
    comment     text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

------------------------------------------------------------------------------------------------
-- 13. Performance indexes
------------------------------------------------------------------------------------------------

-- Indexes for the control-plane queries that scanned whole tables (measured in docs/performance.md,
-- reproduced by BenchmarkControlPlaneQueries in internal/server/service/perf_pg_test.go).

-- Snapshot count / existence per storage object (GET /v1/objects, admin object list, status overview)
-- was a sequential scan of snapshots for EVERY object; also serves ON DELETE SET NULL from storage_objects.
CREATE INDEX snapshots_object ON snapshots (object_id);

-- Objects of a host (status overview) and ON DELETE SET NULL when a host is removed.
CREATE INDEX storage_objects_host ON storage_objects (host_id);

-- Runs of the last 24 h per repository (status overview).
CREATE INDEX backup_runs_repo_time ON backup_runs (repository_id, started_at);
-- Latest run of a host (status overview) and ON DELETE SET NULL when a host is removed.
CREATE INDEX backup_runs_host_time ON backup_runs (host_id, started_at DESC);

-- Recent maintenance runs of a repository regardless of kind (admin history list).
CREATE INDEX maintenance_runs_time ON maintenance_runs (repository_id, started_at DESC);

-- Session history grows with every backup; ON DELETE SET NULL when a host is removed.
CREATE INDEX sessions_host ON sessions (host_id);

------------------------------------------------------------------------------------------------
-- 14. Passkeys / security keys (WebAuthn) and single sign-on (OIDC)
------------------------------------------------------------------------------------------------

-- docs/authentication.md. Both methods end in the same user_sessions row as a password sign-in;
-- roles, repository scopes and the audit log are those of section 8.

-- One row per registered authenticator: everything go-webauthn needs to rebuild webauthn.Credential
-- (public key, flags, AAGUID, signature counter, transports). The attestation statement is not kept
-- (attestation is not verified: conveyance 'none').
CREATE TABLE webauthn_credentials (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id      bytea NOT NULL UNIQUE,
    public_key         bytea NOT NULL,                     -- COSE_Key
    attestation_type   text NOT NULL DEFAULT '',
    attestation_format text NOT NULL DEFAULT '',
    aaguid             bytea NOT NULL DEFAULT '\x',
    sign_count         bigint NOT NULL DEFAULT 0,          -- last signature counter seen (clone detection)
    clone_warning      boolean NOT NULL DEFAULT false,     -- a counter went backwards once: the key is refused
    attachment         text NOT NULL DEFAULT '',
    transports         text[] NOT NULL DEFAULT '{}',
    user_present       boolean NOT NULL DEFAULT false,
    user_verified      boolean NOT NULL DEFAULT false,
    backup_eligible    boolean NOT NULL DEFAULT false,
    backup_state       boolean NOT NULL DEFAULT false,
    discoverable       boolean,                            -- credProps.rk as reported by the browser (NULL = unknown)
    name               text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    last_used_at       timestamptz
);
CREATE INDEX webauthn_credentials_user ON webauthn_credentials (user_id);

-- Ceremony state between begin and finish (go-webauthn SessionData as JSON). Single use (finish
-- deletes the row), five minutes. kind: 'register' (user_id = who registers), 'second_factor' (user_id =
-- whose password was just verified), 'passkey' (no user: discoverable sign-in), 'pending_2fa' (a
-- verified password waiting for its second factor; no WebAuthn data).
CREATE TABLE auth_ceremonies (
    id_hash     bytea PRIMARY KEY,                  -- SHA-256 of the id handed to the browser
    kind        text NOT NULL CHECK (kind IN ('register','second_factor','passkey','pending_2fa')),
    user_id     uuid REFERENCES users(id) ON DELETE CASCADE,
    data        jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);
CREATE INDEX auth_ceremonies_expiry ON auth_ceremonies (expires_at);

-- OpenID Connect identity providers. The client secret is sealed in credentials (kind 'oidc') and
-- never returned by any API. Roles and repository scopes of the users who sign in through a provider
-- are computed from the ID token's claims at EVERY sign-in:
--   role  = the highest of default_role and every role_mappings entry whose group is in the groups claim;
--   scope = when scope_mappings is not empty (and the role is not admin), the user is restricted to the
--           repositories of the matching entries (none matching: no repository at all).
-- default_role can never be admin (a CHECK); admin needs an explicit group mapping.
CREATE TABLE oidc_providers (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name            text NOT NULL UNIQUE,              -- URL slug: /api/v1/auth/oidc/{name}/login
    display_name    text NOT NULL DEFAULT '',          -- button label on the sign-in page
    issuer          text NOT NULL,
    client_id       text NOT NULL,
    secret_id       uuid REFERENCES credentials(id),   -- NULL: public client (PKCE only)
    scopes          text[] NOT NULL DEFAULT '{openid,email,profile}',
    allowed_domains text[] NOT NULL DEFAULT '{}',      -- e-mail domains allowed to sign in ({} = any)
    groups_claim    text NOT NULL DEFAULT 'groups',
    default_role    text NOT NULL DEFAULT 'none' CHECK (default_role IN ('operator','restore','readonly','none')),
    role_mappings   jsonb NOT NULL DEFAULT '[]',       -- [{"group": "...", "role": "admin|operator|restore|readonly"}]
    scope_mappings  jsonb NOT NULL DEFAULT '[]',       -- [{"group": "...", "repository": "...", "role": "..."}]
    auto_create     boolean NOT NULL DEFAULT false,
    enabled         boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- A user's identity at a provider: (provider, subject), never the e-mail address alone. A row with
-- subject NULL is an administrator's explicit link by e-mail that the first sign-in with that verified
-- address binds to its subject (once); afterwards only the subject matches.
CREATE TABLE user_identities (
    provider_id   uuid NOT NULL REFERENCES oidc_providers(id) ON DELETE CASCADE,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    subject       text,
    email         text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,
    PRIMARY KEY (provider_id, user_id)
);
CREATE UNIQUE INDEX user_identities_subject ON user_identities (provider_id, subject) WHERE subject IS NOT NULL;
CREATE UNIQUE INDEX user_identities_pending ON user_identities (provider_id, lower(email)) WHERE subject IS NULL;

-- An authorization request in flight: state (hashed), nonce and PKCE verifier, single use, ten minutes.
-- browser_hash binds it to the browser that started it (a cookie), so nobody can complete someone
-- else's sign-in in a victim's browser.
CREATE TABLE oidc_logins (
    state_hash    bytea PRIMARY KEY,
    provider_id   uuid NOT NULL REFERENCES oidc_providers(id) ON DELETE CASCADE,
    browser_hash  bytea NOT NULL,
    nonce         text NOT NULL,
    verifier      text NOT NULL,
    next          text NOT NULL DEFAULT '/',
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL
);
CREATE INDEX oidc_logins_expiry ON oidc_logins (expires_at);
