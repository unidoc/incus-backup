// Operations: server tasks, repository retirement, object deletion, server status, the daemon log,
// support reports and the update notice. Sources: internal/server/tasks, internal/server/mgmt
// routes_tasks.go, routes_system.go, routes_lifecycle.go, internal/server/sysinfo,
// internal/server/logring, internal/server/updatecheck (docs/api-ops.md).
import { http, request, seg } from '../client'
import type { RFC3339, Role, VerifyRequest } from '../types'
import { atLeast, roleFor } from '../perms'

type Sig = AbortSignal | undefined

// ---- wire types (exact Go JSON names) ----------------------------------------------------------

/** tasks.Task */
export interface Task {
  id: string
  kind: string
  repository: string
  started_by: string
  actor_kind: 'user' | 'token' | 'local' | 'system'
  state: 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled'
  progress: number
  message: string
  params: unknown
  result: unknown
  error: string
  exclusive: boolean
  cancel_requested: boolean
  log_lines: number
  log_dropped: number
  created: RFC3339
  started: RFC3339 | null
  finished: RFC3339 | null
  duration_ms: number
}

/** tasks.LogLine */
export interface TaskLogLine { seq: number; at: RFC3339; level: 'debug' | 'info' | 'warn' | 'error'; line: string }

/** tasks.Detail (GET /tasks/{id}) */
export interface TaskDetail extends Task { log: TaskLogLine[]; next_seq: number }

/** 202 answer of every task-starting route. */
export interface TaskAccepted { task: Task }

export interface TaskQuery { state?: string; kind?: string; repository?: string; started_by?: string; limit?: number; before?: number }

/** sysinfo.Disk */
export interface Disk { path: string; label: string; total_bytes: number; used_bytes: number; free_bytes: number; error?: string }

/** mgmt.SystemStatus */
export interface SystemStatus {
  version: string
  go_version: string
  server_time: RFC3339
  started: RFC3339
  uptime_seconds: number
  instance_name: string
  host: {
    hostname: string; kernel: string; os: string; uptime_seconds: number; load1: number; load5: number; load15: number; cpus: number
    cpu_percent: number; mem_total_bytes: number; mem_available_bytes: number; mem_used_bytes: number; swap_total_bytes: number; swap_free_bytes: number
  }
  process: { pid: number; rss_bytes: number; open_fds: number; goroutines: number; heap_bytes: number; heap_sys_bytes: number; gc_cycles: number; gc_pause_total_seconds: number; go_version: string }
  state_dir: Disk
  datastores: Disk[]
  database: {
    ok: boolean; error?: string; version: string; size_bytes: number; connections: number; max_connections: number
    pool_total: number; pool_idle: number; pool_in_use: number; pool_max: number; migrations: string[]; latency_ms: number
  }
  listeners: Array<{ name: string; addr: string; detail?: string }>
  update?: UpdateStatus
}

/** sysinfo.Sample (GET /system/samples) */
export interface SystemSample {
  at: RFC3339; cpu_percent: number; load1: number; mem_used_bytes: number; mem_total_bytes: number
  disk_used_bytes: number; disk_total_bytes: number; db_bytes: number; process_rss_bytes: number; goroutines: number
}

/** logring.Line */
export interface LogLine { seq: number; time: RFC3339; level: string; message: string; attrs: string }
/** logring.Result (+ note when the process keeps no log) */
export interface LogResult { lines: LogLine[]; last_seq: number; capacity: number; dropped: number; note?: string }

/** updatecheck.Status */
export interface UpdateStatus {
  enabled: boolean; url: string; current: string; latest?: string; update_available: boolean
  checked_at?: RFC3339; error?: string; release_url?: string
}

/** service.DetachResult / DestroyResult / GroupDeleteResult */
export interface DetachResult { repository: string; location: string; owner_cleared: boolean }
export interface DestroyResult { repository: string; location: string; objects_deleted: number; bytes_deleted: number }
export interface GroupDeleteResult { group: string; object: string; snapshots_deleted: number; protected_overruled: number }

