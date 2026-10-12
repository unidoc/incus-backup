// Pure logic of the storage and protection-configuration pages: health roll-ups, run statistics,
// probe advice, rate limits, time zones. Unit-tested in storage.test.ts.
import type { RepoOverview, RunRow, GCStats, PruneResult, VerifyStats, ReconcileResult, SyncJob, RepoMode } from '@/api/types'
import type { Status } from './status'
import { formatBytes, formatNumber, plural } from './format'

// ---- repository health ------------------------------------------------------------------------

export interface Health { status: Status; label: string; reasons: string[] }

const DAY = 86_400_000

/** One verdict for a repository card: the worst finding wins, every finding is listed. */
export function repoHealth(o: RepoOverview | undefined, mode: RepoMode, now = Date.now()): Health {
  const reasons: string[] = []
  let status: Status = 'ok'
  const bump = (s: Status) => { if (rank[s] > rank[status]) status = s }
  if (mode === 'offline') { bump('critical'); reasons.push('Offline: no backups, restores or maintenance.') }
  if (mode === 'read-only') { bump('warn'); reasons.push('Read-only: backups and maintenance are refused.') }
  if (!o) return { status, label: label(status), reasons }
  if (o.corrupt_chunks > 0) { bump('critical'); reasons.push(`${plural(o.corrupt_chunks, 'corrupt chunk')} quarantined.`) }
  if (o.failed_verify_snapshots > 0) { bump('critical'); reasons.push(`${plural(o.failed_verify_snapshots, 'snapshot')} failed verification.`) }
  if (o.sync_failing > 0) { bump('warn'); reasons.push(`${plural(o.sync_failing, 'sync job')} failing.`) }
  if (o.fs_total > 0) {
    const used = 1 - o.fs_free / o.fs_total
    if (used >= 0.9) { bump('critical'); reasons.push(`Disk ${Math.round(used * 100)} % full.`) } else if (used >= 0.8) { bump('warn'); reasons.push(`Disk ${Math.round(used * 100)} % full.`) }
  }
  const lv = o.last_verify ? Date.parse(o.last_verify) : 0
  if (o.snapshots > 0 && (!lv || now - lv > 8 * DAY)) { bump('warn'); reasons.push(lv ? 'Not verified for more than a week.' : 'Never verified.') }
  const lg = o.last_gc ? Date.parse(o.last_gc) : 0
  if (o.snapshots > 0 && (!lg || now - lg > 8 * DAY)) { bump('warn'); reasons.push(lg ? 'No garbage collection for more than a week.' : 'Garbage collection never ran.') }
  return { status, label: label(status), reasons }
}
const rank: Record<Status, number> = { ok: 0, neutral: 1, info: 2, warn: 3, critical: 4 }
function label(s: Status) { return s === 'ok' ? 'Healthy' : s === 'critical' ? 'Needs action' : s === 'warn' ? 'Attention' : 'Unknown' }

// ---- maintenance runs -------------------------------------------------------------------------

export const RUN_KINDS: Record<string, string> = {
  verify: 'Verify', scrub: 'Scrub (full verify)', gc: 'Garbage collection', retention: 'Retention (prune)', reconcile: 'Reconcile', capacity: 'Capacity sample',
}
export const runKindLabel = (k: string) => RUN_KINDS[k] ?? k

export type RunStats =
  | { kind: 'gc'; s: GCStats }
  | { kind: 'retention'; s: PruneResult }
  | { kind: 'verify' | 'scrub'; s: VerifyStats }
  | { kind: 'reconcile'; s: ReconcileResult }
  | { kind: 'other'; s: unknown }

export function parseRunStats(r: RunRow): RunStats | null {
  if (!r.stats) return null
  let s: unknown
  try { s = JSON.parse(r.stats) } catch { return null }
  if (s === null || typeof s !== 'object') return null
  switch (r.kind) {
    case 'gc': return { kind: 'gc', s: s as GCStats }
    case 'retention': return { kind: 'retention', s: s as PruneResult }
    case 'verify': case 'scrub': return { kind: r.kind, s: s as VerifyStats }
    case 'reconcile': return { kind: 'reconcile', s: s as ReconcileResult }
  }
  return { kind: 'other', s }
}

