// Snapshot rows of the selected repository with the recorded run statistics and verification ages
// (lib/protect/snapshots.ts mergeSnapshots). One call each: GET /snapshots, GET /snapshot-stats,
// GET /policies (re-verify days) and GET /maintenance (repository default).
import { computed } from 'vue'
import { api, list } from '@/api/endpoints'
import { useResource } from '@/api/cache'
import { ApiError } from '@/api/client'
import { protectApi } from '@/api/sections/protect'
import { repos } from '@/stores/repos'
import { mergeSnapshots } from '@/lib/protect/snapshots'

export function useSnapRows(group?: () => string | undefined) {
  const g = () => group?.() ?? ''
  const on = () => !!repos.current
  const snaps = useResource(() => `snapshots:${repos.current}:${g() || 'all'}`, (s) => api.snapshots.list(g() || undefined, s), { enabled: on, refreshOn: ['run', 'repository'] })
  // the statistics route is new: an older server answers 404, the pages then show "not recorded"
  const stats = useResource(() => `snapshot-stats:${repos.current}:${g() || 'all'}`, async (s) => {
    try { return await protectApi.snapshotStats(g() || undefined, s) } catch (e) { if (e instanceof ApiError && e.status === 404) return null; throw e }
  }, { enabled: on, refreshOn: ['run', 'repository'] })
  const policies = useResource(() => `policies:${repos.current}`, (s) => api.policies.list(s), { enabled: on, ttl: 60_000 })
  const maint = useResource(() => `maintenance:${repos.current}`, (s) => api.maintenance.get(s), { enabled: on, ttl: 60_000 })

  const reverify = computed(() => {
    const def = maint.data.value?.default_reverify_days ?? 0
    const by = new Map(list(policies.data.value).map((p) => [p.name, p.reverify_days ?? 0]))
    return (policy: string | undefined) => (policy && by.get(policy)) || def
  })
  const rows = computed(() => (snaps.data.value === undefined ? undefined : mergeSnapshots(list(snaps.data.value), list(stats.data.value), reverify.value)))
  const statsMissing = computed(() => stats.data.value === null)
  const refresh = async () => { await Promise.all([snaps.refresh(), stats.refresh()]) }
  return { rows, snaps, stats, policies, maint, statsMissing, refresh }
}
