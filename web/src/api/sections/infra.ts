// API of the "Infrastructure & Overview" section: hosts, objects, capacity, plus the real shapes of
// the job, alert and certificate routes these pages read (the Draft* types in types.ts predate the Go
// code; these follow internal/server/jobs, internal/server/notify and internal/server/service/certs.go).
// New routes (docs/api-infra.md): GET /runs/backups, GET /objects/stats, GET/DELETE /onboarding/pending.
import { http, seg } from '../client'
import type { AuditEntry, CapacityView, CapacityPoint, HostRow, ObjectRow, RFC3339 } from '../types'

type Sig = AbortSignal | undefined
const R = (signal?: Sig) => ({ repo: true, signal })

// ---- new read models (internal/server/service/infra_views.go) -------------------------------

export type Freshness = 'ok' | 'stale' | 'never' | 'unscheduled' | 'reference'

/** service.SnapStat: the newest snapshot of an object. */
export interface SnapStat {
  id: string
  created: RFC3339
  logical_bytes: number
  duration_ms: number
  verify_state: '' | 'ok' | 'failed'
}

/** service.ObjectStat (GET /objects/stats). */
export interface ObjectStat {
  group_id: string
  host: string
  project: string
  name: string
  kind: 'block' | 'filesystem' | 'instance' | string
  declared_policy: string
  retention_policy: string
  /** The policy whose schedule judged freshness ("" = none known). */
  policy: string
  schedule: string
  notes: string
  first_seen: RFC3339
  snapshots: number
  protected: number
  failed_verify: number
  latest: SnapStat | null
  freshness: Freshness
  due: RFC3339 | null
  overdue_at: RFC3339 | null
  /** Sum of what the host reported as newly uploaded for the existing snapshots (approximate). */
  uploaded_bytes: number
  /** Logical size of the newest snapshots (at most 30), oldest first. */
  history: Array<{ at: RFC3339; logical_bytes: number }>
}

/** service.BackupRunRow (GET /runs/backups). */
export interface BackupRun {
  id: string
  host: string
  started: RFC3339
  finished: RFC3339 | null
  status: 'running' | 'ok' | 'partial' | 'failed' | string
  objects_total: number
  objects_ok: number
  objects_failed: number
  logical_bytes: number
  uploaded_bytes: number
  error: string
}

/** service.PendingEnrolment (GET /onboarding/pending). */
export interface PendingEnrolment {
  id: string
  host: string
  scope: string
  created: RFC3339
  expires: RFC3339
  /** An identity with this name and scope exists: using the token revokes its earlier certificates. */
  replaces: boolean
}

// ---- real shapes of shipped routes ----------------------------------------------------------

export type JobState = 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled' | 'expired'
export type JobType = 'backup_now' | 'restore_test' | 'deep_verify' | 'restore_instance' | 'restore_volume' | 'guest_files' | 'fs_extract'

/** jobs.Job (internal/server/jobs/jobs.go). */
export interface Job {
  id: string
  repository: string
  host: string
  type: JobType | string
  params: Record<string, unknown> | null
  state: JobState | string
  progress: number
  bytes_done: number
  bytes_total: number
  message: string
  result?: unknown
  created_by: string
  created_at: RFC3339
  started_at?: RFC3339
  finished_at?: RFC3339
  cancel_requested: boolean
  attempt: number
  max_attempts: number
  permission: string
}

export interface JobRequest {
  type: JobType
  host: string
  repository: string
  params: Record<string, unknown>
}

/** notify.Alert (internal/server/notify/alerts.go). */
export interface Alert {
  id: number
  key: string
  type: string
  severity: 'info' | 'warning' | 'critical' | string
  title: string
  detail: string
  host: string
  repository: string
  object: string
  source: string
  state: 'firing' | 'acknowledged' | 'silenced' | 'resolved' | string
  first_seen: RFC3339
  last_seen: RFC3339
  resolved_at: RFC3339 | null
  count: number
  flapping: boolean
  acked_by: string
  acked_at: RFC3339 | null
}

/** service.CertSummary. */
export interface CertSummary {
  subject?: string
  fingerprint: string
  not_before: RFC3339
  not_after: RFC3339
  days_left: number
  /** ok | expiring | expired (hosts also: untrusted). */
  state: string
  level: 'ok' | 'warn' | 'critical' | string
}

/** service.HostCertStatus. */
export interface HostCertStatus extends CertSummary {
  repository: string
  name: string
  scope: string
  disabled: boolean
  last_renewed: RFC3339 | null
  last_seen: RFC3339 | null
  other_valid: number
  on_old_ca: boolean
  untrusted: boolean
}

/** GET /certificates: {report: service.CertReport, web?: webtls.Info}. */
export interface CertificatesResponse {
  report: {
    generated: RFC3339
    ca: CertSummary
    server: CertSummary
    hosts: HostCertStatus[] | null
    level: string
  }
  web?: unknown
}

export const infraApi = {
  objectStats: (host?: string, signal?: Sig) => http.get<ObjectStat[]>('/objects/stats', { ...R(signal), query: { host } }),
  objectStatsOf: (repository: string, signal?: Sig) => http.get<ObjectStat[]>('/objects/stats', { repo: repository, signal }),
  backupRuns: (q: { host?: string; status?: string; before?: string; limit?: number } = {}, signal?: Sig) =>
    http.get<BackupRun[]>('/runs/backups', { ...R(signal), query: q }),
  pending: (signal?: Sig) => http.get<PendingEnrolment[]>('/onboarding/pending', R(signal)),
  revokePending: (id: string) => http.del<void>(`/onboarding/pending/${seg(id)}`, undefined, R()),

  /** GET /capacity of a named repository. NOTE: every call takes a capacity sample; call once per view. */
  capacityOf: (repository: string, signal?: Sig) => http.get<CapacityView>('/capacity', { repo: repository, signal }),
  capacitySeriesOf: (repository: string, days: number, signal?: Sig) => http.get<CapacityPoint[] | null>('/capacity/series', { repo: repository, signal, query: { days } }),
  hostsOf: (repository: string, signal?: Sig) => http.get<HostRow[] | null>('/hosts', { repo: repository, signal }),
  setHostEnabled: (repository: string, name: string, scope: string, enabled: boolean) =>
    http.post<void>(`/hosts/${seg(name)}/${seg(scope)}/${enabled ? 'enable' : 'disable'}`, undefined, { repo: repository }),
  objectsOf: (repository: string, signal?: Sig) => http.get<ObjectRow[] | null>('/objects', { repo: repository, signal }),

  jobs: (q: { host?: string; repository?: string; state?: string; type?: string; limit?: number } = {}, signal?: Sig) =>
    http.get<Job[] | null>('/jobs', { query: q, signal }),
  createJob: (b: JobRequest) => http.post<Job>('/jobs', b),
  cancelJob: (id: string) => http.post<Job>(`/jobs/${seg(id)}/cancel`),

  alerts: (q: { host?: string; state?: string; active?: 1; severity?: string; limit?: number } = {}, signal?: Sig) =>
    http.get<Alert[] | null>('/alerts', { query: q, signal }),
  certificates: (signal?: Sig) => http.get<CertificatesResponse>('/certificates', { signal }),
  audit: (q: { limit?: number; before?: number } = {}, signal?: Sig) => http.get<AuditEntry[] | null>('/audit', { query: q, signal }),
}
