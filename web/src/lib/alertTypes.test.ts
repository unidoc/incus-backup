import { describe, expect, it } from 'vitest'
import { alertOrder, alertType, shortAge, statusOfAlert, ALERT_TYPES, ALERT_GROUPS } from './alertTypes'
import { countAlerts } from '@/components/config/alertCounts'

describe('alert types', () => {
  it('puts open before resolved, critical before warning, newest first', () => {
    const a = [
      { id: 1, state: 'resolved', severity: 'critical', last_change: '2026-10-08T10:00:00Z' },
      { id: 2, state: 'firing', severity: 'warning', last_change: '2026-10-08T11:00:00Z' },
      { id: 3, state: 'acknowledged', severity: 'critical', last_change: '2026-10-08T09:00:00Z' },
      { id: 4, state: 'firing', severity: 'warning', last_change: '2026-10-08T12:00:00Z' },
    ]
    expect([...a].sort(alertOrder).map((x) => x.id)).toEqual([3, 4, 2, 1])
  })
  it('maps states to status colours', () => {
    expect(statusOfAlert('firing', 'critical')).toBe('critical')
    expect(statusOfAlert('firing', 'warning')).toBe('warn')
    expect(statusOfAlert('acknowledged', 'critical')).toBe('info')
    expect(statusOfAlert('resolved', 'critical')).toBe('ok')
    expect(statusOfAlert('silenced', 'warning')).toBe('neutral')
  })
  it('formats ages', () => {
    expect(shortAge(30_000)).toBe('just now')
    expect(shortAge(5 * 60_000)).toBe('5 min')
    expect(shortAge(125 * 60_000)).toBe('2 h 5 min')
    expect(shortAge(30 * 3600_000)).toBe('30 h')
    expect(shortAge(5 * 86400_000)).toBe('5 d')
  })
  it('groups every type and has a fallback', () => {
    for (const t of ALERT_TYPES) expect(ALERT_GROUPS).toContain(t.group)
    expect(alertType('something_new').group).toBe('Other')
  })
  it('counts only firing critical/warning for the badge', () => {
    expect(countAlerts([{ state: 'firing', severity: 'critical' }, { state: 'firing', severity: 'warning' }, { state: 'firing', severity: 'info' }, { state: 'acknowledged', severity: 'critical' }, { state: 'silenced', severity: 'warning' }]))
      .toEqual({ critical: 1, warning: 1, acknowledged: 1, silenced: 1 })
  })
})
