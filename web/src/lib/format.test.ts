import { describe, expect, it } from 'vitest'
import {
  describeSchedule, formatBytes, formatDate, formatDateTime, formatDuration, formatNumber, formatPercent, formatRatio,
  formatRelative, nextRun, parseGoDuration, parseSchedule, plural, toDate,
} from './format'

describe('formatBytes', () => {
  it('uses binary units with at most three significant digits', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1023)).toBe('1023 B')
    expect(formatBytes(1024)).toBe('1 KiB')
    expect(formatBytes(1536)).toBe('1.5 KiB')
    expect(formatBytes(10.25 * 1024 ** 3)).toBe('10.3 GiB')
    expect(formatBytes(123.4 * 1024 ** 4)).toBe('123 TiB')
    expect(formatBytes(1023.99 * 1024)).toBe('1 MiB') // rounding up crosses the unit
  })
  it('handles signs and missing values', () => {
    expect(formatBytes(-2048)).toBe('−2 KiB')
    expect(formatBytes(2048, { signed: true })).toBe('+2 KiB')
    expect(formatBytes(null)).toBe('–')
    expect(formatBytes(NaN)).toBe('–')
  })
})

describe('numbers', () => {
  it('formats counts, percents and ratios', () => {
    expect(formatNumber(1234567)).toBe('1,234,567')
    expect(formatPercent(0.4567)).toBe('45.7 %')
    expect(formatPercent(1)).toBe('100 %')
    expect(formatPercent(0)).toBe('0 %')
    expect(formatRatio(3.24)).toBe('3.2×')
    expect(formatRatio(0)).toBe('–')
    expect(plural(1, 'object')).toBe('1 object')
    expect(plural(2, 'policy', 'policies')).toBe('2 policies')
  })
})

describe('formatDuration', () => {
  it('shows the two largest units', () => {
    expect(formatDuration(0)).toBe('0 ms')
    expect(formatDuration(950)).toBe('950 ms')
    expect(formatDuration(1000)).toBe('1 s')
    expect(formatDuration(61_000)).toBe('1 min 1 s')
    expect(formatDuration(3_600_000)).toBe('1 h')
    expect(formatDuration(3_605_000)).toBe('1 h') // a skipped unit ends the run
    expect(formatDuration(90_061_000)).toBe('1 d 1 h')
    expect(formatDuration(90_061_000, { units: 3 })).toBe('1 d 1 h 1 min')
    expect(formatDuration(-5000)).toBe('−5 s')
    expect(formatDuration(undefined)).toBe('–')
  })
})

describe('time', () => {
  const now = Date.parse('2026-10-08T12:00:00Z')
  it('treats Go zero time as never', () => {
    expect(toDate('0001-01-01T00:00:00Z')).toBeNull()
    expect(formatRelative('0001-01-01T00:00:00Z', now)).toBe('never')
    expect(formatRelative(null, now, 'not yet')).toBe('not yet')
  })
  it('formats relative times', () => {
    expect(formatRelative('2026-10-08T11:59:40Z', now)).toBe('just now')
    expect(formatRelative('2026-10-08T12:00:20Z', now)).toBe('just now')
    expect(formatRelative('2026-10-08T11:57:00Z', now)).toBe('3 min ago')
    expect(formatRelative('2026-10-08T14:00:00Z', now)).toBe('in 2 h')
    expect(formatRelative('2026-10-07T12:00:00Z', now)).toBe('yesterday')
    expect(formatRelative('2026-10-03T12:00:00Z', now)).toBe('5 d ago')
    expect(formatRelative('2026-08-01T12:00:00Z', now)).toMatch(/^2026-0[78]-\d\d$/)
  })
  it('formats absolute times in a given zone', () => {
    expect(formatDateTime('2026-10-08T12:03:04Z', { tz: 'UTC', seconds: true })).toBe('2026-10-08 12:03:04')
    expect(formatDateTime('2026-10-08T23:30:00Z', { tz: 'Asia/Tokyo' })).toBe('2026-10-09 08:30')
    expect(formatDate('2026-10-08T12:00:00Z', 'UTC')).toBe('2026-10-08')
    expect(formatDateTime('garbage')).toBe('–')
  })
})

describe('schedules (internal/schedule)', () => {
  it('parses Go durations', () => {
    expect(parseGoDuration('4h')).toBe(4 * 3_600_000)
    expect(parseGoDuration('1h30m')).toBe(90 * 60_000)
    expect(parseGoDuration('1.5h')).toBe(90 * 60_000)
    expect(parseGoDuration('90x')).toBeNull()
    expect(parseGoDuration('')).toBeNull()
  })
  it('accepts exactly what the server accepts', () => {
    for (const ok of ['', 'manual', 'never', 'hourly', 'daily', 'weekly', 'monthly', 'every 4h', 'every 30m', 'every 1m', 'every 24h', '03:00', '0:5', 'mon 02:30', 'SUN 23:59', '  daily  '])
      expect(parseSchedule(ok), ok).not.toBeInstanceOf(Error)
    for (const bad of ['every 30s', 'every 25h', 'every x', 'funday 02:00', '24:00', '12:60', '1200', 'mon tue 02:00', 'a:b'])
      expect(parseSchedule(bad), bad).toBeInstanceOf(Error)
  })
  it('describes schedules in plain English', () => {
    expect(describeSchedule('')).toBe('Manual only')
    expect(describeSchedule('hourly')).toBe('Every hour, on the hour')
    expect(describeSchedule('every 4h')).toBe('Every 4 hours from midnight')
    expect(describeSchedule('every 15m')).toBe('Every 15 minutes')
    expect(describeSchedule('every 90m')).toBe('Every 1 h 30 min from midnight')
    expect(describeSchedule('daily')).toBe('Every day at 03:00')
    expect(describeSchedule('03:30', 'Europe/Berlin')).toBe('Every day at 03:30 (Europe/Berlin)')
    expect(describeSchedule('weekly')).toBe('Sundays at 03:00')
    expect(describeSchedule('mon 02:30')).toBe('Mondays at 02:30')
    expect(describeSchedule('monthly')).toBe('1st of every month at 03:00')
    expect(describeSchedule('every 30s')).toMatch(/^Invalid schedule: invalid interval/)
  })
  it('computes the next run like schedule.Spec.Next', () => {
    const from = new Date(2026, 9, 8, 13, 10) // Thursday
    expect(nextRun('every 4h', from)).toEqual(new Date(2026, 9, 8, 16, 0))
    expect(nextRun('every 4h', new Date(2026, 9, 8, 22, 0))).toEqual(new Date(2026, 9, 9, 0, 0))
    expect(nextRun('03:00', from)).toEqual(new Date(2026, 9, 9, 3, 0))
    expect(nextRun('mon 02:30', from)).toEqual(new Date(2026, 9, 12, 2, 30))
    expect(nextRun('monthly', from)).toEqual(new Date(2026, 10, 1, 3, 0))
    expect(nextRun('manual', from)).toBeNull()
  })
})
