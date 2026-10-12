import { describe, expect, it } from 'vitest'
import {
  kindInfo, matchesKind, hostProtection, hostState, hostNameProblem, certIndex, certKey, certStatus, dailyCounts, heatLevel,
  calendarWeeks, inDays, runsPerDay, runDurationMs, shareBy, linearForecast, daysUntil, projectionStatus, repoHealth, jobTarget, planFor, dayKey,
} from './infra'

describe('kinds', () => {
  it('tells instance records, disks and volumes apart', () => {
    expect(kindInfo('instance', 'instance/web').short).toBe('Instance')
    expect(kindInfo('block', 'instance/web/root')).toMatchObject({ short: 'Disk', icon: 'hard-drive' })
    expect(kindInfo('block', 'volume/data').short).toBe('Volume')
    expect(kindInfo('filesystem', 'instance/c1/root')).toMatchObject({ short: 'Filesystem', icon: 'folder' })
    expect(kindInfo('filesystem', 'volume/nc').label).toBe('Filesystem volume')
    expect(matchesKind('block', 'volume/x', 'volume')).toBe(true)
    expect(matchesKind('block', 'volume/x', 'disk')).toBe(false)
    expect(matchesKind('filesystem', 'instance/c/root', 'filesystem')).toBe(true)
    expect(matchesKind('instance', 'instance/c', 'all')).toBe(true)
    expect(matchesKind('instance', 'instance/c', 'data')).toBe(false)
    expect(matchesKind('block', 'instance/c/root', 'data')).toBe(true)
  })
})

describe('hosts', () => {
  it('rolls up protection: never beats overdue beats protected', () => {
    expect(hostProtection({ objects: 4, stale: 1, never_backed_up: 2 })).toMatchObject({ state: 'never', status: 'critical', label: '2 never, 1 overdue' })
    expect(hostProtection({ objects: 4, stale: 0, never_backed_up: 1 }).label).toBe('1 never backed up')
    expect(hostProtection({ objects: 4, stale: 3, never_backed_up: 0 })).toMatchObject({ state: 'stale', label: '3 overdue' })
    expect(hostProtection({ objects: 0, stale: 0, never_backed_up: 0 }).state).toBe('empty')
    expect(hostProtection({ objects: 2, stale: 0, never_backed_up: 0 }).state).toBe('protected')
  })
  it('derives online state, disabled first', () => {
    const now = Date.parse('2026-10-08T12:00:00Z')
    expect(hostState({ disabled: true, online: true, last_seen: null }, now)).toBe('disabled')
    expect(hostState({ disabled: false, online: false, last_seen: '2026-10-08T11:59:00Z' }, now)).toBe('offline')
    expect(hostState({ disabled: false, last_seen: '2026-10-08T07:00:00Z' }, now)).toBe('online')
    expect(hostState({ disabled: false, last_seen: '2026-10-08T05:00:00Z' }, now)).toBe('offline')
    expect(hostState({ disabled: false, last_seen: null }, now)).toBe('offline')
  })
  it('checks host names like the server', () => {
    expect(hostNameProblem('')).toMatch(/Enter/)
    expect(hostNameProblem('a b')).toMatch(/spaces/)
    expect(hostNameProblem('a/b')).toMatch(/slashes/)
    expect(hostNameProblem('node-01.example')).toBe('')
  })
  it('indexes certificates by repository, name and scope', () => {
    const m = certIndex([{ repository: 'r', name: 'h', scope: 'backup', state: 'expiring', days_left: 20, level: 'warn' }])
    expect(m.get(certKey('r', 'h', 'backup'))?.days_left).toBe(20)
    expect(m.get(certKey('r', 'h', 'sync-read'))).toBeUndefined()
    expect(certStatus('untrusted')).toBe('critical')
    expect(certStatus('expiring')).toBe('warn')
    expect(certStatus(undefined)).toBe('neutral')
  })
})

describe('calendar', () => {
  const now = new Date(2026, 9, 8, 15, 0) // local time, Thursday
  it('counts per local day, oldest first, ignoring older times', () => {
    const t = (d: number, h: number) => new Date(2026, 9, d, h).toISOString()
    const c = dailyCounts([t(8, 3), t(8, 9), t(7, 3), t(1, 3), null], 7, now, (i) => i === 1)
    expect(c).toHaveLength(7)
    expect(c[6]).toMatchObject({ key: '2026-10-08', count: 2, extra: 1 })
    expect(c[5].count).toBe(1)
    expect(c[0].key).toBe('2026-10-02')
    expect(c.reduce((a, x) => a + x.count, 0)).toBe(3) // Oct 1 is outside the window
  })
  it('maps counts to heat levels', () => {
    expect(heatLevel(0, 5)).toBe(0)
    expect(heatLevel(1, 8)).toBe(1)
    expect(heatLevel(5, 8)).toBe(3)
    expect(heatLevel(8, 8)).toBe(4)
  })
  it('lays out weeks Monday first with padding', () => {
    const days = dailyCounts([], 10, now) // Sep 29 (Tue) .. Oct 8 (Thu)
    const w = calendarWeeks(days)
    expect(w).toHaveLength(2)
    expect(w[0][0]).toBeNull() // Monday Sep 28 is before the window
    expect(dayKey(w[0][1]!.date)).toBe('2026-09-29')
    expect(w[1].filter(Boolean)).toHaveLength(4)
    expect(w.every((x) => x.length === 7)).toBe(true)
  })
})

