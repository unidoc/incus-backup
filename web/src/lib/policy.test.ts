import { describe, expect, it } from 'vitest'
import { assignmentTree, deleteImpact, ownerName, resolve } from './policy'
import type { EffectivePolicy } from '@/api/sections/storage'
import type { AssignmentRow } from '@/api/types'

const A = (host: string, project: string, object: string, policy: string): AssignmentRow => ({ host, project, object, policy })
const E = (host: string, project: string, name: string, policy: string, source: EffectivePolicy['source'], pinned = 'nightly'): EffectivePolicy => ({
  group_id: `${project}/${name}`, host, project, name, kind: 'block', owner: ownerName(name), declared_policy: pinned, pinned_policy: pinned, policy, source, policy_known: true, snapshots: 3,
})

describe('policy resolution (mirror of service.ResolvePolicy + pinned fallback)', () => {
  it('maps object names to the names assignments use', () => {
    expect(ownerName('instance/web/root')).toBe('web')
    expect(ownerName('instance/web')).toBe('web')
    expect(ownerName('volume/scratch')).toBe('scratch')
    expect(ownerName('vm/root')).toBe('vm/root')
  })
  // the same cases as TestEffectivePoliciesAgreeWithPrune (internal/server/service)
  it('object beats project beats host default beats pinned', () => {
    const as = [A('benfica', '', '', 'weekly'), A('benfica', 'prod', '', 'short'), A('benfica', 'prod', 'web', 'hourly')]
    expect(resolve(as, 'prod', 'web')).toEqual({ policy: 'hourly', source: 'object' })
    expect(resolve(as, 'prod', 'db')).toEqual({ policy: 'short', source: 'project' })
    expect(resolve(as, 'dev', 'scratch')).toEqual({ policy: 'weekly', source: 'host' })
    expect(resolve([], 'dev', 'scratch', 'nightly')).toEqual({ policy: 'nightly', source: 'pinned' })
    expect(resolve([], 'dev', 'scratch')).toEqual({ policy: '', source: 'none' })
  })
})

describe('assignment tree', () => {
  it('groups objects per host, project and owner, and keeps assignments without objects', () => {
    const eff = [E('bosch', 'prod', 'instance/redis', 'hourly', 'object'), E('bosch', 'prod', 'instance/redis/root', 'hourly', 'object'), E('bosch', 'staging', 'instance/ci/root', 'nightly', 'host')]
    const as = [A('bosch', '', '', 'nightly'), A('bosch', 'prod', 'redis', 'hourly'), A('bosch', 'legacy', '', 'weekly')]
    const t = assignmentTree(['bosch', 'newhost'], eff, as)
    expect(t.map((h) => h.host)).toEqual(['bosch', 'newhost'])
    const b = t[0]
    expect(b.assigned).toBe('nightly')
    expect(b.projects.map((p) => p.project)).toEqual(['legacy', 'prod', 'staging'])
    const prod = b.projects[1]
    expect(prod.owners).toHaveLength(1)
    expect(prod.owners[0]).toMatchObject({ owner: 'redis', assigned: 'hourly', policy: 'hourly', source: 'object' })
    expect(prod.owners[0].entries).toHaveLength(2)
    expect(b.projects[0]).toMatchObject({ project: 'legacy', assigned: 'weekly', owners: [] })
    expect(t[1]).toMatchObject({ host: 'newhost', assigned: '', projects: [], objects: 0 })
  })
})

describe('delete impact', () => {
  it('says what governs the affected objects afterwards', () => {
    const as = [A('bosch', '', '', 'nightly'), A('bosch', 'prod', 'redis', 'hourly')]
    const eff = [
      E('bosch', 'prod', 'instance/redis/root', 'hourly', 'object'),
      E('uni', 'default', 'instance/mon/root', 'hourly', 'pinned', 'hourly'),
      E('bosch', 'prod', 'instance/web/root', 'nightly', 'host'),
    ]
    expect(deleteImpact('hourly', eff, as)).toEqual({ assignments: 1, objects: 2, fallback: { nightly: 1, '': 1 } })
  })
})
