import { describe, expect, it } from 'vitest'
import { authn, ssoErrorText } from './authn'

describe('authn section', () => {
  it('builds the single sign-on link (a top-level navigation)', () => {
    expect(authn.ssoURL('corp')).toBe('/api/v1/auth/oidc/corp/login')
    expect(authn.ssoURL('corp', '/repos?x=1')).toBe('/api/v1/auth/oidc/corp/login?next=%2Frepos%3Fx%3D1')
  })
  it('explains every error word the server sends, and unknown ones generically', () => {
    for (const c of ['state', 'denied', 'unverified', 'domain', 'no_account', 'exists', 'disabled', 'locked', 'provider', 'config']) {
      expect(ssoErrorText(c)).not.toMatch(/Single sign-on failed\. Try again/)
    }
    expect(ssoErrorText('<script>')).toMatch(/Single sign-on failed/)
  })
})
