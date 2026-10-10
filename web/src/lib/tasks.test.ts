import { describe, expect, it } from 'vitest'
import { cancelRole, clockSkewMs, isActive, levelStatus, mergeLog, skewStatus, startedByLabel, taskKindLabel, taskResultSummary } from './tasks'

describe('tasks helpers', () => {
  it('labels kinds and actors', () => {
    expect(taskKindLabel('gc')).toBe('Garbage collection')
    expect(taskKindLabel('mystery')).toBe('mystery')
    expect(startedByLabel({ started_by: 'system', actor_kind: 'system' })).toBe('scheduler')
    expect(startedByLabel({ started_by: 'local', actor_kind: 'local' })).toBe('server CLI')
    expect(startedByLabel({ started_by: 'token:ci', actor_kind: 'token' })).toBe('token:ci')
    expect(isActive({ state: 'running' })).toBe(true)
    expect(isActive({ state: 'cancelled' })).toBe(false)
  })

  it('knows the cancel role of each kind (unknown kinds need admin)', () => {
    expect(cancelRole('verify')).toBe('operator')
    expect(cancelRole('gc')).toBe('admin')
    expect(cancelRole('repository_destroy')).toBe('admin')
    expect(cancelRole('new_kind')).toBe('admin')
  })

  it('summarises results per kind', () => {
    expect(taskResultSummary('gc', { removed: 3, bytes_reclaimed: 2048, scanned: 10, dry_run: true })).toBe('would remove 3 chunks (2 KiB) of 10 scanned')
    expect(taskResultSummary('retention', { removed: 1, kept: 4, groups: 2 })).toBe('1 snapshot removed, 4 kept in 2 objects')
    expect(taskResultSummary('scrub', { ok: false, stats: { snapshots: 2, chunks: 5, problems: [{}, {}] } })).toBe('2 problems found: 2 snapshots, 5 chunks read')
    expect(taskResultSummary('verify', { ok: true, stats: { snapshots: 1, chunks: 1 } })).toBe('no problems: 1 snapshot, 1 chunk read')
    expect(taskResultSummary('group_delete', { snapshots_deleted: 3, protected_overruled: 1 })).toBe('3 snapshots deleted, 1 protected overruled')
    expect(taskResultSummary('probe', [{ ok: true }, { ok: false }])).toBe('1 of 2 checks passed')
    expect(taskResultSummary('gc', null)).toBe('')
  })

  it('merges overlapping log polls and bounds the buffer', () => {
    const a = [{ seq: 1 }, { seq: 2 }]
    expect(mergeLog(a, [{ seq: 2 }, { seq: 3 }]).map((l) => l.seq)).toEqual([1, 2, 3])
    expect(mergeLog(a, [{ seq: 1 }])).toBe(a)
    expect(mergeLog(a, [{ seq: 3 }, { seq: 4 }], 3).map((l) => l.seq)).toEqual([2, 3, 4])
  })

  it('estimates clock skew from the request midpoint', () => {
    const sent = Date.parse('2026-10-08T12:00:00Z')
    expect(clockSkewMs('2026-10-08T12:00:31Z', sent, sent + 2000)).toBe(30_000)
    expect(clockSkewMs('garbage', sent, sent)).toBe(0)
    expect(skewStatus(30_000)).toBe('warn')
    expect(skewStatus(-2_000)).toBe('ok')
    expect(skewStatus(-120_000)).toBe('critical')
  })

  it('colours log levels', () => {
    expect(levelStatus('ERROR')).toBe('critical')
    expect(levelStatus('warn')).toBe('warn')
    expect(levelStatus('INFO')).toBe('info')
  })
})
