import { describe, expect, it } from 'vitest'
import vectors from './retention.vectors.json'
import { describeRules, isoWeek, plan, retentionSummary, simulate, type Rules } from './retention'

type Vec = { rules: number[]; tz: string; snaps: Array<[string, number, number]>; plan: Array<[string, number, string]> }
const R = (last = 0, hourly = 0, daily = 0, weekly = 0, monthly = 0, yearly = 0): Rules => ({ last, hourly, daily, weekly, monthly, yearly })

describe('retention plan (port of internal/retention.Plan)', () => {
  it('matches every vector computed by the Go code', () => {
    const vs = vectors as unknown as Vec[]
    expect(vs.length).toBeGreaterThan(100)
    let decisions = 0
    for (const [i, v] of vs.entries()) {
      const got = plan(v.snaps.map(([id, t, p]) => ({ id, time: t * 1000, protected: p === 1 })), R(...v.rules), v.tz)
      const want = v.plan.map(([id, keep, reason]) => ({ id, keep: keep === 1, reason }))
      expect(got.map((d) => ({ id: d.id, keep: d.keep, reason: d.reason })), `vector ${i} (${v.tz}, ${v.rules})`).toEqual(want)
      decisions += want.length
    }
    expect(decisions).toBeGreaterThan(3000)
  })

  it('is cumulative like PBS: one backup a day with last 3 + daily 7 keeps 10', () => {
    const day = 86_400_000
    const snaps = Array.from({ length: 30 }, (_, i) => ({ id: `s${i}`, time: Date.UTC(2026, 0, 1, 3) + i * day }))
    expect(plan(snaps, R(3, 0, 7)).filter((d) => d.keep).length).toBe(10)
  })

  it('keeps the newest always, and everything without rules', () => {
    const snaps = [{ id: 'a', time: 1000 }, { id: 'b', time: 2000 }]
    expect(plan(snaps, R()).every((d) => d.keep && d.reason === 'retention disabled')).toBe(true)
    const p = plan([{ id: 'a', time: 1000 }, { id: 'b', time: 2000, protected: true }], R(0, 0, 0, 0, 0, 1))
    expect(p.map((d) => [d.id, d.keep, d.reason])).toEqual([['b', true, 'protected'], ['a', true, 'yearly']])
  })

  it('cuts days in the policy time zone', () => {
    // 23:30 and 00:30 UTC are two UTC days but one day in New York
    const snaps = [{ id: 'x', time: Date.UTC(2026, 5, 1, 23, 30) }, { id: 'y', time: Date.UTC(2026, 5, 2, 0, 30) }]
    expect(plan(snaps, R(0, 0, 2), 'UTC').filter((d) => d.keep).length).toBe(2)
    expect(plan(snaps, R(0, 0, 2), 'America/New_York').filter((d) => d.keep).length).toBe(1)
  })

  it('skips snapshots a weekly rule walked past (PBS remove mark) when it reaches monthly', () => {
    // one backup a day, 1 Apr .. 9 Jul 2026; ISO week 27 = 29 Jun .. 5 Jul
    const day = 86_400_000
    const snaps = Array.from({ length: 100 }, (_, i) => ({ id: new Date(Date.UTC(2026, 3, 1, 12) + i * day).toISOString().slice(0, 10), time: Date.UTC(2026, 3, 1, 12) + i * day }))
      .filter((x) => x.id <= '2026-07-09')
    const kept = plan(snaps, R(0, 0, 0, 2, 3)).filter((d) => d.keep).map((d) => `${d.id}:${d.reason}`)
    expect(kept).toEqual(['2026-07-09:weekly', '2026-07-05:weekly', '2026-06-28:monthly', '2026-05-31:monthly', '2026-04-30:monthly'])
  })

  it('counts keep-last per snapshot even when two share a time', () => {
    const p = plan([{ id: 'a', time: 5000 }, { id: 'b', time: 5000 }, { id: 'c', time: 1000 }], R(2))
    expect(p.map((d) => [d.id, d.keep])).toEqual([['b', true], ['a', true], ['c', false]])
  })

  it('computes ISO weeks like Go', () => {
    expect(isoWeek(2026, 1, 1)).toEqual([2026, 1])
    expect(isoWeek(2027, 1, 1)).toEqual([2026, 53])
    expect(isoWeek(2024, 12, 30)).toEqual([2025, 1])
  })
})

describe('retention text and simulation', () => {
  it('describes rules in plain English', () => {
    expect(describeRules(R(0, 0, 7, 4))).toBe('7 daily and 4 weekly')
    expect(retentionSummary(R(3, 0, 7, 4, 12))).toBe('Keeps the last 3, 7 daily, 4 weekly and 12 monthly.')
    expect(retentionSummary(R())).toBe('Keeps everything (no retention rule).')
  })
  it('simulates a daily schedule', () => {
    const now = Date.UTC(2026, 9, 8, 3)
    const s = simulate(R(0, 0, 7, 4), 'UTC', 86_400_000, now, 120)
    // 7 daily (Oct 2-8 span ISO weeks 40 and 41) + 4 weekly from weeks 36-39 (cumulative rules)
    expect(s.kept).toBe(11)
    expect(s.byRule.daily).toBe(7)
    expect(s.reach).toBeGreaterThan(20 * 86_400_000)
  })
})
