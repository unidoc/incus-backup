// Pure logic of the infrastructure pages (hosts, objects, capacity, overview widgets). Unit-tested in
// infra.test.ts; the views only render what these functions decide.
import type { IconName } from '@/components/icons'
import type { Status } from './status'
import { worst } from './status'

// ---- kinds ----------------------------------------------------------------------------------

export interface KindInfo { label: string; icon: IconName; short: string }

/** True for custom volumes ("volume/<name>"), false for instance records and their disks. */
export const isVolume = (name: string) => name.startsWith('volume/')

/** How a storage object is shown: an instance record, an instance disk, or a custom volume. */
export function kindInfo(kind: string, name = ''): KindInfo {
  const vol = isVolume(name)
  switch (kind) {
    case 'instance': return { label: 'Instance record', icon: 'box', short: 'Instance' }
    case 'block': return vol ? { label: 'Block volume', icon: 'hard-drive', short: 'Volume' } : { label: 'VM disk', icon: 'hard-drive', short: 'Disk' }
    case 'filesystem': return vol ? { label: 'Filesystem volume', icon: 'folder', short: 'Volume' } : { label: 'Container filesystem', icon: 'folder', short: 'Filesystem' }
    default: return { label: kind || 'Object', icon: 'file', short: kind || 'Object' }
  }
}

/** Kind filter values used by the object list. */
export type KindFilter = 'all' | 'data' | 'instance' | 'disk' | 'volume' | 'filesystem'
export function matchesKind(kind: string, name: string, f: KindFilter): boolean {
  switch (f) {
    case 'all': return true
    case 'data': return kind !== 'instance'
    case 'instance': return kind === 'instance'
    case 'volume': return isVolume(name)
    case 'disk': return kind === 'block' && !isVolume(name)
    case 'filesystem': return kind === 'filesystem' && !isVolume(name)
  }
}

// ---- freshness and protection ---------------------------------------------------------------

export const freshnessText: Record<string, string> = {
  ok: 'Current', stale: 'Overdue', never: 'Never backed up', unscheduled: 'No schedule', reference: 'Instance record',
}
export const freshnessStatus: Record<string, Status> = {
  ok: 'ok', stale: 'warn', never: 'critical', unscheduled: 'neutral', reference: 'neutral',
}

/** Sort rank of a freshness state: worst first, instance records last. */
export const freshnessRank: Record<string, number> = { stale: 0, never: 1, unscheduled: 2, ok: 3, reference: 4 }

export type HostProtection = 'protected' | 'stale' | 'never' | 'empty'

/** Roll-up of a host's objects: never backed up beats overdue beats protected. */
export function hostProtection(h: { objects: number; stale: number; never_backed_up: number }): { state: HostProtection; status: Status; label: string } {
  if (h.never_backed_up > 0) return { state: 'never', status: 'critical', label: h.stale ? `${h.never_backed_up} never, ${h.stale} overdue` : `${h.never_backed_up} never backed up` }
  if (h.stale > 0) return { state: 'stale', status: 'warn', label: `${h.stale} overdue` }
  if (h.objects === 0) return { state: 'empty', status: 'neutral', label: 'No objects yet' }
  return { state: 'protected', status: 'ok', label: 'Protected' }
}

/** online | offline | disabled for a host row (the overview's online flag when known). */
export function hostState(h: { disabled: boolean; online?: boolean | null; last_seen: string | null }, now = Date.now(), offlineAfterMs = 6 * 3600_000): 'online' | 'offline' | 'disabled' {
  if (h.disabled) return 'disabled'
  if (h.online !== undefined && h.online !== null) return h.online ? 'online' : 'offline'
  if (!h.last_seen) return 'offline'
  return now - Date.parse(h.last_seen) < offlineAfterMs ? 'online' : 'offline'
}

/** The server's host name rule (service.CreateToken): non-empty, no space, tab or slash. */
export function hostNameProblem(name: string): string {
  if (!name.trim()) return 'Enter the host name.'
  if (/[\s/]/.test(name)) return 'A host name cannot contain spaces or slashes.'
  if (name.length > 253) return 'A host name is at most 253 characters.'
  return ''
}

// ---- certificates ---------------------------------------------------------------------------

export interface CertLike { repository: string; name: string; scope: string; state: string; days_left: number; level: string }

export function certKey(repository: string, name: string, scope: string) { return `${repository}\u0000${name}\u0000${scope}` }

export function certIndex<T extends CertLike>(hosts: T[] | null | undefined): Map<string, T> {
  const m = new Map<string, T>()
  for (const h of hosts ?? []) m.set(certKey(h.repository, h.name, h.scope), h)
  return m
}

/** Status for a certificate state word (untrusted is critical: the host must enrol again). */
export function certStatus(state: string | undefined | null): Status {
  switch (state) {
    case 'ok': return 'ok'
    case 'expiring': return 'warn'
    case 'expired': case 'untrusted': return 'critical'
    default: return 'neutral'
  }
}

