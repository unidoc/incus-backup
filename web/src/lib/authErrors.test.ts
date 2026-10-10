import { describe, expect, it } from 'vitest'
import { ApiError } from '@/api/client'
import { crossSiteHint, passwordCheckError, signInError } from './authErrors'

const api = (status: number, msg: string) => new ApiError(status, msg, null, 'POST', '/session')

describe('auth error messages', () => {
  it('explains a refused cross-site sign-in or setup with what to do', () => {
    const e = api(403, 'cross-site request refused: send JSON from this origin')
    expect(crossSiteHint(e)).toMatch(/server's own address/)
    expect(signInError(e)).toMatch(/cross-site request/)
    expect(crossSiteHint(api(403, 'your role (readonly) may not do this'))).toBeNull()
    expect(crossSiteHint(new Error('x'))).toBeNull()
  })
  it('sign-in: wrong password, wrong code, lock', () => {
    expect(signInError(api(401, 'invalid credentials'))).toBe('The name or password is wrong.')
    expect(signInError(api(401, 'invalid credentials'), 'code')).toMatch(/30 seconds/)
    expect(signInError(api(429, 'too many failed attempts: try again later'))).toMatch(/locked for a few minutes/)
  })
  it('profile: the lock after wrong current passwords is spelled out', () => {
    const m = passwordCheckError(api(429, 'too many failed attempts: try again later'))
    expect(m).toMatch(/Too many wrong passwords/)
    expect(m).toMatch(/stay signed in/)
    expect(passwordCheckError(api(401, 'The password is wrong.'))).toMatch(/five wrong passwords/)
    expect(passwordCheckError(api(403, 'missing or wrong CSRF token'))).toMatch(/Reload/)
    expect(passwordCheckError(api(400, 'password too weak'))).toBe('password too weak')
  })
})
