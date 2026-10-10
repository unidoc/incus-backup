import { describe, expect, it } from 'vitest'
import { compareValues, filterRows, nextSort, sortRows, toCSV, virtualWindow, type ColumnDef } from './table'

interface Row { name: string; size: number | null; host: string }
const rows: Row[] = [
  { name: 'web10', size: 300, host: 'alpha' },
  { name: 'web2', size: null, host: 'beta' },
  { name: 'db1', size: 1000, host: 'alpha' },
  { name: 'Web3', size: 5, host: 'gamma' },
]
const cols: ColumnDef<Row>[] = [
  { key: 'name', label: 'Name', sortable: true },
  { key: 'size', label: 'Size', sortable: true },
  { key: 'host', label: 'Host' },
]

describe('table logic', () => {
  it('sorts naturally and keeps empty values last in both directions', () => {
    expect(sortRows(rows, cols, { key: 'name', dir: 'asc' }).map((r) => r.name)).toEqual(['db1', 'web2', 'Web3', 'web10'])
    expect(sortRows(rows, cols, { key: 'size', dir: 'asc' }).map((r) => r.size)).toEqual([5, 300, 1000, null])
    expect(sortRows(rows, cols, { key: 'size', dir: 'desc' }).map((r) => r.size)).toEqual([1000, 300, 5, null])
    expect(sortRows(rows, cols, null)).toEqual(rows)
  })
  it('cycles sort asc -> desc -> none', () => {
    expect(nextSort(null, 'a')).toEqual({ key: 'a', dir: 'asc' })
    expect(nextSort({ key: 'a', dir: 'asc' }, 'a')).toEqual({ key: 'a', dir: 'desc' })
    expect(nextSort({ key: 'a', dir: 'desc' }, 'a')).toBeNull()
    expect(nextSort({ key: 'a', dir: 'desc' }, 'b')).toEqual({ key: 'b', dir: 'asc' })
  })
  it('compares mixed values', () => {
    expect(compareValues(2, 10)).toBeLessThan(0)
    expect(compareValues('a2', 'a10')).toBeLessThan(0)
    expect(compareValues(null, 'x')).toBeGreaterThan(0)
  })
  it('filters with AND terms, column scoping and negation', () => {
    expect(filterRows(rows, cols, 'web').length).toBe(3)
    expect(filterRows(rows, cols, 'web alpha').map((r) => r.name)).toEqual(['web10'])
    expect(filterRows(rows, cols, 'host:alpha').map((r) => r.name)).toEqual(['web10', 'db1'])
    expect(filterRows(rows, cols, '-alpha').map((r) => r.name)).toEqual(['web2', 'Web3'])
    expect(filterRows(rows, cols, '  ').length).toBe(4)
  })
  it('writes RFC 4180 CSV and neutralises formulas', () => {
    const csv = toCSV([{ name: 'a,"b"', size: -3, host: '=cmd()' }], cols)
    expect(csv).toBe('Name,Size,Host\r\n"a,""b""",-3,\'=cmd()\r\n')
  })
  it('computes the virtual window', () => {
    expect(virtualWindow(0, 360, 36, 1000, 5)).toEqual({ start: 0, end: 15, padTop: 0, padBottom: 985 * 36 })
    const w = virtualWindow(36 * 500, 360, 36, 1000, 5)
    expect(w.start).toBe(495)
    expect(w.end).toBe(515)
    expect(w.padTop + (w.end - w.start) * 36 + w.padBottom).toBe(36000)
    expect(virtualWindow(0, 100, 36, 0)).toEqual({ start: 0, end: 0, padTop: 0, padBottom: 0 })
  })
})
