import { describe, expect, it } from 'vitest'
import { checkAdvice, formatRateLimit, isRunning, parseRate, repoHealth, runState, runSummary, searchZones, syncHealth, timeZones, zoneOffset } from './storage'
import type { RepoOverview, RunRow, SyncJob } from '@/api/types'

const repo = (o: Partial<RepoOverview> = {}): RepoOverview => ({
  name: 'r', backend: 'filesystem', mode: 'online', hosts: 1, objects: 1, snapshots: 10, logical_bytes: 0, physical_bytes: 0, dedup_ratio: 1,
  fs_total: 100, fs_free: 50, chunks: 1, corrupt_chunks: 0, failed_verify_snapshots: 0, last_backup: null,
  last_verify: new Date().toISOString(), last_gc: new Date().toISOString(), last_retention: null, sync_jobs: 0, sync_failing: 0, ...o,
})
const run = (o: Partial<RunRow>): RunRow => ({ kind: 'gc', started: '2026-10-08T05:00:00Z', finished: '2026-10-08T05:00:03Z', status: 'ok', error: '', stats: '', ...o })

describe('repository health', () => {
  it('is healthy when nothing is wrong', () => {
    expect(repoHealth(repo(), 'online').status).toBe('ok')
  })
  it('rolls up the worst finding and lists every reason', () => {
    const h = repoHealth(repo({ corrupt_chunks: 2, sync_failing: 1, fs_free: 15 }), 'read-only')
    expect(h.status).toBe('critical')
    expect(h.reasons).toEqual(['Read-only: backups and maintenance are refused.', '2 corrupt chunks quarantined.', '1 sync job failing.', 'Disk 85 % full.'])
  })
  it('flags a repository that was never verified or collected', () => {
    const h = repoHealth(repo({ last_verify: null, last_gc: null, fs_total: 0 }), 'online')
    expect(h.status).toBe('warn')
    expect(h.reasons).toContain('Never verified.')
  })
  it('offline is critical even without overview data', () => {
    expect(repoHealth(undefined, 'offline').status).toBe('critical')
  })
})

describe('runs', () => {
  it('summarises GC, retention and verify stats (stats are JSON text)', () => {
    expect(runSummary(run({ stats: JSON.stringify({ scanned: 121, live: 118, kept_young: 3, kept_pinned: 0, removed: 2, bytes_reclaimed: 2048, quarantine_purged: 0, orphans_removed: 0, dry_run: true }) })))
      .toBe('Would remove 2 chunks (2 KiB) of 121 scanned; 3 kept as too young, 0 pinned')
    expect(runSummary(run({ kind: 'retention', stats: JSON.stringify({ groups: 21, kept: 98, removed: 2, protected: 1, plan: [], dry_run: false }) })))
      .toBe('Removed 2 snapshots, kept 98 (1 protected) in 21 objects')
    const v = run({ kind: 'scrub', stats: JSON.stringify({ chunks: 135, snapshots: 125, skipped: 0, quarantined: 1, problems: [{ kind: 'chunk-corrupt', object: 'x', detail: 'd' }], results: [] }) })
    expect(runSummary(v)).toBe('125 snapshots and 135 chunks checked; 1 problem, 1 quarantined')
    expect(runState(v)).toBe('warning')
  })
  it('a failed run shows its error; an unfinished one is running', () => {
    expect(runSummary(run({ status: 'failed', error: 'repository is offline' }))).toBe('repository is offline')
    const r = run({ finished: null, status: '' })
    expect(isRunning(r)).toBe(true)
    expect(runState(r)).toBe('running')
    expect(runSummary(r)).toBe('Running…')
  })
})

describe('check advice', () => {
  it('advises by check id and stays silent on passes', () => {
    expect(checkAdvice('conditional', 'fail')).toMatch(/If-None-Match/)
    expect(checkAdvice('delete', 'fail')).toMatch(/DeleteObject/)
    expect(checkAdvice('delete', 'pass')).toBe('')
  })
})

describe('rate limits (same units as the CLI)', () => {
  it('parses decimal, binary and bare units', () => {
    expect(parseRate('20MB')).toBe(20_000_000)
    expect(parseRate('20 MB/s')).toBe(20_000_000)
    expect(parseRate('500KiB')).toBe(512_000)
    expect(parseRate('1M')).toBe(1_048_576)
    expect(parseRate('1048576')).toBe(1_048_576)
    expect(parseRate('')).toBe(0)
    expect(parseRate('0')).toBe(0)
    expect(parseRate('fast')).toBeInstanceOf(Error)
    expect(parseRate('5 TB')).toBeInstanceOf(Error)
  })
  it('formats', () => {
    expect(formatRateLimit(0)).toBe('unlimited')
    expect(formatRateLimit(20_000_000)).toBe('20 MB/s')
    expect(formatRateLimit(512_000)).toBe('512 kB/s')
  })
})

describe('sync health', () => {
  const job = (o: Partial<SyncJob> = {}): SyncJob => ({ id: '1', name: 'j', direction: 'push', remote: 'x:8007', remote_repository: 'r', schedule: '02:00', group_filter: null, enabled: true, last_success: '2026-10-08T02:00:00Z', verified_only: false, resync_corrupt: false, transfer_last: 0, rate_limit_bps: 0, ...o })
  it('maps run outcomes and pause', () => {
    expect(syncHealth(job({ enabled: false }), 'failed').label).toBe('Paused')
    expect(syncHealth(job(), 'failed').status).toBe('critical')
    expect(syncHealth(job(), 'ok', 3 * 86_400_000, 86_400_000).label).toBe('Lagging')
    expect(syncHealth(job(), 'ok', 3_600_000, 86_400_000).label).toBe('In sync')
    expect(syncHealth(job({ last_success: null }), '').label).toBe('Never ran')
  })
})

describe('time zones', () => {
  it('lists UTC first and finds zones by city or offset', () => {
    const all = timeZones()
    expect(all[0]).toBe('UTC')
    expect(all).toContain('Atlantic/Reykjavik')
    expect(searchZones(all, 'reykj')).toEqual(['Atlantic/Reykjavik'])
    expect(searchZones(all, 'new york')).toContain('America/New_York')
    expect(zoneOffset('Atlantic/Reykjavik')).toBe('UTC+00:00')
    expect(zoneOffset('Asia/Kolkata')).toBe('UTC+05:30')
  })
})
