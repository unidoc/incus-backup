<script setup lang="ts">
// Sync jobs of the selected repository: direction, remote, schedule, last run, lag, options; a
// topology of this server and its peers; adding a job (wizard) and running one now.
import { computed, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import EmptyState from '@/components/EmptyState.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import SyncTopology, { type TopoJob } from '@/components/storage/SyncTopology.vue'
import SyncAddWizard from '@/components/storage/SyncAddWizard.vue'
import { api, list } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import { ApiError } from '@/api/client'
import { repos, repoNone } from '@/stores/repos'
import { instance } from '@/stores/instance'
import { hasRole } from '@/stores/session'
import { toast } from '@/stores/toast'
import { describeSchedule, formatDuration, parseSchedule, plural } from '@/lib/format'
import { formatRateLimit, scheduleIntervalMs, syncHealth } from '@/lib/storage'
import type { ColumnDef } from '@/lib/table'
import type { SyncJob, SyncResult, SyncRunFailure, SyncStatus } from '@/api/types'

const router = useRouter()
const enabled = () => !!repos.current && !repoNone.value
const jobs = useResource(() => `sync:${repos.current}`, (s) => api.sync.list(s), { enabled, refreshOn: ['run'] })
// one status call per job (last run, lag); a handful of jobs per repository is the normal case
const statuses = useResource(() => `sync-status:${repos.current}:${list(jobs.data.value).map((j) => j.name).join(',')}`,
  async (s) => Object.fromEntries(await Promise.all(list(jobs.data.value).map(async (j) => [j.name, await api.sync.get(j.name, s).catch(() => null)] as const))) as Record<string, SyncStatus | null>,
  { enabled: () => enabled() && !!jobs.data.value, refreshOn: ['run'] })
const notAvailable = computed(() => jobs.error.value instanceof ApiError && jobs.error.value.status === 404)

interface Row extends SyncJob { st: SyncStatus | null; health: { status: ReturnType<typeof syncHealth>['status']; label: string } }
const rows = computed<Row[]>(() => list(jobs.data.value).map((j) => {
  const st = statuses.data.value?.[j.name] ?? null
  const p = parseSchedule(j.schedule)
  const iv = p instanceof Error ? 0 : scheduleIntervalMs(p)
  return { ...j, st, health: running.value[j.name] ? { status: 'info', label: 'Running' } : syncHealth(j, st?.last_run_status, st ? st.lag_ns / 1e6 : undefined, iv) }
}))
const topo = computed<TopoJob[]>(() => rows.value.map((r) => ({ name: r.name, remote: r.remote, remoteRepo: r.remote_repository, direction: r.direction, status: r.health.status, label: r.health.label.toLowerCase() })))
const failing = computed(() => rows.value.filter((r) => r.health.status === 'critical').length)

const options = (j: SyncJob) => [j.verified_only && 'verified only', j.resync_corrupt && 'resync corrupt', j.transfer_last && `last ${j.transfer_last}`, j.rate_limit_bps && formatRateLimit(j.rate_limit_bps), j.group_filter?.length && plural(j.group_filter.length, 'object')].filter(Boolean) as string[]
const cols: ColumnDef<Row>[] = [
  { key: 'name', label: 'Job', required: true },
  { key: 'direction', label: 'Direction' },
  { key: 'remote', label: 'Remote', value: (r) => `${r.remote} ${r.remote_repository}` },
  { key: 'schedule', label: 'Schedule', format: (r) => (r.enabled ? describeSchedule(r.schedule) : 'paused') },
  { key: 'health', label: 'State', value: (r) => r.health.label },
  { key: 'last', label: 'Last run', value: (r) => r.st?.last_run_at ?? '' },
  { key: 'lag', label: 'Lag', align: 'right', value: (r) => (r.st ? r.st.lag_ns : null), format: (r) => (r.st ? (r.st.lag_ns ? formatDuration(r.st.lag_ns / 1e6, { units: 1 }) : 'none') : '–') },
  { key: 'pending', label: 'Pending', align: 'right', value: (r) => r.st?.snapshots_pending ?? null },
  { key: 'options', label: 'Options', value: (r) => options(r).join(', '), sortable: false },
  { key: 'actions', label: '', sortable: false, noExport: true },
]

const wizard = ref(false)
const offline = computed(() => repos.list.find((r) => r.name === repos.current)?.mode === 'offline')
const running = ref<Record<string, boolean>>({})
async function runNow(j: SyncJob) {
  running.value = { ...running.value, [j.name]: true }
  try {
    const r = await api.sync.run(j.name)
    if ('ok' in r && (r as SyncRunFailure).ok === false) {
      const f = r as SyncRunFailure
      toast.warn(`Sync ${j.name} finished with failures`, `${f.result.synced} synced, ${f.result.failed} failed: ${f.error}`)
    } else {
      const x = r as SyncResult
      toast.success(`Sync ${j.name} finished`, `${plural(x.synced, 'snapshot')} synced, ${x.skipped_unverified + x.skipped_corrupt} skipped, ${x.repaired} repaired.`)
    }
  } catch (e) { toast.error(e, `Sync ${j.name} failed`) } finally {
    running.value = { ...running.value, [j.name]: false }
    invalidate(`sync-status:${repos.current}`)
    invalidate(`sync-runs:${repos.current}:${j.name}`)
    void statuses.refresh()
  }
}
function added(name: string) {
  invalidate(`sync:${repos.current}`)
  void jobs.refresh()
  void router.push({ name: 'sync-job', params: { name } })
}
</script>

<template>
  <div class="sync">
    <header class="page-head">
      <div>
        <h1>Sync</h1>
        <p class="lede">Replicate committed backups to or from other incus-backup servers, without any key. This server opens every connection; retention stays independent on each side.</p>
      </div>
      <div class="actions">
        <button v-if="hasRole('admin') && !repoNone" type="button" class="btn btn-primary" data-testid="sync-add-open" @click="wizard = true"><Icon name="plus" />Add a sync job</button>
      </div>
    </header>

    <EmptyState v-if="repoNone" title="No repository yet" icon="database">Sync jobs belong to a repository. Add one first.</EmptyState>
    <EmptyState v-else-if="notAvailable" title="Sync is not enabled on this server" icon="refresh-cw">The server runs without its sync manager, so the sync routes do not exist.</EmptyState>
    <template v-else>
      <Callout v-if="jobs.error.value && !jobs.data.value" status="critical" title="Could not load the sync jobs" :retry="jobs.refresh">{{ jobs.error.value.message }}</Callout>
      <section class="panel topo-panel">
        <div class="panel-head"><h2>Topology</h2><span class="sub">{{ rows.length ? `${plural(rows.length, 'job')}${failing ? ` · ${failing} failing` : ''}` : 'not replicated' }}</span></div>
        <div class="panel-body"><SyncTopology :server="instance.name || 'this server'" :repo="repos.current" :jobs="topo" @select="(n) => router.push({ name: 'sync-job', params: { name: n } })" /></div>
      </section>

      <DataTable :rows="jobs.data.value === undefined ? undefined : rows" :columns="cols" row-key="name" storage-key="sync-jobs" csv-name="sync-jobs" :loading="jobs.loading.value" :error="jobs.error.value" :retry="jobs.refresh"
        row-clickable @row-click="(r) => router.push({ name: 'sync-job', params: { name: r.name } })">
        <template #cell-name="{ row }"><RouterLink :to="{ name: 'sync-job', params: { name: row.name } }" class="strong">{{ row.name }}</RouterLink></template>
        <template #cell-direction="{ row }"><span class="dir" :class="row.direction"><Icon :name="row.direction === 'push' ? 'arrow-up' : 'arrow-down'" :size="12" />{{ row.direction }}</span></template>
        <template #cell-remote="{ row }"><span class="mono small">{{ row.remote }}</span> <span class="muted small">/ {{ row.remote_repository }}</span></template>
        <template #cell-health="{ row }"><StatusPill :status="row.health.status" :label="row.health.label" size="sm" /></template>
        <template #cell-last="{ row }"><template v-if="row.st?.last_run_at"><RelativeTime :time="row.st.last_run_at" /> <StatusPill :state="row.st.last_run_status" variant="dot" /></template><span v-else class="muted">never</span></template>
        <template #cell-options="{ row }"><span class="opts"><span v-for="o in options(row)" :key="o" class="opt">{{ o }}</span></span></template>
        <template #cell-actions="{ row }">
          <button v-if="hasRole('operator')" type="button" class="btn btn-sm" :disabled="running[row.name] || offline" :data-testid="`sync-run-${row.name}`" @click.stop="runNow(row)">
            <Spinner v-if="running[row.name]" :size="12" /><Icon v-else name="play" :size="12" />Run now</button>
        </template>
        <template #empty>
          <EmptyState title="No sync job" icon="refresh-cw">A second server with a copy of your backups survives the loss of this one. Push to it, or let it pull.
            <template v-if="hasRole('admin')" #actions><button type="button" class="btn btn-primary btn-sm" @click="wizard = true"><Icon name="plus" :size="14" />Add a sync job</button></template>
          </EmptyState>
        </template>
      </DataTable>
    </template>
    <SyncAddWizard v-model:open="wizard" :existing="rows.map((r) => r.name)" @added="added" />
  </div>
</template>

<style scoped>
.sync { display: flex; flex-direction: column; gap: var(--s-4); }
.sync .page-head { margin-bottom: 0; }
.strong { font-weight: var(--fw-semibold); }
.dir { display: inline-flex; align-items: center; gap: 4px; font-size: var(--fs-sm); font-weight: var(--fw-medium); padding: 1px 7px; border-radius: var(--r-pill); border: 1px solid var(--border-strong); }
.dir.push { color: var(--series-1); border-color: color-mix(in srgb, var(--series-1) 40%, transparent); }
.dir.pull { color: var(--series-3); border-color: color-mix(in srgb, var(--series-3) 40%, transparent); }
.opts { display: inline-flex; gap: 4px; flex-wrap: wrap; }
.opt { font-size: var(--fs-xs); padding: 1px 6px; border-radius: var(--r-sm); background: var(--surface-3); color: var(--text-2); white-space: nowrap; }
</style>