// ---- days, calendars ------------------------------------------------------------------------

/** Local calendar day key YYYY-MM-DD. */
export function dayKey(d: Date): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

function startOfDay(d: Date): Date { return new Date(d.getFullYear(), d.getMonth(), d.getDate()) }
function addDays(d: Date, n: number): Date { return new Date(d.getFullYear(), d.getMonth(), d.getDate() + n) }

export interface DayCount { date: Date; key: string; count: number; extra: number }

/**
 * Count timestamps per local day for the last `days` days ending today (oldest first). `flag` marks
 * timestamps counted in `extra` too (e.g. snapshots that failed verification).
 */
export function dailyCounts(times: Array<string | null | undefined>, days: number, now: Date = new Date(), flag?: (i: number) => boolean): DayCount[] {
  const today = startOfDay(now)
  const out: DayCount[] = []
  const at = new Map<string, DayCount>()
  for (let i = days - 1; i >= 0; i--) {
    const d = addDays(today, -i)
    const c = { date: d, key: dayKey(d), count: 0, extra: 0 }
    out.push(c)
    at.set(c.key, c)
  }
  times.forEach((t, i) => {
    if (!t) return
    const c = at.get(dayKey(new Date(t)))
    if (!c) return
    c.count++
    if (flag?.(i)) c.extra++
  })
  return out
}

/** Heat level 0..4 for a count relative to the busiest day (0 only for zero). */
export function heatLevel(count: number, max: number): 0 | 1 | 2 | 3 | 4 {
  if (count <= 0 || max <= 0) return 0
  const r = count / max
  return r > 0.75 ? 4 : r > 0.5 ? 3 : r > 0.25 ? 2 : 1
}

/** Columns of 7 cells (Monday first) for a calendar heatmap; cells before the first day are null. */
export function calendarWeeks<T extends { date: Date }>(days: T[]): Array<Array<T | null>> {
  if (!days.length) return []
  const weeks: Array<Array<T | null>> = []
  const lead = (days[0].date.getDay() + 6) % 7 // Monday = 0
  let week: Array<T | null> = Array.from({ length: lead }, () => null)
  for (const d of days) {
    week.push(d)
    if (week.length === 7) { weeks.push(week); week = [] }
  }
  if (week.length) { while (week.length < 7) week.push(null); weeks.push(week) }
  return weeks
}

// ---- runs -----------------------------------------------------------------------------------

export interface RunLike { started: string; finished: string | null; status: string }

export function runDurationMs(r: RunLike): number | null {
  if (!r.finished) return null
  return Math.max(0, Date.parse(r.finished) - Date.parse(r.started))
}

const runStatus = (s: string): Status => (s === 'ok' ? 'ok' : s === 'partial' ? 'warn' : s === 'failed' ? 'critical' : 'info')

/** Runs per local day for the last `days` days: count and the worst status of the day. */
export function runsPerDay(runs: RunLike[], days: number, now: Date = new Date()): { labels: string[]; values: number[]; statuses: Array<Status | null> } {
  const counts = dailyCounts(runs.map((r) => r.started), days, now)
  const worstOf = new Map<string, Status>()
  for (const r of runs) {
    const k = dayKey(new Date(r.started))
    worstOf.set(k, worst(worstOf.get(k) ?? 'ok', runStatus(r.status)))
  }
  return {
    labels: counts.map((c) => c.key.slice(5)),
    values: counts.map((c) => c.count),
    statuses: counts.map((c) => (c.count ? worstOf.get(c.key) ?? null : null)),
  }
}

// ---- capacity -------------------------------------------------------------------------------

/** Sum of a value per key, largest first; everything after `top` folded into "Other". */
export function shareBy<T>(rows: T[], key: (r: T) => string, value: (r: T) => number, top = 4): Array<{ label: string; value: number }> {
  const m = new Map<string, number>()
  for (const r of rows) m.set(key(r), (m.get(key(r)) ?? 0) + Math.max(0, value(r)))
  const all = [...m.entries()].map(([label, v]) => ({ label, value: v })).sort((a, b) => b.value - a.value || a.label.localeCompare(b.label))
  if (all.length <= top + 1) return all
  const rest = all.slice(top).reduce((a, x) => a + x.value, 0)
  return [...all.slice(0, top), { label: `Other (${all.length - top})`, value: rest }]
}

/**
 * Forecast points after the last sample: linear growth `perDay` (bytes/day) from `lastValue` at
 * `lastAt`, one point per `stepDays` until `untilDays` ahead. Returns [] for unknown or negative growth.
 */
export function linearForecast(lastAt: number, lastValue: number, perDay: number | null | undefined, untilDays: number, stepDays = 1): Array<{ at: number; value: number }> {
  if (perDay === null || perDay === undefined || !Number.isFinite(perDay) || perDay < 0 || untilDays <= 0) return []
  const out: Array<{ at: number; value: number }> = []
  for (let d = stepDays; d <= untilDays + 1e-9; d += stepDays) out.push({ at: lastAt + d * 86400_000, value: lastValue + perDay * d })
  return out
}

