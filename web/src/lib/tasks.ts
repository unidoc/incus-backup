// Pure helpers for server tasks (internal/server/tasks) and the daemon log viewer.
import type { Role } from '@/api/types'
import { formatBytes, formatNumber, plural } from './format'

export type TaskState = 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled'

export const TASK_KINDS: Record<string, string> = {
  gc: 'Garbage collection',
  verify: 'Verify',
  scrub: 'Full verify (scrub)',
  retention: 'Retention',
  reconcile: 'Reconcile',
  sync: 'Sync run',
  capacity: 'Capacity sample',
  probe: 'Repository probe',
  repository_destroy: 'Destroy repository',
  group_delete: 'Delete object',
}
export const taskKindLabel = (k: string) => TASK_KINDS[k] ?? k

export const isActive = (t: { state: string }) => t.state === 'queued' || t.state === 'running'

/** Role needed to cancel a task of a kind: the role that may start it (mirror of taskCancelPerm in routes_tasks.go). */
const CANCEL_ROLE: Record<string, Role> = {
  verify: 'operator', scrub: 'operator', sync: 'operator', probe: 'operator', capacity: 'operator',
  gc: 'admin', retention: 'admin', reconcile: 'admin', repository_destroy: 'admin', group_delete: 'admin',
}
export const cancelRole = (kind: string): Role => CANCEL_ROLE[kind] ?? 'admin'

/** Who started a task, for people: "system" (the scheduler), "this server's CLI", a user or token. */
export function startedByLabel(t: { started_by: string; actor_kind: string }): string {
  switch (t.actor_kind) {
    case 'system': return 'scheduler'
    case 'local': return 'server CLI'
    default: return t.started_by || '–'
  }
}

type Obj = Record<string, unknown>
const num = (o: Obj, k: string) => (typeof o[k] === 'number' ? (o[k] as number) : 0)

/** One line saying what a finished task produced (the result JSON differs per kind). */
export function taskResultSummary(kind: string, result: unknown): string {
  if (!result || typeof result !== 'object') return ''
  const r = result as Obj
  switch (kind) {
    case 'gc': {
      const verb = r.dry_run ? 'would remove' : 'removed'
      return `${verb} ${plural(num(r, 'removed'), 'chunk')} (${formatBytes(num(r, 'bytes_reclaimed'))}) of ${formatNumber(num(r, 'scanned'))} scanned`
    }
    case 'retention': return `${plural(num(r, 'removed'), 'snapshot')} removed, ${formatNumber(num(r, 'kept'))} kept in ${plural(num(r, 'groups'), 'object')}`
    case 'verify': case 'scrub': {
      const s = (r.stats ?? {}) as Obj
      const probs = Array.isArray(s.problems) ? s.problems.length : 0
      return `${r.ok === false ? `${plural(probs, 'problem')} found` : 'no problems'}: ${plural(num(s, 'snapshots'), 'snapshot')}, ${plural(num(s, 'chunks'), 'chunk')} read`
    }
    case 'reconcile': return `${plural(num(r, 'inserted'), 'record')} added, ${formatNumber(num(r, 'removed'))} removed`
    case 'sync': {
      const x = (r.result ?? r) as Obj
      return `${plural(num(x, 'synced'), 'snapshot')} synced, ${formatBytes(num(x, 'bytes_transferred'))} transferred`
    }
    case 'repository_destroy': return `${formatNumber(num(r, 'objects_deleted'))} objects (${formatBytes(num(r, 'bytes_deleted'))}) deleted`
    case 'group_delete': return `${plural(num(r, 'snapshots_deleted'), 'snapshot')} deleted${num(r, 'protected_overruled') ? `, ${num(r, 'protected_overruled')} protected overruled` : ''}`
    case 'probe': return Array.isArray(result) ? `${(result as Obj[]).filter((x) => x.ok).length} of ${(result as unknown[]).length} checks passed` : ''
  }
  return ''
}

export interface Seq { seq: number }
/** Appends newly fetched log lines, dropping duplicates (overlapping polls) and keeping the last `max`. */
export function mergeLog<T extends Seq>(have: readonly T[], add: readonly T[], max = 5000): T[] {
  const last = have.length ? have[have.length - 1].seq : 0
  const fresh = add.filter((l) => l.seq > last)
  if (!fresh.length) return have as T[]
  const out = [...have, ...fresh]
  return out.length > max ? out.slice(out.length - max) : out
}

/**
 * Clock skew of the server against this browser in ms (positive: the server is ahead), from a
 * response's server time and the request's start/end (the midpoint estimates when the server answered).
 * Returns 0 for an unreadable time.
 */
export function clockSkewMs(serverTime: string, sentAt: number, receivedAt: number): number {
  const t = Date.parse(serverTime)
  if (!Number.isFinite(t)) return 0
  return Math.round(t - (sentAt + receivedAt) / 2)
}

/** How bad a skew is: under 5 s fine, under 60 s a warning (TLS and TOTP tolerate it), above critical. */
export function skewStatus(ms: number): 'ok' | 'warn' | 'critical' {
  const a = Math.abs(ms)
  return a < 5_000 ? 'ok' : a < 60_000 ? 'warn' : 'critical'
}

export const LOG_LEVELS = ['DEBUG', 'INFO', 'WARN', 'ERROR'] as const
/** Status colour of a daemon or task log level. */
export function levelStatus(level: string): 'neutral' | 'info' | 'warn' | 'critical' {
  switch (level.toUpperCase()) {
    case 'ERROR': return 'critical'
    case 'WARN': case 'WARNING': return 'warn'
    case 'DEBUG': return 'neutral'
  }
  return 'info'
}
