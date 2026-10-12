<script setup lang="ts">
// Host jobs of the selected repository, newest first, live: every SSE `job` event refetches the list
// (debounced), with a slow poll as a fallback. Filters by state, type and host live in the URL.
import { computed, reactive, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import DataTable from '@/components/DataTable.vue'
import EmptyState from '@/components/EmptyState.vue'
import StatusPill from '@/components/StatusPill.vue'
import StatTile from '@/components/StatTile.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import PIcon from '@/components/protect/PIcon.vue'
import { list } from '@/api/endpoints'
import { useResource } from '@/api/cache'
import { ApiError } from '@/api/client'
import { protectApi, JOB_STATES, JOB_TYPES, terminal, type Job } from '@/api/sections/protect'
import { repos, repoNone } from '@/stores/repos'
import { jobLabel, jobSubject } from '@/lib/protect/jobs'
import { formatDuration, formatNumber } from '@/lib/format'
import type { ColumnDef } from '@/lib/table'

const route = useRoute()
const router = useRouter()
const RESTORES = ['restore_instance', 'restore_volume', 'guest_files', 'fs_extract']
const f = reactive({ state: '', type: '', host: '' })
function fromQuery() { for (const k of ['state', 'type', 'host'] as const) f[k] = typeof route.query[k] === 'string' ? (route.query[k] as string) : '' }
fromQuery()
watch(() => route.query, fromQuery)
watch(f, () => { const q: Record<string, string> = {}; for (const [k, v] of Object.entries(f)) if (v) q[k] = v; void router.replace({ query: q }) }, { deep: true })

// the server filters by state; type groups ("restore") and hosts are cheap to filter here
const jobs = useResource(() => `jobs:${repos.current}:${f.state}`, async (s) => {
  try { return await protectApi.jobs.list({ state: f.state === 'active' ? 'queued,running' : f.state || undefined, limit: 500 }, s) } catch (e) { if (e instanceof ApiError && e.status === 404) return null; throw e }
}, { enabled: () => !!repos.current && !repoNone.value, refreshOn: ['job'], poll: 30_000 })
const all = computed(() => list(jobs.data.value))
const shown = computed(() => all.value.filter((j) => (!f.host || j.host === f.host) && (!f.type || (f.type === 'restore' ? RESTORES.includes(j.type) : j.type === f.type))))
const hosts = computed(() => [...new Set(all.value.map((j) => j.host))].sort())
const counts = computed(() => {
  const c = { active: 0, failed24: 0, succeeded24: 0 }
  const day = Date.now() - 86_400_000
  for (const j of all.value) {
    if (!terminal(j.state)) c.active++
    else if (Date.parse(j.finished_at ?? j.created_at) > day) { if (j.state === 'succeeded') c.succeeded24++; else if (j.state === 'failed' || j.state === 'expired') c.failed24++ }
  }
  return c
})

const took = (j: Job) => (j.started_at ? (j.finished_at ? Date.parse(j.finished_at) : Date.now()) - Date.parse(j.started_at) : null)
const pct = (j: Job) => (j.state === 'succeeded' ? 100 : j.bytes_total > 0 ? Math.round((j.bytes_done / j.bytes_total) * 100) : j.progress)
const cols: ColumnDef<Job>[] = [
  { key: 'created_at', label: 'Created', width: '120px' },
  { key: 'type', label: 'Job', format: (j) => jobLabel(j.type), width: '170px', required: true },
  { key: 'subject', label: 'What', value: (j) => jobSubject(j) },
  { key: 'host', label: 'Host', width: '100px' },
  { key: 'state', label: 'State', width: '170px' },
  { key: 'took', label: 'Took', value: took, align: 'right', width: '80px' },
  { key: 'message', label: 'Message', hidden: true },
  { key: 'created_by', label: 'By', width: '100px', hidden: true },
  { key: 'id', label: 'Id', mono: true, hidden: true },
]
const open = (j: Job) => router.push({ name: 'job', params: { id: j.id } })
</script>

<template>
  <div data-testid="jobs">
    <header class="page-head">
      <div>
        <h1>Jobs</h1>
        <p class="lede">Work this server asked a host to do: restores, file recoveries, restore tests, deep verification and backups on demand. The host runs them with its own key and reports back.</p>
      </div>
      <div class="actions">
        <RouterLink class="btn" :to="{ name: 'restore-tests' }"><PIcon name="badge-check" :size="15" />Restore tests</RouterLink>
        <RouterLink class="btn btn-primary" :to="{ name: 'restore' }"><PIcon name="archive-restore" :size="15" />Restore…</RouterLink>
      </div>
    </header>

    <EmptyState v-if="repoNone" title="No repository yet" icon="database" />
    <template v-else>
      <div class="tiles">
        <StatTile label="Running or queued" :value="formatNumber(counts.active)" :status="counts.active ? 'info' : undefined" :to="{ name: 'jobs', query: { state: 'active' } }" :loading="jobs.loading.value" />
        <StatTile label="Succeeded (24 h)" :value="formatNumber(counts.succeeded24)" status="ok" :loading="jobs.loading.value" />
        <StatTile label="Failed (24 h)" :value="formatNumber(counts.failed24)" :status="counts.failed24 ? 'critical' : 'ok'" :to="{ name: 'jobs', query: { state: 'failed' } }" :loading="jobs.loading.value" />
      </div>
      <div class="filters panel">
        <label><span>State</span><select v-model="f.state" class="select sm" data-testid="filter-state"><option value="">Any</option><option value="active">Running or queued</option><option v-for="s in JOB_STATES" :key="s" :value="s">{{ s }}</option></select></label>
        <label><span>Type</span><select v-model="f.type" class="select sm"><option value="">Any</option><option value="restore">Restores and file recoveries</option><option v-for="t in JOB_TYPES" :key="t" :value="t">{{ jobLabel(t) }}</option></select></label>
        <label><span>Host</span><select v-model="f.host" class="select sm"><option value="">Any</option><option v-for="h in hosts" :key="h" :value="h">{{ h }}</option></select></label>
      </div>
      <DataTable :rows="jobs.data.value === undefined ? undefined : shown" :columns="cols" row-key="id" storage-key="jobs" csv-name="jobs" :loading="jobs.loading.value" :error="jobs.error.value" :retry="jobs.refresh"
        :initial-sort="{ key: 'created_at', dir: 'desc' }" row-clickable data-testid="job-table" @row-click="open">
        <template #cell-created_at="{ row }"><RelativeTime :time="row.created_at" /></template>
        <template #cell-type="{ row }"><RouterLink :to="{ name: 'job', params: { id: row.id } }" class="jt">{{ jobLabel(row.type) }}</RouterLink></template>
        <template #cell-subject="{ row }"><span class="truncate subj" :title="jobSubject(row)">{{ jobSubject(row) }}</span></template>
        <template #cell-state="{ row }">
          <span class="st">
            <StatusPill :state="row.state" size="sm" />
            <span v-if="row.state === 'running'" class="mini" role="progressbar" :aria-valuenow="pct(row)" aria-valuemin="0" aria-valuemax="100"><span :style="{ width: `${pct(row)}%` }" /></span>
            <span v-if="row.cancel_requested && !terminal(row.state)" class="muted small">cancelling</span>
          </span>
        </template>
        <template #cell-took="{ row }"><span v-if="took(row) !== null" class="num">{{ formatDuration(took(row)) }}</span><span v-else class="muted">–</span></template>
        <template #empty>
          <EmptyState v-if="jobs.data.value === null" title="This server does not run host jobs yet" icon="activity">Upgrade the server; jobs need the host-jobs API.</EmptyState>
          <EmptyState v-else-if="f.state || f.type || f.host" title="No job matches these filters" icon="filter" />
          <EmptyState v-else title="No jobs yet" icon="activity">Jobs appear when someone starts a restore, a restore test or a deep verification.
            <template #actions><RouterLink class="btn" :to="{ name: 'restore' }">Restore…</RouterLink></template>
          </EmptyState>
        </template>
      </DataTable>
    </template>
  </div>
</template>

<style scoped>
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); gap: var(--s-3); margin-bottom: var(--s-4); }
.filters { display: flex; flex-wrap: wrap; gap: var(--s-2) var(--s-3); padding: var(--s-3); margin-bottom: var(--s-3); }
.filters label { display: flex; flex-direction: column; gap: 3px; font-size: var(--fs-xs); color: var(--text-muted); min-width: 160px; }
.sm { height: var(--control-h-sm); font-size: var(--fs-sm); }
.jt { font-weight: var(--fw-medium); color: var(--text); }
.subj { display: block; max-width: 48ch; }
.st { display: inline-flex; align-items: center; gap: 8px; }
.mini { width: 56px; height: 5px; border-radius: var(--r-pill); background: var(--surface-3); overflow: hidden; display: inline-block; }
.mini span { display: block; height: 100%; background: var(--accent); }
.small { font-size: var(--fs-sm); }
</style>