export interface RemoveRepoRequest { mode: 'detach' | 'destroy'; confirm?: string; force_hosts?: string[]; clear_owner?: boolean }

// ---- roles (the routes are registered in init() of routes_tasks/system/lifecycle.go) ---------

export const OPS_ROUTE_ROLES: Record<string, Role> = {
  'GET /tasks': 'readonly',
  'GET /tasks/{id}': 'readonly',
  'POST /tasks/{id}/cancel': 'operator',
  'GET /metrics': 'readonly',
  'GET /system/status': 'readonly',
  'GET /system/samples': 'readonly',
  'GET /system/logs': 'admin',
  'POST /system/support-report': 'admin',
  'GET /system/update': 'readonly',
  'POST /system/update/check': 'admin',
  'DELETE /repositories/{name}': 'admin',
  'DELETE /objects/{group}': 'admin',
}

/** can() that also knows the routes of this section. */
export function canOps(role: Role | '' | undefined | null, route: string): boolean {
  return atLeast(role, OPS_ROUTE_ROLES[route] ?? roleFor(route))
}

// ---- calls -------------------------------------------------------------------------------------

export const opsApi = {
  tasks: {
    list: (q: TaskQuery = {}, signal?: Sig) => http.get<Task[] | null>('/tasks', { signal, query: { ...q } }),
    get: (id: string, after = 0, signal?: Sig) => http.get<TaskDetail>(`/tasks/${seg(id)}`, { signal, query: { after, limit: 2000 } }),
    cancel: (id: string) => http.post<Task>(`/tasks/${seg(id)}/cancel`),
  },
  /** Maintenance on one repository as tasks (202): the answer is the task to follow. */
  start: {
    prune: (repo: string) => http.post<TaskAccepted>('/prune', undefined, { repo }),
    gc: (repo: string, dryRun: boolean, graceHours?: number) => http.post<TaskAccepted>('/gc', undefined, { repo, query: { dry_run: dryRun ? 1 : undefined, grace_hours: graceHours } }),
    verify: (repo: string, b: VerifyRequest) => http.post<TaskAccepted>('/verify', b, { repo }),
    reconcile: (repo: string) => http.post<TaskAccepted>('/reconcile', undefined, { repo }),
    sync: (repo: string, name: string) => http.post<TaskAccepted>(`/sync/${seg(name)}/run`, undefined, { repo }),
  },
  /** detach answers 200 with the result; destroy answers 202 with a task. */
  removeRepository: (name: string, b: RemoveRepoRequest) => http.del<DetachResult | TaskAccepted>(`/repositories/${seg(name)}`, b),
  deleteObject: (repo: string, group: string, forceProtected: boolean) =>
    http.del<TaskAccepted>(`/objects/${seg(group)}`, { force_protected: forceProtected || undefined }, { repo }),
  system: {
    status: (signal?: Sig) => http.get<SystemStatus>('/system/status', { signal }),
    samples: (hours = 24, points = 300, signal?: Sig) => http.get<SystemSample[] | null>('/system/samples', { signal, query: { hours, points } }),
    logs: (q: { level?: string; q?: string; after?: number; limit?: number } = {}, signal?: Sig) => http.get<LogResult>('/system/logs', { signal, query: { ...q } }),
    supportReport: (includeHostNames: boolean) =>
      request<ArrayBuffer>('POST', '/system/support-report', { body: { include_host_names: includeHostNames || undefined }, raw: true }),
    update: (signal?: Sig) => http.get<UpdateStatus>('/system/update', { signal }),
    checkUpdate: () => http.post<{ status: UpdateStatus; error?: string }>('/system/update/check'),
  },
}

/** Saves bytes as a file in the browser. */
export function saveBytes(b: ArrayBuffer, name: string, type = 'application/gzip') {
  const url = URL.createObjectURL(new Blob([b], { type }))
  const a = document.createElement('a')
  a.href = url
  a.download = name
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 10_000)
}