/** One line for a run row: "Removed 12 chunks, 1.2 GiB freed". */
export function runSummary(r: RunRow): string {
  if (r.status === 'failed' && r.error) return r.error
  const p = parseRunStats(r)
  if (!p) return isRunning(r) ? 'Running…' : ''
  switch (p.kind) {
    case 'gc': {
      const g = p.s
      const what = g.dry_run ? 'Would remove' : 'Removed'
      return `${what} ${plural(g.removed, 'chunk')} (${formatBytes(g.bytes_reclaimed)}) of ${formatNumber(g.scanned)} scanned; ${formatNumber(g.kept_young)} kept as too young, ${formatNumber(g.kept_pinned)} pinned`
    }
    case 'retention': {
      const x = p.s
      return `${x.dry_run ? 'Would remove' : 'Removed'} ${plural(x.removed, 'snapshot')}, kept ${formatNumber(x.kept)} (${formatNumber(x.protected)} protected) in ${plural(x.groups, 'object')}`
    }
    case 'verify': case 'scrub': {
      const v = p.s
      const probs = v.problems?.length ?? 0
      return `${plural(v.snapshots, 'snapshot')} and ${plural(v.chunks, 'chunk')} checked${v.skipped ? `, ${formatNumber(v.skipped)} skipped` : ''}; ${probs ? `${plural(probs, 'problem')}, ${formatNumber(v.quarantined)} quarantined` : 'no problems'}`
    }
    case 'reconcile': return `${plural(p.s.inserted, 'record')} added, ${formatNumber(p.s.removed)} removed`
  }
  return ''
}

export const isRunning = (r: { finished: string | null; status: string }) => !r.finished || r.status === 'running'

/** Status word of a run for StatusPill (a GC/verify that found problems is a warning even if "ok"). */
export function runState(r: RunRow): string {
  if (isRunning(r)) return 'running'
  if (r.status !== 'ok') return r.status
  const p = parseRunStats(r)
  if (p && (p.kind === 'verify' || p.kind === 'scrub') && (p.s.problems?.length ?? 0) > 0) return 'warning'
  return 'ok'
}

export function runDurationMs(r: { started: string; finished: string | null }, now = Date.now()): number {
  const a = Date.parse(r.started)
  const b = r.finished ? Date.parse(r.finished) : now
  return Math.max(0, b - a)
}

/** The newest run of a kind (runs come newest first). */
export function lastRun(runs: readonly RunRow[], kinds: string[]): RunRow | undefined {
  return runs.find((r) => kinds.includes(r.kind))
}

// ---- S3 / filesystem check advice -----------------------------------------------------------------

/** What to do about a failed or warned check, keyed by the check id (never by its prose). */
export function checkAdvice(id: string, level: string, kind: 'filesystem' | 's3' | string = 's3'): string {
  if (level === 'pass' || level === 'skip') return ''
  switch (id) {
    case 'connect': return 'Check the endpoint (host:port), HTTP versus HTTPS (the HTTPS switch), the region, and that the bucket exists: the server never creates buckets. With a private CA, paste its certificate.'
    case 'put': return 'The credential may not write objects: allow s3:PutObject on the bucket for the prefix (prefix/*).'
    case 'conditional': return 'The service ignores If-None-Match: *, so two writers could both commit. Use a service that honours conditional writes (AWS S3, MinIO, Ceph RGW 18+, Cloudflare R2, Backblaze B2 and others do).'
    case 'get': return 'Allow s3:GetObject on prefix/*.'
    case 'stat': return 'Allow HEAD requests (part of s3:GetObject).'
    case 'list': return 'Allow s3:ListBucket on the bucket (with the prefix as condition if you restrict it).'
    case 'delete': return 'Allow s3:DeleteObject on prefix/*: GC and retention need it. Without it space is never freed.'
    case 'credential': return 'Re-enter the access key and secret key, or pick another stored credential.'
    case 'versioning': case 'lifecycle': case 'object-lock': return ''
    case 'owner': return 'Use adopt only after you made sure the old server is switched off for good, or remove its repository there first.'
    case 'path': return kind === 'filesystem' ? 'The path is on the SERVER, not on your computer. The service account (incus-backup) must own it.' : ''
    case 'space': return 'Plan capacity: the repository grows with every backup until retention and GC free space.'
    case 'tls': return ''
    case 'name': return 'Choose another name.'
  }
  return ''
}

// ---- sync ------------------------------------------------------------------------------------

/**
 * Parse a rate limit exactly like the CLI's --rate-limit (cmd/incus-backup-server parseRate): "20MB",
 * "500KiB", "1048576" (bytes per second; "/s" is tolerated). KB/MB/GB are decimal, KiB/MiB/GiB and the
 * bare K/M/G binary; "" or "0" = unlimited.
 */
export function parseRate(input: string): number | Error {
  const s = input.trim().replace(/\/s$/i, '').replace(/\s+/g, '')
  if (s === '' || s === '0' || /^unlimited$/i.test(s)) return 0
  const m = /^(\d+(?:\.\d+)?)([kmg]?)(i?)(b?)$/i.exec(s)
  if (!m || (m[3] && !m[2])) return new Error('Use a number with a unit, e.g. 20MB (per second), 500KiB, or 0 for unlimited.')
  const n = parseFloat(m[1])
  const unit = m[2].toLowerCase()
  const pow = { '': 0, k: 1, m: 2, g: 3 }[unit] ?? 0
  const base = unit && m[4] && !m[3] ? 1000 : 1024 // KB = 1000, KiB and bare K = 1024
  const v = Math.floor(n * base ** pow)
  if (v < 1 || v > 1e12) return new Error('The rate limit is out of range (1 byte/s to 1 TB/s).')
  return v
}

