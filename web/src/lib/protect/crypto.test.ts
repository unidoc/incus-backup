import { describe, expect, it } from 'vitest'
import { ApiError } from '@/api/client'
import { explainCryptoError, findRecoveryKey } from './crypto'

const KEY = 'IBR1-AAAAA-BBBBB-CCCCC-DDDDD-EEEEE-FFFFF-GGGGG-HHHHH-22222-33333-44444-5'
const err = (name: string, message: string) => Object.assign(new Error(message), { name })

describe('findRecoveryKey', () => {
  it('finds a key inside pasted text or a recovery sheet', () => {
    expect(findRecoveryKey(KEY).key).toBe(KEY)
    expect(findRecoveryKey(`Recovery key for custody:\n\n  ${KEY}\n\nKeep this offline.`).key).toBe(KEY)
    expect(findRecoveryKey(KEY.replace(/-/g, ' ')).key?.replace(/\s/g, '')).toBe(KEY.replace(/-/g, ''))
  })
  it('explains the host key file and escrow keys', () => {
    expect(findRecoveryKey('{"version":1,"protection":"passphrase","domain_id":"abcd"}').hint).toMatch(/incus-backup key recovery/)
    expect(findRecoveryKey('IBE1-xyz').hint).toMatch(/escrow/)
    expect(findRecoveryKey('hello').key).toBeNull()
  })
})

describe('explainCryptoError', () => {
  it('tells a wrong key from tampering by the domain id', () => {
    const e = err('IntegrityError', 'manifest authentication failed')
    expect(explainCryptoError(e, false).kind).toBe('wrong-key')
    expect(explainCryptoError(e, false).title).toBe('This key belongs to another repository')
    expect(explainCryptoError(e, true).kind).toBe('tampered')
    expect(explainCryptoError(err('IntegrityError', 'a/b: content does not match its recorded SHA-256'), true).title).toMatch(/not saved/)
  })
  it('explains key format errors, HTTP failures and module failures', () => {
    expect(explainCryptoError(err('KeyError', 'recovery key checksum mismatch (typo?)')).kind).toBe('key-format')
    expect(explainCryptoError(new ApiError(404, 'chunk not found', null, 'GET', '/chunks/x')).kind).toBe('missing')
    expect(explainCryptoError(new ApiError(503, 'offline', null, 'GET', '/x')).kind).toBe('maintenance')
    expect(explainCryptoError(new ApiError(403, 'no', null, 'GET', '/x')).kind).toBe('forbidden')
    expect(explainCryptoError(new Error('loading /assets/crypto-x/wasm/ibwasm.wasm: HTTP 404')).kind).toBe('module')
    expect(explainCryptoError(new DOMException('x', 'AbortError')).kind).toBe('aborted')
  })
})
