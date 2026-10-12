import { describe, expect, it } from 'vitest'
import { areaPath, byteTicks, linePath, linearScale, nearestIndex, niceStep, niceTicks, timeTicks } from './chart'

describe('chart maths', () => {
  it('maps and inverts linear scales', () => {
    const s = linearScale([0, 100], [200, 0])
    expect(s(0)).toBe(200)
    expect(s(25)).toBe(150)
    expect(s.invert(150)).toBe(25)
    expect(linearScale([5, 5], [0, 10])(5)).toBe(0) // zero span does not divide by zero
  })
  it('chooses nice steps and ticks', () => {
    expect(niceStep(0.7)).toBe(1)
    expect(niceStep(23)).toBe(25)
    expect(niceStep(380)).toBe(500)
    expect(niceTicks(0, 87, 4)).toEqual({ ticks: [0, 25, 50, 75, 100], domain: [0, 100] })
    expect(niceTicks(3, 3).domain[0]).toBe(0)
    expect(niceTicks(0, 0).ticks).toEqual([0, 0.25, 0.5, 0.75, 1])
  })
  it('makes byte ticks on binary units', () => {
    const t = byteTicks(3.4 * 1024 ** 3, 4)
    expect(t.ticks[1]).toBe(1024 ** 3)
    expect(t.domain[1]).toBe(4 * 1024 ** 3)
  })
  it('makes day ticks at midnight for multi-day ranges', () => {
    const t0 = new Date(2026, 9, 1, 13).getTime()
    const t1 = new Date(2026, 9, 6, 9).getTime()
    const ticks = timeTicks(t0, t1, 6)
    expect(ticks.length).toBe(5)
    expect(new Date(ticks[0]).getHours()).toBe(0)
    expect(ticks.every((t) => t >= t0 && t <= t1)).toBe(true)
  })
  it('builds paths with gaps', () => {
    expect(linePath([{ x: 0, y: 1 }, { x: 1, y: 2 }, null, { x: 3, y: 3 }])).toBe('M0,1L1,2M3,3')
    expect(areaPath([{ x: 0, y: 1 }, { x: 2, y: 2 }], 10)).toBe('M0,10L0,1L2,2L2,10Z')
  })
  it('finds the nearest index', () => {
    expect(nearestIndex([0, 10, 20, 30], 14)).toBe(1)
    expect(nearestIndex([0, 10, 20, 30], 16)).toBe(2)
    expect(nearestIndex([0, 10], -5)).toBe(0)
    expect(nearestIndex([], 3)).toBe(-1)
  })
})
