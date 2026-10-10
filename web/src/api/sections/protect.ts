// "Protection & recovery" section: host jobs, restore tests and per-snapshot statistics. Wire types
// follow the Go JSON names exactly: internal/server/jobs (jobs.go, restoretests.go, hub.go),
// internal/jobspec (params and results) and internal/server/mgmt/routes_protect.go (snapshot stats).
import { ApiError, buildUrl, http, seg } from '../client'
import type { RFC3339 } from '../types'

type Sig = AbortSignal | undefined
const R = (signal?: Sig) => ({ repo: true, signal })

// ---- jobs ------------------------------------------------------------------------------------

export type JobType = 'backup_now' | 'restore_test' | 'deep_verify' | 'restore_instance' | 'restore_volume' | 'guest_files' | 'fs_extract'
export const JOB_TYPES: JobType[] = ['backup_now', 'restore_test', 'deep_verify', 'restore_instance', 'restore_volume', 'guest_files', 'fs_extract']
export type JobState = 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled' | 'expired'
export const JOB_STATES: JobState[] = ['queued', 'running', 'succeeded', 'failed', 'cancelled', 'expired']
export const terminal = (s: string) => s === 'succeeded' || s === 'failed' || s === 'cancelled' || s === 'expired'

/** jobs.LogEntry */
export interface JobLogLine { seq: number; at: RFC3339; level: string; line: string }

/** jobs.Delivery: a sealed file the host uploaded (opaque bytes for the server). */
export interface JobDelivery { size: number; sha256: string; created_at: RFC3339; expires_at: RFC3339 }

/** jobs.Job */
export interface Job {
  id: string
  repository: string
  host: string
  type: JobType | string
  /** Canonical params (jobspec.Validate output). */
  params: Record<string, unknown> | null
  state: JobState | string
  /** 0..100 */
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
  lease_until?: RFC3339
  permission: string
  delivery?: JobDelivery
  log?: JobLogLine[] | null
}

/** jobs.Event: the data of an SSE `job` event (no params, no results). */
export interface JobEvent {
  job_id: string
  repository: string
  host: string
  type: string
  state: string
  progress: number
  message: string
  attempt: number
  cancel_requested: boolean
  at: RFC3339
  lagged?: boolean
}

export interface CreateJobRequest { type: JobType; host: string; repository?: string; params: Record<string, unknown> }

/** jobspec.DeliveryResult */
export interface DeliveryResult { size: number; sha256: string; files: number; bytes: number; format: string; paths?: Record<string, string> }

/** jobspec.RestoreTestResult */
export interface RestoreTestResult {
  project: string; object: string; snapshot: string; method: string; passed: boolean
  bytes: number; files: number; chunks: number; duration_ms: number; details?: Record<string, unknown>
}

// ---- restore tests ---------------------------------------------------------------------------

export type RestoreTestMethod = 'deep' | 'extract' | 'full'

/** jobs.RestoreTest */
export interface RestoreTest {
  id: string
  repository: string
  host: string
  /** "<project>/<logical name>" */
  object: string
  snapshot_id: string
  method: RestoreTestMethod | string
  state: 'passed' | 'failed' | 'running' | string
  started: RFC3339
  finished?: RFC3339
  duration_ms: number
  bytes: number
  files: number
  details: Record<string, unknown> | null
  job_id?: string
}

export interface RunRestoreTestRequest { host: string; project: string; object: string; snapshot?: string; mode: RestoreTestMethod; repository?: string }

// ---- snapshot statistics (GET /snapshot-stats) -------------------------------------------------

/** service.SnapshotStats: what the backup run recorded in PostgreSQL for one snapshot. */
export interface SnapshotStats {
  group_id: string
  snapshot_id: string
  logical_bytes: number
  dirty_bytes: number
  read_bytes: number
  uploaded_bytes: number
  full_read: boolean
  duration_ms: number
  previous_snapshot_id: string
  /** client | sync | reconcile */
  source: string
  verify_state: '' | 'ok' | 'failed'
  verified_at: RFC3339 | null
}

/** A sealed delivery as a stream (never buffered: deliveries reach 4 GiB). */
export interface DeliveryStream { body: ReadableStream<Uint8Array>; size: number; sha256: string }

async function openDelivery(id: string, signal?: AbortSignal): Promise<DeliveryStream> {
  const path = `/jobs/${seg(id)}/delivery`
  let res: Response
  try {
    res = await fetch(buildUrl(path), { credentials: 'same-origin', cache: 'no-store', signal, headers: { Accept: 'application/octet-stream' } })
  } catch (e) {
    if (e instanceof DOMException && e.name === 'AbortError') throw e
    throw new ApiError(0, 'Could not reach the server. Check your connection and try again.', null, 'GET', path)
  }
  if (!res.ok || !res.body) {
    let msg = `The server answered ${res.status}.`
    try { const b = await res.json() as { error?: string }; if (b.error) msg = b.error } catch { /* not JSON */ }
    throw new ApiError(res.status, msg, null, 'GET', path)
  }
  return { body: res.body, size: Number(res.headers.get('Content-Length') ?? 0), sha256: res.headers.get('X-Content-SHA256') ?? '' }
}

export const protectApi = {
  jobs: {
    list: (q: { state?: string; host?: string; type?: string; limit?: number } = {}, signal?: Sig) =>
      http.get<Job[] | null>('/jobs', { ...R(signal), query: q }),
    get: (id: string, signal?: Sig) => http.get<Job>(`/jobs/${seg(id)}`, { signal }),
    create: (b: CreateJobRequest) => http.post<Job>('/jobs', b, R()),
    cancel: (id: string) => http.post<Job>(`/jobs/${seg(id)}/cancel`),
    delivery: openDelivery,
  },
  restoreTests: {
    list: (q: { host?: string; object?: string; state?: string; limit?: number } = {}, signal?: Sig) =>
      http.get<RestoreTest[] | null>('/restore-tests', { ...R(signal), query: q }),
    latest: (signal?: Sig) => http.get<RestoreTest[] | null>('/restore-tests', { ...R(signal), query: { latest: 1 } }),
    run: (b: RunRestoreTestRequest) => http.post<Job>('/restore-tests/run', b, R()),
  },
  snapshotStats: (group?: string, signal?: Sig) => http.get<SnapshotStats[] | null>('/snapshot-stats', { ...R(signal), query: { group } }),
}
