// The host list read model, shared by the host list, the host page and the overview strip: every
// enrolled identity of every repository (GET /hosts per repository), joined with the overview's
// per-host health (backup scope), the object counts (GET /objects per repository) and the
// certificate state (GET /certificates). Repositories are few, so one request each is fine.
import { computed } from 'vue'
import { api, list } from '@/api/endpoints'
import { useResource } from '@/api/cache'
import { infraApi, type HostCertStatus } from '@/api/sections/infra'
import { repos } from '@/stores/repos'
import { certIndex, certKey, hostProtection, hostState, isVolume, type HostProtection } from '@/lib/infra'
import type { HostOverview, HostRow, ObjectRow } from '@/api/types'
import type { Status } from '@/lib/status'

export interface HostListRow {
  key: string
  repository: string
  name: string
  scope: string
  fingerprint: string
  last_addr: string
  enrolled: string
  last_seen: string | null
  disabled: boolean
  /** Backup hosts: online | offline | disabled. Sync peers connect only while a sync runs: active | idle | disabled. */
  state: 'online' | 'offline' | 'disabled' | 'active' | 'idle'
  instances: number
  disks: number
  filesystems: number
  volumes: number
  objects: number
  stale: number
  never: number
  protection: HostProtection | ''
  protectionStatus: Status
  protectionLabel: string
  last_backup: string | null
  last_run_status: string | null
  cert: HostCertStatus | null
  /** Something an operator should look at. */
  attention: boolean
}

export const rowKey = (repository: string, name: string, scope: string) => `${repository}/${name}/${scope}`

export function useHostRows() {
  const names = () => repos.list.map((r) => r.name).join(',')
  const ov = useResource(() => 'overview', (s) => api.overview(s), { refreshOn: ['job', 'run', 'host', 'repository', 'alert'], poll: 60_000 })
  const ids = useResource(() => `infra-hosts:${names()}`, async (s) => {
    const out: Record<string, { hosts: HostRow[]; objects: ObjectRow[] }> = {}
    await Promise.all(repos.list.map(async (r) => {
      const [h, o] = await Promise.all([infraApi.hostsOf(r.name, s), infraApi.objectsOf(r.name, s)])
      out[r.name] = { hosts: list(h), objects: list(o) }
    }))
    return out
  }, { enabled: () => repos.loaded && repos.list.length > 0, refreshOn: ['host', 'run'], poll: 60_000 })
  const certs = useResource(() => 'certificates', (s) => infraApi.certificates(s), { ttl: 60_000 })

  const rows = computed<HostListRow[] | undefined>(() => {
    if (!ids.data.value) return undefined
    const ovHosts = new Map<string, HostOverview>()
    for (const h of ov.data.value?.hosts ?? []) ovHosts.set(`${h.repository}/${h.name}`, h)
    const ci = certIndex(certs.data.value?.report.hosts)
    const out: HostListRow[] = []
    const now = Date.now()
    for (const [repo, d] of Object.entries(ids.data.value)) {
      const byHost = new Map<string, ObjectRow[]>()
      for (const o of d.objects) byHost.set(o.host, [...(byHost.get(o.host) ?? []), o])
      for (const h of d.hosts) {
        const o = h.scope === 'backup' ? ovHosts.get(`${repo}/${h.name}`) : undefined
        const objs = h.scope === 'backup' ? byHost.get(h.name) ?? [] : []
        const prot = o ? hostProtection(o) : null
        const cert = ci.get(certKey(repo, h.name, h.scope)) ?? null
        const st = hostState({ disabled: h.disabled, online: o ? o.online : undefined, last_seen: h.last_seen }, now)
        const state: HostListRow['state'] = h.scope === 'backup' || st === 'disabled' ? st : st === 'online' ? 'active' : 'idle'
        const row: HostListRow = {
          key: rowKey(repo, h.name, h.scope), repository: repo, name: h.name, scope: h.scope, fingerprint: h.fingerprint, last_addr: h.last_addr,
          enrolled: h.enrolled, last_seen: h.last_seen, disabled: h.disabled, state,
          instances: objs.filter((x) => x.kind === 'instance').length,
          disks: objs.filter((x) => x.kind === 'block' && !isVolume(x.name)).length,
          filesystems: objs.filter((x) => x.kind === 'filesystem' && !isVolume(x.name)).length,
          volumes: objs.filter((x) => isVolume(x.name)).length,
          objects: objs.length,
          stale: o?.stale ?? 0, never: o?.never_backed_up ?? 0,
          protection: prot?.state ?? '', protectionStatus: prot?.status ?? 'neutral', protectionLabel: prot?.label ?? '',
          last_backup: o?.last_backup ?? null, last_run_status: o?.last_run_status ?? null, cert,
          attention: false,
        }
        row.attention = !row.disabled && (row.state === 'offline' || row.stale > 0 || row.never > 0 || (!!cert && cert.state !== 'ok') ||
          row.last_run_status === 'failed' || row.last_run_status === 'partial')
        out.push(row)
      }
    }
    return out.sort((a, b) => a.name.localeCompare(b.name) || a.repository.localeCompare(b.repository) || a.scope.localeCompare(b.scope))
  })

  async function refresh() { await Promise.all([ov.refresh(), ids.refresh(), certs.refresh()]) }
  return { ov, ids, certs, rows, refresh }
}
