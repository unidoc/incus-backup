import { describe, expect, it } from 'vitest'
import type { RestoreTest } from '@/api/sections/protect'
import { defaultMethod, displayParams, jobSubject, keyToBase64, parsePaths, restoreConfidence, schedulePeriodMs, testHealth } from './jobs'

const NOW = Date.parse('2026-10-08T12:00:00Z')
const rt = (state: string, finished: string): RestoreTest => ({
  id: '1', repository: 'r', host: 'h', object: 'default/volume/x', snapshot_id: 's', method: 'deep', state, started: finished, finished,
  duration_ms: 1, bytes: 1, files: 0, details: null,
})

describe('job subjects', () => {
  it('describes every type from its params', () => {
    expect(jobSubject({ type: 'restore_test', params: { project: 'default', object: 'volume/x', mode: 'full' } })).toBe('default/volume/x (full)')
    expect(jobSubject({ type: 'deep_verify', params: { last: 3 } })).toBe('every object, last 3 snapshots')
    expect(jobSubject({ type: 'restore_instance', params: { project: 'p', instance: 'web', as_name: 'web-restored', snapshot: '20261001030000-00000001' } }))
      .toBe('p/web @ 20261001030000-00000001 → p/web-restored')
    expect(jobSubject({ type: 'restore_volume', params: { project: 'p', object: 'volume/d', pool: 'tank', as_name: 'd2' } })).toBe('p/volume/d → p/d2 on tank')
    expect(jobSubject({ type: 'fs_extract', params: { project: 'p', object: 'volume/d', paths: ['a', 'b'] } })).toBe('p/volume/d: 2 paths')
    expect(jobSubject({ type: 'backup_now', params: { policy: 'nightly' } })).toBe('policy nightly')
  })
  it('shortens the recipient key in the params viewer', () => {
    const p = displayParams({ recipient_public_key: 'QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVpbXF1eX2A=', paths: ['/etc'] })
    expect(String(p.recipient_public_key)).toMatch(/^QUJDREVG… /)
    expect(p.paths).toEqual(['/etc'])
  })
})

describe('restore-test coverage', () => {
  it('derives the period of a schedule', () => {
    expect(schedulePeriodMs('monthly')).toBe(31 * 86_400_000)
    expect(schedulePeriodMs('weekly')).toBe(7 * 86_400_000)
    expect(schedulePeriodMs('03:00')).toBe(86_400_000)
    expect(schedulePeriodMs('every 6h')).toBe(6 * 3_600_000)
    expect(schedulePeriodMs('manual')).toBeNull()
    expect(schedulePeriodMs('bogus')).toBeNull()
  })
  it('classifies the latest test against the schedule', () => {
    const month = 31 * 86_400_000
    expect(testHealth(undefined, month, NOW)).toBe('never')
    expect(testHealth(undefined, null, NOW)).toBe('unscheduled')
    expect(testHealth(rt('failed', '2026-10-08T00:00:00Z'), month, NOW)).toBe('failed')
    expect(testHealth(rt('passed', '2026-10-01T00:00:00Z'), month, NOW)).toBe('passed')
    expect(testHealth(rt('passed', '2026-08-01T00:00:00Z'), month, NOW)).toBe('overdue')
    expect(testHealth(rt('passed', '2025-01-01T00:00:00Z'), null, NOW)).toBe('passed') // no schedule: not overdue
  })
  it('rolls up confidence', () => {
    expect(restoreConfidence(['passed', 'passed', 'passed'])).toMatchObject({ score: 1, level: 'ok' })
    expect(restoreConfidence(['passed', 'overdue'])).toMatchObject({ score: 0.5, level: 'warn', overdue: 1 })
    expect(restoreConfidence(['passed', 'passed', 'failed'])).toMatchObject({ level: 'critical', failed: 1 })
    expect(restoreConfidence(['never', 'never', 'passed'])).toMatchObject({ level: 'critical', never: 2 })
    expect(restoreConfidence(['unscheduled'])).toMatchObject({ score: null, level: 'neutral' })
  })
  it('picks the method the host would', () => {
    expect(defaultMethod('filesystem')).toBe('extract')
    expect(defaultMethod('block')).toBe('deep')
  })
})

describe('inputs', () => {
  it('encodes a key as standard base64', () => {
    expect(keyToBase64(new Uint8Array([0xfb, 0xff, 0x01]))).toBe('+/8B')
  })
  it('parses guest and filesystem paths', () => {
    expect(parsePaths('etc/nginx\n/var//log/\n\n/etc/nginx', true)).toEqual({ paths: ['/etc/nginx', '/var/log'] })
    expect(parsePaths('/docs/\nphotos', false)).toEqual({ paths: ['docs', 'photos'] })
    expect(parsePaths('/', true).error).toMatch(/whole disk/)
    expect(parsePaths('a/../b', false).error).toMatch(/not allowed/)
    expect(parsePaths(Array.from({ length: 257 }, (_, i) => `f${i}`).join('\n'), false).error).toMatch(/256/)
  })
})