/** "unlimited" or "20 MB/s" (decimal, like network speeds). */
export function formatRateLimit(bps: number): string {
  if (!bps) return 'unlimited'
  const units = ['B', 'kB', 'MB', 'GB', 'TB']
  let v = bps
  let i = 0
  while (v >= 1000 && i < units.length - 1) { v /= 1000; i++ }
  return `${Number(v.toFixed(v >= 100 ? 0 : 1))} ${units[i]}/s`
}

/** Health of a sync job from its list row and (optional) status. */
export function syncHealth(j: SyncJob, lastStatus?: string | null, lagMs?: number, intervalMs?: number): { status: Status; label: string } {
  if (!j.enabled) return { status: 'neutral', label: 'Paused' }
  if (lastStatus === 'failed') return { status: 'critical', label: 'Failing' }
  if (lastStatus === 'partial') return { status: 'warn', label: 'Partial' }
  if (lastStatus === 'running') return { status: 'info', label: 'Running' }
  if (!j.last_success && !lastStatus) return { status: 'neutral', label: 'Never ran' }
  if (lagMs !== undefined && intervalMs && lagMs > 2 * intervalMs + DAY / 4) return { status: 'warn', label: 'Lagging' }
  return { status: 'ok', label: 'In sync' }
}

/** Approximate interval of a schedule in ms (for lag judgement); 0 for manual. */
export function scheduleIntervalMs(p: { kind: string; everyMs?: number }): number {
  switch (p.kind) {
    case 'every': return p.everyMs ?? 0
    case 'daily': return DAY
    case 'weekly': return 7 * DAY
    case 'monthly': return 31 * DAY
  }
  return 0
}

// ---- time zones -----------------------------------------------------------------------------

const FALLBACK_ZONES = ['UTC', 'Atlantic/Reykjavik', 'Europe/London', 'Europe/Dublin', 'Europe/Lisbon', 'Europe/Berlin', 'Europe/Paris', 'Europe/Amsterdam', 'Europe/Madrid', 'Europe/Rome', 'Europe/Stockholm', 'Europe/Oslo', 'Europe/Copenhagen', 'Europe/Helsinki', 'Europe/Warsaw', 'Europe/Prague', 'Europe/Vienna', 'Europe/Zurich', 'Europe/Athens', 'Europe/Istanbul', 'Europe/Moscow', 'America/New_York', 'America/Chicago', 'America/Denver', 'America/Los_Angeles', 'America/Toronto', 'America/Sao_Paulo', 'America/Mexico_City', 'Asia/Dubai', 'Asia/Kolkata', 'Asia/Jakarta', 'Asia/Singapore', 'Asia/Shanghai', 'Asia/Tokyo', 'Asia/Seoul', 'Australia/Sydney', 'Australia/Perth', 'Pacific/Auckland', 'Africa/Johannesburg', 'Africa/Cairo', 'Africa/Lagos']

/** Every IANA zone the browser knows (Go's tzdata knows the same names), UTC first. */
export function timeZones(): string[] {
  let z: string[] = []
  try { z = (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.('timeZone') ?? [] } catch { /* old browser */ }
  if (!z.length) z = FALLBACK_ZONES
  return ['UTC', ...z.filter((x) => x !== 'UTC')]
}

/** UTC offset of a zone right now: "UTC+02:00". */
export function zoneOffset(tz: string, at = new Date()): string {
  try {
    const p = new Intl.DateTimeFormat('en-US', { timeZone: tz, timeZoneName: 'longOffset' }).formatToParts(at).find((x) => x.type === 'timeZoneName')?.value ?? ''
    return p === 'GMT' ? 'UTC+00:00' : p.replace('GMT', 'UTC')
  } catch { return '' }
}

/** Case-insensitive search over zone names; "berlin", "new york" and "utc+2" all work. */
export function searchZones(all: readonly string[], q: string, limit = 60): string[] {
  const t = q.trim().toLowerCase().replace(/\s+/g, '_')
  if (!t) return all.slice(0, limit)
  const off = /^utc[+-]/.test(t)
  const hits = all.filter((z) => (off ? zoneOffset(z).toLowerCase().startsWith(t.replace(/^utc([+-])(\d)$/, 'utc$10$2')) : z.toLowerCase().includes(t)))
  hits.sort((a, b) => {
    const as = a.toLowerCase().split('/').pop()!.startsWith(t) ? 0 : 1
    const bs = b.toLowerCase().split('/').pop()!.startsWith(t) ? 0 : 1
    return as - bs || a.localeCompare(b)
  })
  return hits.slice(0, limit)
}
