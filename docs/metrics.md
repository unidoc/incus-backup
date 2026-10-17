# Prometheus metrics

`incus-backup-server` exposes its state in the Prometheus text format (0.0.4), written without external
libraries (`internal/server/metrics`, collector `internal/server/mgmt/metrics_collect.go`). A scrape reads
PostgreSQL and in-memory counters only: it never lists the datastore (repository sizes come from the latest
capacity sample, taken hourly by default), so it is cheap on S3 too.

## Where

1. **Web listener, with an API token** (always available): `GET https://SERVER:8443/api/v1/metrics` with
   `Authorization: Bearer <token>`. Any role may scrape; create a `readonly` token for Prometheus
   (`Profile → API tokens`, or `POST /api/v1/me/tokens {"name":"prometheus","role":"readonly"}`).
   An API token created this way lives at most 365 days (90 days unless `ttl_hours` is given), so a scrape
   token must be re-created before it expires and the `credentials_file` replaced. Without a token: 401.
2. **A dedicated plain-HTTP listener** (optional, `server.yaml`):

   ```yaml
   metrics_listen: 127.0.0.1:9187      # serves only GET /metrics
   metrics_unauthenticated: false      # default: an API token is still required
   ```

   Set `metrics_unauthenticated: true` only when the address is private (loopback, a management VLAN):
   anyone who can connect then reads repository names, host names and sizes.

## Scrape configuration

```yaml
scrape_configs:
  - job_name: incus-backup
    scheme: https
    metrics_path: /api/v1/metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/incus-backup.token
    tls_config:
      ca_file: /etc/prometheus/incus-backup-web-ca.pem   # or insecure_skip_verify for the self-signed default
    scrape_interval: 60s
    scrape_timeout: 20s
    static_configs:
      - targets: ['backup.example.com:8443']

  # or the dedicated listener
  - job_name: incus-backup-local
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/incus-backup.token
    static_configs:
      - targets: ['127.0.0.1:9187']
```

Once a minute is plenty: most values change at backup, maintenance and sampling time.

## Metrics

All names start with `incus_backup_`. Timestamps are unix seconds; 0 means "never".

| metric | type | labels | meaning |
|---|---|---|---|
| `build_info` | gauge | `version`, `goversion` | 1 |
| `process_start_time_seconds` | gauge | | start of the server process |
| `scrape_ok` | gauge | | 1 when every source answered during this scrape |
| `repository_stored_bytes` | gauge | `repository`, `backend` | physical bytes (latest capacity sample) |
| `repository_logical_bytes` | gauge | `repository`, `backend` | logical bytes of all snapshots |
| `repository_dedup_ratio` | gauge | `repository`, `backend` | logical / stored (0 when empty) |
| `repository_chunks` | gauge | `repository`, `backend` | chunk objects |
| `repository_capacity_sample_timestamp_seconds` | gauge | `repository`, `backend` | age of the size figures |
| `repository_filesystem_size_bytes`, `_free_bytes` | gauge | `repository`, `backend` | filesystem repositories only |
| `repository_snapshots` | gauge | `repository`, `backend` | committed snapshots |
| `repository_objects` | gauge | `repository`, `backend` | storage objects |
| `repository_corrupt_chunks` | gauge | `repository`, `backend` | corrupt chunks not healed |
| `repository_failed_verifications` | gauge | `repository`, `backend` | snapshots whose last verification failed |
| `repository_mode` | gauge | `repository`, `mode` | 1 for the current mode (`online`, `read-only`, `offline`) |
| `repository_last_backup_timestamp_seconds` | gauge | `repository`, `backend` | newest snapshot commit |
| `repository_last_success_timestamp_seconds` | gauge | `repository`, `kind` | last successful `verify`, `scrub`, `gc`, `retention`, `reconcile` |
| `host_up` | gauge | `repository`, `host` | 1 when the host talked to the server in the last 15 minutes and is not disabled |
| `host_last_seen_timestamp_seconds` | gauge | `repository`, `host` | last request |
| `host_objects` | gauge | `repository`, `host` | objects of the host |
| `host_stale_objects` | gauge | `repository`, `host` | objects without a snapshot in the last 48 hours |
| `backup_runs_total` | counter | `repository`, `status` | backup runs kept in PostgreSQL by status (`ok`, `partial`, `failed`, `deferred`, `running`) |
| `tasks_running` | gauge | `kind` | server tasks queued or running |
| `alerts_firing` | gauge | `severity` | firing alerts (`info`, `warning`, `critical`) |
| `host_jobs_queued`, `host_jobs_running` | gauge | | host job queue |
| `certificate_days_left` | gauge | `certificate` | `ca`, `server`, `sync:<repository>/<job>` |
| `update_available` | gauge | `latest` | 1 when the opt-in update check found a newer release |
| `process_goroutines`, `process_heap_bytes`, `process_heap_sys_bytes`, `process_resident_memory_bytes`, `process_open_fds` | gauge | | the server process |
| `process_gc_cycles_total`, `process_gc_pause_seconds_total` | counter | | Go GC |
| `db_pool_connections` | gauge | `state` (`in_use`, `idle`) | PostgreSQL pool |
| `db_pool_max_connections` | gauge | | pool limit |
| `db_pool_acquires_total`, `db_pool_empty_acquires_total`, `db_pool_acquire_wait_seconds_total` | counter | | pool pressure |
| `db_size_bytes` | gauge | | size of the database |
| `http_requests_total` | counter | `route`, `method`, `code` | management API requests, by route **pattern** (`GET /api/v1/snapshots/{group}/{snap}/verifications`), never the raw path |
| `http_request_duration_seconds` | histogram | `route`, `method` | management API latency (buckets 5 ms … 30 s) |

