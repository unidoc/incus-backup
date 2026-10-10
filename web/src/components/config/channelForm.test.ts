import { describe, expect, it } from 'vitest'
import { emptyForm, formFromChannel, parseHeaders, toInput, validateForm } from './channelForm'
import type { Channel } from '@/api/sections/config'

describe('channel form', () => {
  it('builds a webhook with the URL as a secret and only webhook fields in config', () => {
    const f = emptyForm('webhook')
    Object.assign(f, { name: 'ops-hook', headers: 'X-Env: prod' })
    f.secret.url = 'https://hooks.example.com/x?token=t'
    f.secret.hmac_secret = 'k'
    expect(validateForm(f, true)).toEqual({})
    const b = toInput(f, true)
    expect(b.kind).toBe('webhook')
    expect(b.config).toEqual({ format: 'json', headers: { 'X-Env': 'prod' } })
    expect(b.secret).toEqual({ url: 'https://hooks.example.com/x?token=t', hmac_secret: 'k' })
  })
  it('keeps, replaces or removes the stored secret on edit', () => {
    const ch: Channel = { id: '1', name: 'mail', kind: 'smtp', config: { host: 'smtp.x', port: 587, tls: 'starttls', auth: '', username: '', from: 'a@x', to: ['b@x'] }, has_secret: true, target: '', enabled: true, rate_per_hour: 30, timeout_seconds: 15, created: '' }
    const f = formFromChannel(ch)
    expect(f.secretMode).toBe('keep')
    expect(toInput(f, false).secret).toBeUndefined()
    f.secretMode = 'remove'
    expect(toInput(f, false).secret).toEqual({})
    f.secretMode = 'replace'
    f.secret.password = 'pw'
    expect(toInput(f, false).secret).toEqual({ password: 'pw' })
    expect(toInput(f, false).kind).toBeUndefined()
  })
  it('refuses a password over plain SMTP and bad recipients', () => {
    const f = emptyForm('smtp')
    Object.assign(f, { name: 'm', host: 'smtp.x', tls: 'none', username: 'u', from: 'a@x', to: ['nope'] })
    const e = validateForm(f, true)
    expect(e.tls).toMatch(/unencrypted/)
    expect(e.to).toMatch(/e-mail/)
    expect(e['secret.password']).toBeTruthy()
  })
  it('parses header lines and reports the bad one', () => {
    expect(parseHeaders('A: 1\n\nB-c: x: y')).toEqual({ headers: { A: '1', 'B-c': 'x: y' }, error: '' })
    expect(parseHeaders('A 1').error).toMatch(/Line 1/)
  })
  it('sends gotify priorities only when set', () => {
    const f = emptyForm('gotify')
    Object.assign(f, { name: 'g', url: 'https://gotify.x' })
    f.secret.token = 't'
    f.priorities.critical = '10'
    expect(toInput(f, true).config).toEqual({ url: 'https://gotify.x', priorities: { critical: 10 } })
  })
})
