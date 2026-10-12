// API of the storage and protection-configuration pages (repositories, maintenance, policies, sync).
//
// Two things differ from the shared `api` object:
//  * Repository-detail calls take the repository NAME explicitly (`repo: name`), so a page about
//    repository "offsite" never acts on whatever the top-bar selector points at. Every maintenance
//    action (GC, prune, verify, reconcile, escrow) goes through these wrappers.
//  * The routes added by internal/server/mgmt/routes_storage.go (docs/api-storage.md): connection
//    checks, repository identity, effective policies, sync run history/schedule, peer check, schedule preview.
import { http, seg } from '../client'
import type * as T from '../types'
import type { Role } from '../types'
import { atLeast, roleFor } from '../perms'

type Sig = AbortSignal | undefined

// ---- wire types of the new routes (exact Go JSON names) -------------------------------------

/** service.CheckItem */
export interface CheckItem {
  id: string
  label: string
  level: 'pass' | 'warn' | 'fail' | 'skip' | 'unknown'
  detail?: string
}

/** datastore.Owner (owner.json) */
export interface OwnerRecord {
  server_id: string
  host?: string
  since: T.RFC3339
  previous?: string
}

/** service.DatastoreFacts */
export interface DatastoreFacts {
  exists: boolean
  id?: string
  created?: T.RFC3339
  owner?: OwnerRecord
  owned_by_this_server: boolean
  attached_as?: string
}

/** service.RepoCheckReport: POST /repositories/check and /repositories/{name}/check */
export interface RepoCheckReport {
  kind: string
  purpose: 'create' | 'attach' | 'existing'
  location: string
  checks: CheckItem[]
  datastore: DatastoreFacts
  server_id: string
  ready: boolean
  needs_adopt: boolean
  free_bytes?: number
  total_bytes?: number
}

/** service.RepoCheckRequest (same fields as CreateRepoRequest plus attach). */
export interface RepoCheckRequest extends T.CreateRepoRequest {
  attach?: boolean
}

/** service.RepoInfo: GET /repositories/{name}/info */
export interface RepoInfo {
  name: string
  backend: 'filesystem' | 's3'
  location: string
  domain_id: string
  datastore_id: string
  mode: T.RepoMode
  mode_reason?: string
  server_id: string
  owner: OwnerRecord | null
  owner_error?: string
  datastore_created?: T.RFC3339
}

/** service.EffectivePolicy: GET /policies/effective */
export interface EffectivePolicy {
  group_id: string
  host: string
  project: string
  name: string
  kind: string
  owner: string
  declared_policy: string
  pinned_policy: string
  policy: string
  source: 'object' | 'project' | 'host' | 'pinned' | 'none'
  assignment?: { project: string; object: string; policy: string }
  policy_known: boolean
  snapshots: number
}

/** syncer.RunRecord: GET /sync/{name}/runs */
export interface SyncRunRecord {
  id: string
  started_at: T.RFC3339
  finished_at: T.RFC3339 | null
  status: 'running' | 'ok' | 'partial' | 'failed' | string
  snapshots_pending: number
  snapshots_synced: number
  bytes_transferred: number
  chunks_transferred: number
  chunks_reused: number
  snapshots_skipped: number
  snapshots_repaired: number
  error: string
}

/** syncer.RemoteCheckResult: POST /sync/check-remote */
export interface RemoteCheckResult {
  checks: CheckItem[]
  ok: boolean
  address: string
  ca_fingerprint?: string
}

/** mgmt.SchedulePreview: GET /schedule/preview */
export interface SchedulePreview {
  spec: string
  valid: boolean
  error?: string
  manual: boolean
  next: T.RFC3339[]
  zone: string
  due_now: boolean
}

// ---- roles of the new routes (the server's init() in routes_storage.go; perms.ts mirrors only perms.go)

export const STORAGE_ROUTE_ROLES: Record<string, Role> = {
  'POST /repositories/check': 'admin',
  'POST /repositories/{name}/check': 'operator',
  'GET /repositories/{name}/info': 'readonly',
  'GET /policies/effective': 'readonly',
  'GET /sync/{name}/runs': 'readonly',
  'PUT /sync/{name}/schedule': 'operator',
  'POST /sync/check-remote': 'admin',
  'GET /schedule/preview': 'readonly',
}

