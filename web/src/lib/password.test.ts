import { describe, expect, it } from 'vitest'
import { passwordStrength, MIN_PASSWORD } from './password'

describe('passwordStrength', () => {
  it('refuses what the server refuses (length < 12, counted in characters)', () => {
    expect(passwordStrength('short').acceptable).toBe(false)
    expect(passwordStrength('a'.repeat(MIN_PASSWORD - 1)).score).toBe(0)
    expect(passwordStrength('ááááááááááá').acceptable).toBe(false) // 11 characters, 22 bytes
    expect(passwordStrength('correct horse battery').acceptable).toBe(true)
  })
  it('never blocks an acceptable length, only advises', () => {
    const s = passwordStrength('passwordpassword')
    expect(s.acceptable).toBe(true)
    expect(s.score).toBeLessThanOrEqual(2)
    expect(s.hints.join(' ')).toMatch(/common words/)
  })
  it('rates long varied passphrases strong', () => {
    expect(passwordStrength('Tide-lantern-47-ochre-mosaic').score).toBe(4)
  })
  it('flags the user name', () => {
    const s = passwordStrength('gunnsteinn-was-here-2026', ['gunnsteinn'])
    expect(s.score).toBe(1)
    expect(s.hints).toContain('Do not include your user name.')
  })
})