/** Days from now until an RFC 3339 time (negative = past; null when unknown). */
export function daysUntil(t: string | null | undefined, now = Date.now()): number | null {
  if (!t) return null
  const ms = Date.parse(t)
  if (!Number.isFinite(ms)) return null
  return (ms - now) / 86400_000
}

/** "in 12 days", "in 5 months", "in 2.4 years", "3 days ago" for a day count (null: "not in sight"). */
export function inDays(days: number | null): string {
  if (days === null || !Number.isFinite(days)) return 'not in sight'
  const a = Math.abs(days)
  const t = a < 1 ? 'less than a day' : a < 60 ? `${Math.round(a)} ${Math.round(a) === 1 ? 'day' : 'days'}` : a < 730 ? `${Math.round(a / 30.44)} months` : `${(a / 365.25).toFixed(1)} years`
  return days >= 0 ? `in ${t}` : `${t} ago`
}

/** Status of a projected date: under 30 days critical, under 180 days warn. */
export function projectionStatus(days: number | null): Status {
  if (days === null) return 'neutral'
  if (days < 30) return 'critical'
  if (days < 180) return 'warn'
  return 'ok'
}

// ---- repository health ----------------------------------------------------------------------

export interface RepoHealthInput {
  mode: string
  corrupt_chunks: number
  failed_verify_snapshots: number
  sync_failing: number
  fs_total: number
  fs_free: number
  last_verify: string | null
  snapshots: number
}

/** Roll-up of one repository with the reasons, worst first. */
export function repoHealth(r: RepoHealthInput, now = Date.now()): { status: Status; reasons: Array<{ status: Status; text: string }> } {
  const reasons: Array<{ status: Status; text: string }> = []
  if (r.corrupt_chunks > 0) reasons.push({ status: 'critical', text: `${r.corrupt_chunks} corrupt ${r.corrupt_chunks === 1 ? 'chunk' : 'chunks'} in quarantine` })
  if (r.failed_verify_snapshots > 0) reasons.push({ status: 'critical', text: `${r.failed_verify_snapshots} ${r.failed_verify_snapshots === 1 ? 'snapshot' : 'snapshots'} failed verification` })
  if (r.mode === 'offline') reasons.push({ status: 'critical', text: 'Offline: backups and restores are refused' })
  else if (r.mode === 'read-only') reasons.push({ status: 'warn', text: 'Read-only: new backups are refused' })
  if (r.fs_total > 0) {
    const used = 1 - r.fs_free / r.fs_total
    if (used >= 0.9) reasons.push({ status: 'critical', text: `Disk ${Math.round(used * 100)} % full` })
    else if (used >= 0.8) reasons.push({ status: 'warn', text: `Disk ${Math.round(used * 100)} % full` })
  }
  if (r.sync_failing > 0) reasons.push({ status: 'warn', text: `${r.sync_failing} sync ${r.sync_failing === 1 ? 'job' : 'jobs'} failing` })
  if (r.snapshots > 0 && (!r.last_verify || now - Date.parse(r.last_verify) > 14 * 86400_000)) reasons.push({ status: 'warn', text: r.last_verify ? 'Not verified for more than 14 days' : 'Never verified' })
  const rank: Record<Status, number> = { critical: 0, warn: 1, info: 2, neutral: 3, ok: 4 }
  reasons.sort((a, b) => rank[a.status] - rank[b.status])
  return { status: worst(...reasons.map((x) => x.status)), reasons }
}

// ---- jobs -----------------------------------------------------------------------------------

export const jobTypeText: Record<string, string> = {
  backup_now: 'Backup now', restore_test: 'Restore test', deep_verify: 'Deep verify', restore_instance: 'Restore instance',
  restore_volume: 'Restore volume', guest_files: 'Guest files', fs_extract: 'File extract',
}

/** One line describing a job's target from its params. */
export function jobTarget(params: Record<string, unknown> | null | undefined): string {
  if (!params) return ''
  const p = params as Record<string, string | undefined>
  const parts = [p.project, p.object ?? p.instance, p.policy ? `policy ${p.policy}` : undefined].filter(Boolean)
  return parts.join(' / ')
}

export const activeJob = (state: string) => state === 'queued' || state === 'running'

// ---- retention preview ----------------------------------------------------------------------

export interface PlanLike { group: string; snapshot: string; keep: boolean; protected: boolean; reason: string }

/** The repository-wide prune plan narrowed to one object, newest snapshot first. */
export function planFor<T extends PlanLike>(plan: T[] | null | undefined, group: string): { entries: T[]; keep: number; remove: number; protectedCount: number } {
  const entries = (plan ?? []).filter((p) => p.group === group).sort((a, b) => b.snapshot.localeCompare(a.snapshot))
  return { entries, keep: entries.filter((e) => e.keep).length, remove: entries.filter((e) => !e.keep).length, protectedCount: entries.filter((e) => e.protected).length }
}