/** can() that also knows the routes of this section. */
export function canDo(role: Role | '' | undefined | null, route: string): boolean {
  return atLeast(role, STORAGE_ROUTE_ROLES[route] ?? roleFor(route))
}

// ---- calls -----------------------------------------------------------------------------------

/** Every call on a repository's own page: the repository is explicit, never the top-bar choice. */
export function repoApi(name: string) {
  const R = (signal?: Sig) => ({ repo: name, signal })
  return {
    name,
    status: (signal?: Sig) => http.get<T.Status>('/status', R(signal)),
    info: (signal?: Sig) => http.get<RepoInfo>(`/repositories/${seg(name)}/info`, { signal }),
    check: () => http.post<RepoCheckReport>(`/repositories/${seg(name)}/check`),
    probe: () => http.post<T.ProbeResult[]>(`/repositories/${seg(name)}/probe`),
    setMode: (b: T.SetModeRequest) => http.put<void>(`/repositories/${seg(name)}/mode`, b),
    hosts: (signal?: Sig) => http.get<T.HostRow[] | null>('/hosts', R(signal)),
    setHostEnabled: (host: string, scope: string, enabled: boolean) =>
      http.post<void>(`/hosts/${seg(host)}/${seg(scope)}/${enabled ? 'enable' : 'disable'}`, undefined, R()),
    maintenance: (signal?: Sig) => http.get<T.MaintenanceView>('/maintenance', R(signal)),
    putMaintenance: (b: T.MaintenanceUpdate) => http.put<void>('/maintenance', b, R()),
    runs: (limit = 100, signal?: Sig) => http.get<T.RunRow[] | null>('/runs', { ...R(signal), query: { limit } }),
    prune: (dryRun: boolean) => http.post<T.PruneResult>('/prune', undefined, { ...R(), query: { dry_run: dryRun ? 1 : undefined, wait: dryRun ? undefined : 1 } }),
    gc: (dryRun: boolean, graceHours?: number) => http.post<T.GCStats>('/gc', undefined, { ...R(), query: { dry_run: dryRun ? 1 : undefined, grace_hours: graceHours, wait: 1 } }),
    verify: (b: T.VerifyRequest) => http.post<T.VerifyResponse>('/verify', b, { ...R(), query: { wait: 1 } }),
    reconcile: () => http.post<T.ReconcileResult>('/reconcile', undefined, { ...R(), query: { wait: 1 } }),
    corrupt: (healed = false, signal?: Sig) => http.get<T.CorruptChunk[] | null>('/corrupt', { ...R(signal), query: { healed: healed ? 1 : undefined } }),
    snapshots: (signal?: Sig) => http.get<T.SnapshotRef[] | null>('/snapshots', R(signal)),
    clearEscrow: () => http.del<void>('/escrow', undefined, R()),
    sync: (signal?: Sig) => http.get<T.SyncJob[] | null>('/sync', R(signal)),
  }
}
export type RepoApi = ReturnType<typeof repoApi>

export const storageApi = {
  /** Test settings before creating/attaching (admin; the secret is sent, never returned). */
  checkSettings: (b: RepoCheckRequest) => http.post<RepoCheckReport>('/repositories/check', b),
  effectivePolicies: (signal?: Sig) => http.get<EffectivePolicy[] | null>('/policies/effective', { repo: true, signal }),
  schedulePreview: (spec: string, count = 5, after?: string, signal?: Sig) =>
    http.get<SchedulePreview>('/schedule/preview', { query: { spec, count, after }, signal }),
  sync: {
    runs: (name: string, limit = 60, signal?: Sig) => http.get<SyncRunRecord[] | null>(`/sync/${seg(name)}/runs`, { repo: true, signal, query: { limit } }),
    setSchedule: (name: string, b: { schedule?: string; enabled?: boolean }) => http.put<void>(`/sync/${seg(name)}/schedule`, b, { repo: true }),
    checkRemote: (b: { remote: string; token?: string }) => http.post<RemoteCheckResult>('/sync/check-remote', b),
  },
}
