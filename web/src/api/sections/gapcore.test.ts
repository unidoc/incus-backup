// @vitest-environment node
import { execFileSync } from 'node:child_process'
import { describe, expect, it } from 'vitest'
import { curlFor, deref, endpoints, example, parseRate, rateText, scheduleError, typeLabel, type OpenAPIDoc } from './gapcore'

describe('rates', () => {
  it('parses and prints rates', () => {
    expect(parseRate('50 MiB/s')).toBe(50 * 1024 * 1024)
    expect(parseRate('800k')).toBe(800 * 1024)
    expect(parseRate('2 MB/s')).toBe(2_000_000)
    expect(parseRate('1.5M')).toBe(Math.round(1.5 * 1024 * 1024))
    expect(parseRate('65536')).toBe(65536)
    expect(parseRate('fast')).toBeNaN()
    expect(rateText(0)).toBe('0 B/s')
    expect(rateText(1024 * 1024 * 12.5)).toBe('12.5 MiB/s')
    expect(rateText(300)).toBe('300 B/s')
  })
})

describe('schedule grammar (mirror of internal/server/traffic.ParseSchedule)', () => {
  it('accepts what the server accepts and rejects what it rejects', () => {
    for (const ok of ['', 'always', 'mon-fri 08:00-18:00', 'sat,sun', '22:00-06:00', 'fri-mon', 'daily 00:00-24:00', 'mon-fri 08:00-12:00; mon-fri 13:00-18:00', '* 01:00-02:00'])
      expect(scheduleError(ok), ok).toBe('')
    for (const bad of ['monday', 'mon 8-18', 'mon 08:00-08:00', 'mon 25:00-26:00', 'mon tue 08:00-09:00', 'xyz-fri', 'mon 24:00-01:00'])
      expect(scheduleError(bad), bad).not.toBe('')
  })
})

describe('openapi helpers', () => {
  const doc: OpenAPIDoc = {
    openapi: '3.1.0',
    info: { title: 't', version: '1', description: '' },
    paths: {
      '/api/v1/b': { put: { operationId: 'put_b', summary: '', tags: ['B'], 'x-role': 'admin', 'x-repository-binding': 'global', responses: {} }, get: { operationId: 'get_b', summary: '', tags: ['B'], 'x-role': 'readonly', 'x-repository-binding': 'global', responses: {} } },
      '/api/v1/a': { get: { operationId: 'get_a', summary: '', tags: ['A'], 'x-role': 'readonly', 'x-repository-binding': 'query', responses: {} } },
    },
    components: { schemas: { 'x.T': { type: 'object', properties: { n: { type: 'integer' }, when: { type: ['string', 'null'], format: 'date-time' }, kids: { type: 'array', items: { $ref: '#/components/schemas/x.T' } } } } } },
  }
  it('lists operations by path then method', () => {
    expect(endpoints(doc).map((e) => `${e.method} ${e.path}`)).toEqual(['GET /api/v1/a', 'GET /api/v1/b', 'PUT /api/v1/b'])
  })
  it('labels types and builds examples without looping on recursive schemas', () => {
    const ref = { $ref: '#/components/schemas/x.T' }
    expect(typeLabel(ref)).toBe('x.T')
    expect(typeLabel({ type: 'array', items: ref })).toBe('x.T[]')
    expect(deref(doc, ref)?.properties?.n.type).toBe('integer')
    const ex = example(doc, ref) as Record<string, unknown>
    expect(ex.n).toBe(0)
    expect(ex.when).toBe('2026-10-08T12:00:00Z')
    expect(Array.isArray(ex.kids)).toBe(true)
  })
  it('writes curl with the bearer placeholder, query and quoted body', () => {
    const c = curlFor('https://srv:8443', 'PUT', '/api/v1/policies/daily', { repository: 'custody', empty: '' }, `{"note":"it's"}`)
    expect(c).toContain("-X PUT")
    expect(c).toContain("Authorization: Bearer $IB_TOKEN")
    expect(c).toContain("'https://srv:8443/api/v1/policies/daily?repository=custody'")
    expect(c).toContain(`--data '{"note":"it'\\''s"}'`)
  })
})

// The explorer reads the real document: generate it with the server binary's code and check the
// helpers on it (every operation listed, every request body gets an example).
describe('the real OpenAPI document', () => {
  it('lists every operation and builds an example for every request body', () => {
    let raw = ''
    try {
      raw = execFileSync('go', ['run', './cmd/incus-backup-server', 'openapi'], { cwd: new URL('../../../../', import.meta.url).pathname, encoding: 'utf8', maxBuffer: 64 << 20, timeout: 240_000 })
    } catch (e) {
      throw new Error(`go run ./cmd/incus-backup-server openapi failed (Go toolchain on PATH?): ${(e as Error).message}`)
    }
    const real = JSON.parse(raw) as OpenAPIDoc
    const eps = endpoints(real)
    expect(eps.length).toBeGreaterThan(125)
    for (const e of eps) {
      const s = e.op.requestBody?.content['application/json']?.schema
      if (s) expect(example(real, s), e.op.operationId).not.toBeUndefined()
      expect(e.op['x-role'], e.op.operationId).toMatch(/^(readonly|restore|operator|admin)$/)
    }
  }, 300_000)
})