describe('runs', () => {
  it('computes durations and the worst status per day', () => {
    expect(runDurationMs({ started: '2026-10-08T03:00:00Z', finished: '2026-10-08T03:02:30Z', status: 'ok' })).toBe(150_000)
    expect(runDurationMs({ started: '2026-10-08T03:00:00Z', finished: null, status: 'running' })).toBeNull()
    const now = new Date(2026, 9, 8, 15)
    const at = (d: number) => new Date(2026, 9, d, 3).toISOString()
    const r = runsPerDay([{ started: at(8), finished: at(8), status: 'ok' }, { started: at(8), finished: at(8), status: 'failed' }, { started: at(7), finished: at(7), status: 'partial' }], 3, now)
    expect(r.values).toEqual([0, 1, 2])
    expect(r.statuses).toEqual([null, 'warn', 'critical'])
    expect(r.labels[2]).toBe('10-08')
  })
})

describe('capacity', () => {
  it('folds small shares into Other', () => {
    const rows = [['a', 5], ['b', 4], ['a', 1], ['c', 3], ['d', 2], ['e', 1], ['f', 1]] as const
    const s = shareBy(rows as unknown as Array<[string, number]>, (r) => r[0], (r) => r[1], 3)
    expect(s.map((x) => x.label)).toEqual(['a', 'b', 'c', 'Other (3)'])
    expect(s[0].value).toBe(6)
    expect(s[3].value).toBe(4)
    expect(shareBy([['x', 1]] as Array<[string, number]>, (r) => r[0], (r) => r[1])).toEqual([{ label: 'x', value: 1 }])
  })
  it('extends a linear forecast and refuses unknown growth', () => {
    const f = linearForecast(0, 100, 10, 3)
    expect(f.map((p) => p.value)).toEqual([110, 120, 130])
    expect(f[2].at).toBe(3 * 86400_000)
    expect(linearForecast(0, 100, null, 30)).toEqual([])
    expect(linearForecast(0, 100, -5, 30)).toEqual([])
  })
  it('turns projected dates into days and statuses', () => {
    const now = Date.parse('2026-10-08T00:00:00Z')
    expect(daysUntil('2026-10-18T00:00:00Z', now)).toBe(10)
    expect(daysUntil(null, now)).toBeNull()
    expect(projectionStatus(10)).toBe('critical')
    expect(projectionStatus(100)).toBe('warn')
    expect(projectionStatus(1000)).toBe('ok')
    expect(projectionStatus(null)).toBe('neutral')
    expect(inDays(12.4)).toBe('in 12 days')
    expect(inDays(1)).toBe('in 1 day')
    expect(inDays(152)).toBe('in 5 months')
    expect(inDays(1000)).toBe('in 2.7 years')
    expect(inDays(-3)).toBe('3 days ago')
    expect(inDays(null)).toBe('not in sight')
  })
})

describe('repository health', () => {
  const base = { mode: 'online', corrupt_chunks: 0, failed_verify_snapshots: 0, sync_failing: 0, fs_total: 100, fs_free: 50, last_verify: '2026-10-07T00:00:00Z', snapshots: 3 }
  const now = Date.parse('2026-10-08T00:00:00Z')
  it('is ok when nothing is wrong', () => {
    expect(repoHealth(base, now)).toEqual({ status: 'ok', reasons: [] })
  })
  it('lists every reason, worst first', () => {
    const h = repoHealth({ ...base, mode: 'read-only', corrupt_chunks: 2, fs_free: 15, last_verify: null }, now)
    expect(h.status).toBe('critical')
    expect(h.reasons[0].text).toBe('2 corrupt chunks in quarantine')
    expect(h.reasons.map((r) => r.text)).toContain('Disk 85 % full')
    expect(h.reasons.map((r) => r.text)).toContain('Never verified')
    expect(h.reasons.map((r) => r.text)).toContain('Read-only: new backups are refused')
  })
  it('does not judge fill level on object storage', () => {
    expect(repoHealth({ ...base, fs_total: 0, fs_free: 0 }, now).status).toBe('ok')
  })
})

describe('jobs and retention', () => {
  it('describes a job target', () => {
    expect(jobTarget({ project: 'prod', object: 'web-01' })).toBe('prod / web-01')
    expect(jobTarget({ policy: 'nightly' })).toBe('policy nightly')
    expect(jobTarget(null)).toBe('')
  })
  it('narrows the prune plan to one object', () => {
    const plan = [
      { group: 'g1', snapshot: '20261001', keep: false, protected: false, reason: '' },
      { group: 'g2', snapshot: '20261002', keep: true, protected: false, reason: 'last 1' },
      { group: 'g1', snapshot: '20261003', keep: true, protected: true, reason: 'protected' },
    ]
    const p = planFor(plan, 'g1')
    expect(p.entries.map((e) => e.snapshot)).toEqual(['20261003', '20261001'])
    expect([p.keep, p.remove, p.protectedCount]).toEqual([1, 1, 1])
    expect(planFor(null, 'g1').entries).toEqual([])
  })
})