`backup_runs_total` counts the rows PostgreSQL keeps; it can drop when old runs are cleaned up. Use
`increase()` over short windows or alert on the alert engine's own `backup_failed` instead.

## Alert rules (examples)

The server has its own alert engine with notifications (`docs/notifications.md`); these rules are for
teams that alert from Prometheus/Alertmanager instead or in addition.

```yaml
groups:
  - name: incus-backup
    rules:
      - alert: IncusBackupDown
        expr: up{job="incus-backup"} == 0 or incus_backup_scrape_ok == 0
        for: 10m
        labels: {severity: critical}
        annotations:
          summary: "incus-backup-server {{ $labels.instance }} cannot be scraped or its database does not answer"

      - alert: IncusBackupNoRecentBackup
        expr: time() - incus_backup_repository_last_backup_timestamp_seconds > 36 * 3600
        for: 30m
        labels: {severity: warning}
        annotations:
          summary: "No snapshot committed to {{ $labels.repository }} for more than 36 h"

      - alert: IncusBackupStaleObjects
        expr: incus_backup_host_stale_objects > 0
        for: 2h
        labels: {severity: warning}
        annotations:
          summary: "{{ $value }} object(s) of {{ $labels.host }} have no snapshot in 48 h"

      - alert: IncusBackupHostDown
        expr: incus_backup_host_up == 0
        for: 2h
        labels: {severity: warning}
        annotations:
          summary: "Backup host {{ $labels.host }} has not contacted the server for 2 h"

      - alert: IncusBackupCorruption
        expr: incus_backup_repository_corrupt_chunks > 0 or incus_backup_repository_failed_verifications > 0
        labels: {severity: critical}
        annotations:
          summary: "Corrupt data in {{ $labels.repository }}: verify and re-run backups"

      - alert: IncusBackupNotOnline
        expr: incus_backup_repository_mode{mode="online"} == 0
        for: 6h
        labels: {severity: warning}
        annotations:
          summary: "{{ $labels.repository }} has been in maintenance mode for 6 h (backups are deferred)"

      - alert: IncusBackupVerifyOverdue
        expr: time() - incus_backup_repository_last_success_timestamp_seconds{kind="verify"} > 3 * 86400
        labels: {severity: warning}
        annotations:
          summary: "{{ $labels.repository }}: no successful verification for 3 days"

      - alert: IncusBackupFilesystemFilling
        expr: predict_linear(incus_backup_repository_filesystem_free_bytes[7d], 14 * 86400) < 0
        for: 1h
        labels: {severity: warning}
        annotations:
          summary: "{{ $labels.repository }} will be full within 14 days at the current growth"

      - alert: IncusBackupCertificateExpiry
        expr: incus_backup_certificate_days_left < 14
        labels: {severity: warning}
        annotations:
          summary: "Certificate {{ $labels.certificate }} expires in {{ $value }} days"

      - alert: IncusBackupCriticalAlerts
        expr: incus_backup_alerts_firing{severity="critical"} > 0
        for: 15m
        labels: {severity: critical}
        annotations:
          summary: "{{ $value }} critical alert(s) firing in the incus-backup console"

      - alert: IncusBackupAPISlow
        expr: histogram_quantile(0.95, sum by (le) (rate(incus_backup_http_request_duration_seconds_bucket[10m]))) > 2
        for: 30m
        labels: {severity: info}
        annotations:
          summary: "95th percentile management API latency above 2 s"
```

## Tests

`internal/server/metrics/metrics_test.go` checks the writer (label escaping, histogram buckets) with the
strict parser `internal/server/metrics/promtest` (contiguous families, HELP/TYPE once, escaping, cumulative
buckets with `+Inf`, `_count` equal to `+Inf`, no duplicate series), which itself is tested against broken
expositions. `internal/server/mgmt/ops2_test.go` scrapes a real server (readonly token, anonymous 401, the
dedicated listener with and without authentication), parses the result and compares values with the API.
