// Snapshot rows for the list, detail and verification pages: the datastore's view (GET /snapshots:
// existence, protection, notes, verify state) merged with what PostgreSQL recorded for the run
// (GET /snapshot-stats: byte counters, duration, base, verification time). Unit-tested.
import type { SnapshotRef } from '@/api/types'
import type { SnapshotStats } from '@/api/sections/protect'

export type VerifyClass = 'ok' | 'stale' | 'failed' | 'never'

export interface SnapRow extends SnapshotRef {
  /** "group/snapshot", unique in a repository. */
  key: string
  stats: SnapshotStats | null
  verifiedAt: string | null
  verifyClass: VerifyClass
  /** Snapshot id of the previous / next snapshot of the same object (by id = creation order). */
  prevId: string | null
  nextId: string | null
}

export const snapKey = (g: string, s: string) => `${g}/${s}`

/** Re-verify age in days for a snapshot: its policy's reverify_days, else the repository default. */
export type ReverifyDays = (policy: string | undefined) => number

/**
 * The verification state of one snapshot as an operator reads it:
 *   failed: the last verification found a problem (never an incremental base, never synced verified-only);
 *   never:  no verification ever;
 *   stale:  verified OK, but longer ago than the policy's re-verify age (or with an unknown time);
 *   ok:     verified OK recently.
 */
export function verifyClass(state: string | undefined, verifiedAt: string | null, maxAgeDays: number, now: number): VerifyClass {
  if (state === 'failed') return 'failed'
  if (state !== 'ok') return 'never'
  if (!verifiedAt) return 'stale'
  if (maxAgeDays <= 0) return 'ok'
  return now - Date.parse(verifiedAt) > maxAgeDays * 86_400_000 ? 'stale' : 'ok'
}

export function mergeSnapshots(refs: readonly SnapshotRef[] | null | undefined, stats: readonly SnapshotStats[] | null | undefined, reverify: ReverifyDays, now = Date.now()): SnapRow[] {
  const byKey = new Map<string, SnapshotStats>()
  for (const s of stats ?? []) byKey.set(snapKey(s.group_id, s.snapshot_id), s)
  const rows: SnapRow[] = (refs ?? []).map((r) => {
    const st = byKey.get(snapKey(r.group_id, r.snapshot_id)) ?? null
    const at = st?.verified_at ?? null
    return {
      ...r,
      key: snapKey(r.group_id, r.snapshot_id),
      stats: st,
      verifiedAt: at,
      verifyClass: verifyClass(r.verify_state, at, reverify(r.policy), now),
      prevId: null,
      nextId: null,
    }
  })
  const groups = new Map<string, SnapRow[]>()
  for (const r of rows) {
    const l = groups.get(r.group_id)
    if (l) l.push(r)
    else groups.set(r.group_id, [r])
  }
  for (const l of groups.values()) {
    l.sort((a, b) => (a.snapshot_id < b.snapshot_id ? -1 : a.snapshot_id > b.snapshot_id ? 1 : 0))
    for (let i = 0; i < l.length; i++) {
      l[i].prevId = i > 0 ? l[i - 1].snapshot_id : null
      l[i].nextId = i + 1 < l.length ? l[i + 1].snapshot_id : null
    }
  }
  return rows
}

export interface SnapFilter {
  host?: string
  project?: string
  object?: string
  kind?: string
  policy?: string
  protected?: '' | 'yes' | 'no'
  verify?: '' | VerifyClass
  /** yyyy-mm-dd, inclusive, browser time zone */
  from?: string
  to?: string
}

const dayStart = (d: string) => new Date(`${d}T00:00:00`).getTime()

export function filterSnapshots(rows: readonly SnapRow[], f: SnapFilter): SnapRow[] {
  const from = f.from ? dayStart(f.from) : -Infinity
  const to = f.to ? dayStart(f.to) + 86_400_000 : Infinity
  const obj = (f.object ?? '').trim().toLowerCase()
  return rows.filter((r) => {
    if (f.host && r.host !== f.host) return false
    if (f.project && r.project !== f.project) return false
    if (f.kind && r.kind !== f.kind) return false
    if (f.policy && r.policy !== f.policy) return false
    if (obj && !(r.object ?? '').toLowerCase().includes(obj) && !r.group_id.startsWith(obj)) return false
    if (f.protected === 'yes' && !r.protected) return false
    if (f.protected === 'no' && r.protected) return false
    if (f.verify && r.verifyClass !== f.verify) return false
    const t = Date.parse(r.created)
    return t >= from && t < to
  })
}

/** Distinct sorted values of a field (for filter selects). */
export function distinct(rows: readonly SnapRow[], f: (r: SnapRow) => string | undefined): string[] {
  return [...new Set(rows.map(f).filter((x): x is string => !!x))].sort((a, b) => a.localeCompare(b))
}

export interface VerifySummary { total: number; ok: number; stale: number; failed: number; never: number }

export function verifySummary(rows: readonly SnapRow[]): VerifySummary {
  const s: VerifySummary = { total: rows.length, ok: 0, stale: 0, failed: 0, never: 0 }
  for (const r of rows) s[r.verifyClass]++
  return s
}

/** Which of the selected snapshots a bulk forget may remove, and which it must refuse (protected). */
export function forgetPlan(rows: readonly SnapRow[]): { allowed: SnapRow[]; refused: SnapRow[] } {
  return { allowed: rows.filter((r) => !r.protected), refused: rows.filter((r) => r.protected) }
}

/** Human label of the object kind. */
export function kindLabel(kind: string | undefined): string {
  switch (kind) {
    case 'block': return 'Disk (block)'
    case 'filesystem': return 'Filesystem'
    case 'instance': return 'Instance config'
  }
  return kind || 'Unknown'
}

/**
 * What changed between two snapshots of one object, from the recorded counters only (the server cannot
 * see contents): logical size difference, how much the run read and uploaded.
 */
export function changeSummary(cur: SnapRow, prev: SnapRow | null): { sizeDelta: number | null; dirty: number | null; uploaded: number | null; read: number | null; fullRead: boolean | null } {
  return {
    sizeDelta: prev && cur.logical_bytes !== undefined && prev.logical_bytes !== undefined ? cur.logical_bytes - prev.logical_bytes : null,
    dirty: cur.stats ? cur.stats.dirty_bytes : null,
    uploaded: cur.stats ? cur.stats.uploaded_bytes : null,
    read: cur.stats ? cur.stats.read_bytes : null,
    fullRead: cur.stats ? cur.stats.full_read : null,
  }
}
