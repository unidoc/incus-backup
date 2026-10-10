import { describe, expect, it } from 'vitest'
import { fraction, thresholdStatus } from './meter'

describe('meter', () => {
  it('clamps and guards', () => {
    expect(fraction(50, 100)).toBe(0.5)
    expect(fraction(150, 100)).toBe(1)
    expect(fraction(-1, 100)).toBe(0)
    expect(fraction(5, 0)).toBe(0)
    expect(fraction(null, 10)).toBe(0)
  })
  it('applies thresholds both ways', () => {
    expect(thresholdStatus(0.5, { warn: 0.8, critical: 0.9 })).toBe('ok')
    expect(thresholdStatus(0.85, { warn: 0.8, critical: 0.9 })).toBe('warn')
    expect(thresholdStatus(0.9, { warn: 0.8, critical: 0.9 })).toBe('critical')
    expect(thresholdStatus(0.05, { warn: 0.2, critical: 0.1, invert: true })).toBe('critical')
    expect(thresholdStatus(0.15, { warn: 0.2, critical: 0.1, invert: true })).toBe('warn')
  })
})
