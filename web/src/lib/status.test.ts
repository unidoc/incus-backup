import { describe, expect, it } from 'vitest'
import { statusOf, worst } from './status'

describe('status vocabulary', () => {
  it('maps server words to the five statuses', () => {
    expect(statusOf('ok')).toBe('ok')
    expect(statusOf('partial')).toBe('warn')
    expect(statusOf('FAILED')).toBe('critical')
    expect(statusOf('running')).toBe('info')
    expect(statusOf('read-only')).toBe('warn')
    expect(statusOf('something new')).toBe('neutral')
    expect(statusOf(null)).toBe('neutral')
  })
  it('rolls up to the worst', () => {
    expect(worst('ok', 'warn', 'info')).toBe('warn')
    expect(worst('ok', 'critical', 'warn')).toBe('critical')
    expect(worst()).toBe('ok')
  })
})
