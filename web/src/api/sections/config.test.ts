// @vitest-environment node
// The section's role table and alert-type catalogue against the Go sources they mirror.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { CONFIG_ROUTE_ROLES, canCfg } from './config'
import { ALERT_TYPES } from '@/lib/alertTypes'

const read = (p: string) => readFileSync(fileURLToPath(new URL(p, import.meta.url)), 'utf8')
const roleOf: Record<string, string> = { rd: 'readonly', rs: 'restore', op: 'operator', ad: 'admin' }

function goPerms(): Record<string, string> {
  const out: Record<string, string> = {}
  for (const src of [read('../../../../internal/server/mgmt/perms.go'), read('../../../../internal/server/mgmt/routes_config.go')]) {
    for (const m of src.matchAll(/"([A-Z]+) \/api\/v1([^"]*)"\]?\s*[:=]\s*(rd|rs|op|ad)\b/g)) out[`${m[1]} ${m[2]}`] = roleOf[m[3]]
  }
  return out
}

describe('config section', () => {
  it('has the server roles of every route it calls', () => {
    const server = goPerms()
    for (const [route, role] of Object.entries(CONFIG_ROUTE_ROLES)) expect([route, server[route]]).toEqual([route, role])
    expect(canCfg('operator', 'GET /notifications/channels')).toBe(true)
    expect(canCfg('readonly', 'GET /notifications/channels')).toBe(false)
    expect(canCfg('admin', 'GET /nope')).toBe(true)
    expect(canCfg('operator', 'GET /nope')).toBe(false)
  })

  it('knows every condition of the migration with its parameters and defaults', () => {
    const sql = read('../../../../migrations/20261008000000_initial_schema.sql')
    const rows = [...sql.matchAll(/\('([a-z_]+)',\s*'(info|warning|critical)',\s*'(\{[^']*\})'\)/g)]
    expect(rows.length).toBeGreaterThan(10)
    for (const [, type, , params] of rows) {
      const def = ALERT_TYPES.find((t) => t.type === type)
      expect(def, type).toBeTruthy()
      const want = JSON.parse(params) as Record<string, unknown>
      const got = Object.fromEntries(def!.params.map((p) => [p.key, p.def]))
      expect([type, got]).toEqual([type, want])
    }
  })
})
