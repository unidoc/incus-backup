// RFC 6238 TOTP (SHA-1, 6 digits, 30 s) computed in the test from the enrolment secret, independent of
// the server's implementation (internal/server/auth/totp.go).
import { createHmac } from 'node:crypto'

export function base32Decode(s: string): Buffer {
  const alpha = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  const clean = s.toUpperCase().replace(/=+$/, '').replace(/\s/g, '')
  let bits = 0
  let value = 0
  const out: number[] = []
  for (const ch of clean) {
    const i = alpha.indexOf(ch)
    if (i < 0) throw new Error(`bad base32 character ${ch}`)
    value = (value << 5) | i
    bits += 5
    if (bits >= 8) { out.push((value >>> (bits - 8)) & 0xff); bits -= 8 }
  }
  return Buffer.from(out)
}

export function totp(secret: string, step: number): string {
  const msg = Buffer.alloc(8)
  msg.writeBigUInt64BE(BigInt(step))
  const h = createHmac('sha1', base32Decode(secret)).update(msg).digest()
  const off = h[h.length - 1] & 0x0f
  const v = (h.readUInt32BE(off) & 0x7fffffff) % 1_000_000
  return String(v).padStart(6, '0')
}

export const currentStep = (now = Date.now()) => Math.floor(now / 1000 / 30)
