// Threshold logic shared by Meter and Gauge.
import type { Status } from './status'

export interface Thresholds { warn?: number; critical?: number; /** true when low values are bad (free space). */ invert?: boolean }

/** Fraction 0..1 of value in [0, max]; NaN-safe. */
export function fraction(value: number | null | undefined, max: number | null | undefined): number {
  if (value === null || value === undefined || !max || !Number.isFinite(value) || !Number.isFinite(max) || max <= 0) return 0
  return Math.min(1, Math.max(0, value / max))
}

/** Status of a fraction against thresholds (fractions, e.g. warn .8 critical .9). */
export function thresholdStatus(f: number, t: Thresholds = {}): Status {
  const { warn, critical, invert } = t
  if (invert) {
    if (critical !== undefined && f <= critical) return 'critical'
    if (warn !== undefined && f <= warn) return 'warn'
    return 'ok'
  }
  if (critical !== undefined && f >= critical) return 'critical'
  if (warn !== undefined && f >= warn) return 'warn'
  return 'ok'
}
