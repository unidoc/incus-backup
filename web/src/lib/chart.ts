// Minimal chart maths for the SVG charts (no chart library): linear scales, "nice" ticks, time ticks,
// path builders, nearest-point lookup. Unit-tested in chart.test.ts.

export interface Scale { (v: number): number; domain: [number, number]; range: [number, number]; invert: (px: number) => number }

export function linearScale(domain: [number, number], range: [number, number]): Scale {
  const [d0, d1] = domain
  const [r0, r1] = range
  const span = d1 - d0 || 1
  const f = ((v: number) => r0 + ((v - d0) / span) * (r1 - r0)) as Scale
  f.domain = domain
  f.range = range
  f.invert = (px: number) => d0 + ((px - r0) / (r1 - r0 || 1)) * span
  return f
}

/** Round a step to 1, 2, 2.5 or 5 × 10^n. */
export function niceStep(rough: number): number {
  if (!Number.isFinite(rough) || rough <= 0) return 1
  const p = 10 ** Math.floor(Math.log10(rough))
  const f = rough / p
  const n = f <= 1 ? 1 : f <= 2 ? 2 : f <= 2.5 ? 2.5 : f <= 5 ? 5 : 10
  return n * p
}

/** Ticks covering [min, max] with about `count` steps; the domain is extended to the outer ticks. */
export function niceTicks(min: number, max: number, count = 4): { ticks: number[]; domain: [number, number] } {
  if (!Number.isFinite(min) || !Number.isFinite(max)) return { ticks: [0, 1], domain: [0, 1] }
  if (min === max) { max = min === 0 ? 1 : min + Math.abs(min) * 0.5; if (min > 0) min = 0 }
  const step = niceStep((max - min) / count)
  const lo = Math.floor(min / step) * step
  const hi = Math.ceil(max / step) * step
  const ticks: number[] = []
  for (let v = lo; v <= hi + step / 2; v += step) ticks.push(+v.toPrecision(12))
  return { ticks, domain: [lo, hi] }
}

/** Binary-unit friendly ticks for byte axes (steps of 1/2/5 × 1024^k). */
export function byteTicks(max: number, count = 4): { ticks: number[]; domain: [number, number] } {
  if (!Number.isFinite(max) || max <= 0) return { ticks: [0, 1024], domain: [0, 1024] }
  let unit = 1
  while (max / unit >= 1024 && unit < 1024 ** 6) unit *= 1024
  const step = niceStep(max / unit / count) * unit
  const hi = Math.ceil(max / step) * step
  const ticks: number[] = []
  for (let v = 0; v <= hi + step / 2; v += step) ticks.push(v)
  return { ticks, domain: [0, hi] }
}

const DAY = 86_400_000
/** Time ticks for a [t0, t1] range (ms): whole days, hours or weeks, at most ~`count` of them. */
export function timeTicks(t0: number, t1: number, count = 6): number[] {
  const span = t1 - t0
  if (!(span > 0)) return [t0]
  const steps = [3_600_000, 3 * 3_600_000, 6 * 3_600_000, 12 * 3_600_000, DAY, 2 * DAY, 7 * DAY, 14 * DAY, 30 * DAY, 90 * DAY]
  const step = steps.find((s) => span / s <= count) ?? 180 * DAY
  const out: number[] = []
  const start = new Date(t0)
  if (step >= DAY) start.setHours(0, 0, 0, 0)
  else start.setMinutes(0, 0, 0)
  let t = start.getTime()
  while (t < t0) t += step
  for (; t <= t1; t += step) out.push(t)
  return out
}

export interface Pt { x: number; y: number }

/** SVG path for a polyline; gaps (null y) break the line. */
export function linePath(pts: Array<Pt | null>): string {
  let d = ''
  let pen = false
  for (const p of pts) {
    if (!p) { pen = false; continue }
    d += `${pen ? 'L' : 'M'}${round(p.x)},${round(p.y)}`
    pen = true
  }
  return d
}

/** Closed area under a polyline down to baseline y0 (gaps split into separate areas). */
export function areaPath(pts: Array<Pt | null>, y0: number): string {
  const runs: Pt[][] = []
  let cur: Pt[] = []
  for (const p of pts) {
    if (p) cur.push(p)
    else if (cur.length) { runs.push(cur); cur = [] }
  }
  if (cur.length) runs.push(cur)
  return runs.map((r) => `M${round(r[0].x)},${round(y0)}` + r.map((p) => `L${round(p.x)},${round(p.y)}`).join('') + `L${round(r[r.length - 1].x)},${round(y0)}Z`).join('')
}

/** Index of the x closest to px (xs ascending). */
export function nearestIndex(xs: number[], px: number): number {
  if (!xs.length) return -1
  let lo = 0
  let hi = xs.length - 1
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1
    if (xs[mid] < px) lo = mid
    else hi = mid
  }
  return Math.abs(xs[lo] - px) <= Math.abs(xs[hi] - px) ? lo : hi
}

const round = (n: number) => Math.round(n * 10) / 10
