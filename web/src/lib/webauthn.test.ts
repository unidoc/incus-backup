import { describe, expect, it } from 'vitest'
import { b64urlToBytes, bytesToB64url, creationOptions, requestOptions, credentialJSON, webauthnError, WebAuthnUnavailable } from './webauthn'

describe('webauthn helper', () => {
  it('round-trips base64url without padding', () => {
    for (const n of [0, 1, 2, 3, 31, 32, 33, 64]) {
      const b = new Uint8Array(n).map((_, i) => (i * 37 + 250) & 255)
      const s = bytesToB64url(b)
      expect(s).not.toMatch(/[+/=]/)
      expect(Array.from(b64urlToBytes(s))).toEqual(Array.from(b))
    }
    expect(bytesToB64url(new Uint8Array([251, 255]))).toBe('-_8')
  })

  it('turns the server creation options into binary fields', () => {
    const o = creationOptions({ publicKey: { challenge: 'AQID', rp: { id: 'example.com', name: 'x' }, user: { id: 'BAUG', name: 'anna', displayName: 'Anna' }, pubKeyCredParams: [{ type: 'public-key', alg: -7 }], excludeCredentials: [{ type: 'public-key', id: 'BwgJ', transports: ['usb'] }] } })
    expect(Array.from(new Uint8Array(o.challenge as ArrayBuffer))).toEqual([1, 2, 3])
    expect(Array.from(new Uint8Array(o.user.id as ArrayBuffer))).toEqual([4, 5, 6])
    expect(Array.from(new Uint8Array(o.excludeCredentials![0].id as ArrayBuffer))).toEqual([7, 8, 9])
    expect(o.rp.id).toBe('example.com')
  })

  it('turns request options into binary fields, no allow list for a passkey', () => {
    const o = requestOptions({ publicKey: { challenge: 'AQID', rpId: 'example.com', userVerification: 'required' } })
    expect(o.allowCredentials).toBeUndefined()
    expect(o.userVerification).toBe('required')
  })

  it('serialises an assertion like go-webauthn expects', () => {
    const buf = (a: number[]) => new Uint8Array(a).buffer
    const cred = {
      id: 'Bwg', rawId: buf([7, 8]), type: 'public-key', authenticatorAttachment: 'cross-platform',
      getClientExtensionResults: () => ({}),
      response: { clientDataJSON: buf([1]), authenticatorData: buf([2]), signature: buf([3]), userHandle: buf([4]) },
    } as unknown as PublicKeyCredential
    expect(credentialJSON(cred)).toEqual({ id: 'Bwg', rawId: 'Bwg', type: 'public-key', authenticatorAttachment: 'cross-platform', clientExtensionResults: {}, response: { clientDataJSON: 'AQ', authenticatorData: 'Ag', signature: 'Aw', userHandle: 'BA' } })
  })

  it('explains what the browser threw', () => {
    const err = (name: string) => Object.assign(new Error('x'), { name })
    expect(webauthnError(err('NotAllowedError'), 'register')).toMatch(/cancelled or timed out/)
    expect(webauthnError(err('InvalidStateError'))).toMatch(/already registered/)
    expect(webauthnError(err('SecurityError'))).toMatch(/does not match the passkey domain/)
    expect(webauthnError(new WebAuthnUnavailable('needs HTTPS'))).toBe('needs HTTPS')
  })
})
