import { describe, expect, it } from 'vitest'
import type { SnapshotRef } from '@/api/types'
import type { SnapshotStats } from '@/api/sections/protect'
import { changeSummary, distinct, filterSnapshots, forgetPlan, mergeSnapshots, verifyClass, verifySummary } from './snapshots'

const NOW = Date.parse('2026-10-08T12:00:00Z')
const G1 = 'a'.repeat(32)
const G2 = 'b'.repeat(32)
const ref = (g: string, s: string, o: Partial<SnapshotRef> = {}): SnapshotRef => ({
  group_id: g, snapshot_id: s, created: `${s.slice(0, 4)}-${s.slice(4, 6)}-${s.slice(6, 8)}T${s.slice(8, 10)}:00:00Z`, committed: '', index_digest: '', chunk_count: 1,
  host: 'benfica', project: 'default', object: g === G1 ? 'volume/docs' : 'instance/vm1/root', kind: g === G1 ? 'filesystem' : 'block', policy: 'nightly', logical_bytes: 100, ...o,
})
const stat = (g: string, s: string, o: Partial<SnapshotStats> = {}): SnapshotStats => ({
  group_id: g, snapshot_id: s, logical_bytes: 100, dirty_bytes: 10, read_bytes: 20, uploaded_bytes: 5, full_read: false, duration_ms: 1000,
  previous_snapshot_id: '', source: 'client', verify_state: '', verified_at: null, ...o,
})

describe('verifyClass', () => {
  it('classifies failed, never, stale and ok', () => {
    expect(verifyClass('failed', '2026-10-08T00:00:00Z', 30, NOW)).toBe('failed')
    expect(verifyClass(undefined, null, 30, NOW)).toBe('never')
    expect(verifyClass('ok', '2026-08-01T00:00:00Z', 30, NOW)).toBe('stale')
    expect(verifyClass('ok', null, 30, NOW)).toBe('stale') // state without a time: do not claim fresh
    expect(verifyClass('ok', '2026-10-01T00:00:00Z', 30, NOW)).toBe('ok')
    expect(verifyClass('ok', '2020-01-01T00:00:00Z', 0, NOW)).toBe('ok') // re-verification off
  })
})

describe('mergeSnapshots', () => {
  const refs = [ref(G1, '20261003030000-00000003'), ref(G1, '20261001030000-00000001', { protected: true }), ref(G2, '20261002030000-00000002', { verify_state: 'failed' })]
  const stats = [stat(G1, '20261001030000-00000001', { verify_state: 'ok', verified_at: '2026-10-07T00:00:00Z', full_read: true }), stat(G1, '20261003030000-00000003')]
  const rows = mergeSnapshots(refs, stats, (p) => (p === 'nightly' ? 30 : 7), NOW)
  const byId = Object.fromEntries(rows.map((r) => [r.snapshot_id, r]))
  it('joins stats by group and snapshot, missing stats are null', () => {
    expect(byId['20261001030000-00000001'].stats?.full_read).toBe(true)
    expect(byId['20261002030000-00000002'].stats).toBeNull()
  })
  it('links previous and next within one object only', () => {
    expect(byId['20261001030000-00000001'].prevId).toBeNull()
    expect(byId['20261001030000-00000001'].nextId).toBe('20261003030000-00000003')
    expect(byId['20261003030000-00000003'].prevId).toBe('20261001030000-00000001')
    expect(byId['20261002030000-00000002'].prevId).toBeNull()
    expect(byId['20261002030000-00000002'].nextId).toBeNull()
  })
  it('uses the datastore verify state with the recorded time', () => {
    // GET /snapshots says nothing for ...01 (no verify_state on the ref) -> never, even if the cache has a time
    expect(byId['20261001030000-00000001'].verifyClass).toBe('never')
    expect(byId['20261002030000-00000002'].verifyClass).toBe('failed')
    const r2 = mergeSnapshots([ref(G1, '20261001030000-00000001', { verify_state: 'ok' })], stats, () => 30, NOW)
    expect(r2[0].verifyClass).toBe('ok')
    expect(r2[0].verifiedAt).toBe('2026-10-07T00:00:00Z')
  })
  it('summarises', () => {
    expect(verifySummary(rows)).toEqual({ total: 3, ok: 0, stale: 0, failed: 1, never: 2 })
  })
  it('filters by every facet and by date (inclusive days)', () => {
    expect(filterSnapshots(rows, { kind: 'block' }).map((r) => r.snapshot_id)).toEqual(['20261002030000-00000002'])
    expect(filterSnapshots(rows, { protected: 'yes' }).length).toBe(1)
    expect(filterSnapshots(rows, { protected: 'no' }).length).toBe(2)
    expect(filterSnapshots(rows, { verify: 'failed' }).length).toBe(1)
    expect(filterSnapshots(rows, { object: 'DOCS' }).length).toBe(2)
    expect(filterSnapshots(rows, { object: 'bbbb' }).length).toBe(1) // group id prefix
    expect(filterSnapshots(rows, { from: '2026-10-02', to: '2026-10-02' }).length).toBe(1)
    expect(filterSnapshots(rows, { host: 'other' }).length).toBe(0)
    expect(distinct(rows, (r) => r.kind)).toEqual(['block', 'filesystem'])
  })
  it('refuses protected snapshots in a bulk forget', () => {
    const p = forgetPlan(rows)
    expect(p.refused.map((r) => r.snapshot_id)).toEqual(['20261001030000-00000001'])
    expect(p.allowed.length).toBe(2)
  })
  it('summarises a change from the counters', () => {
    const cur = { ...byId['20261003030000-00000003'], logical_bytes: 150 }
    expect(changeSummary(cur, byId['20261001030000-00000001'])).toEqual({ sizeDelta: 50, dirty: 10, uploaded: 5, read: 20, fullRead: false })
    expect(changeSummary(byId['20261002030000-00000002'], null)).toEqual({ sizeDelta: null, dirty: null, uploaded: null, read: null, fullRead: null })
  })
})
