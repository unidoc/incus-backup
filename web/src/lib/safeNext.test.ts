import { describe, expect, it } from 'vitest'
import { safeNext } from './safeNext'

describe('safeNext', () => {
  it('keeps a path on this site', () => {
    expect(safeNext('/jobs?x=1')).toBe('/jobs?x=1')
    expect(safeNext('/')).toBe('/')
  })
  it('refuses other hosts, the API and non-strings', () => {
    for (const v of ['https://evil.example', '//evil.example', '/\\evil.example', '/api/v1/session', 'repos', '', undefined, ['/a'], null]) {
      expect(safeNext(v)).toBe('/')
    }
  })
  it('refuses every control character (a tab is dropped by browsers: /<tab>/evil = //evil)', () => {
    for (const v of ['/\t/evil.example', '/\n/evil.example', '/\r/evil.example', '/\x00/x', '/a\x1fb', '/a\x7fb']) {
      expect(safeNext(v)).toBe('/')
    }
  })
})
