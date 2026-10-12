<script setup lang="ts">
// Repositories: one card per datastore with what an operator checks first (mode, health and why,
// stored size and dedup, free space, last verify/GC/retention), or a dense table for many.
// Data: GET /repositories (identity, mode) + GET /overview (statistics; one call for all).
// GET /capacity is NOT called here: it samples the datastore (a full listing) on every call.
import { computed, ref, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import StatusPill from '@/components/StatusPill.vue'
import Meter from '@/components/Meter.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import Skeleton from '@/components/Skeleton.vue'
import Callout from '@/components/Callout.vue'
import EmptyState from '@/components/EmptyState.vue'
import DataTable from '@/components/DataTable.vue'
import Tooltip from '@/components/Tooltip.vue'
import { api } from '@/api/endpoints'
import { useResource } from '@/api/cache'
import { hasRole } from '@/stores/session'
import { formatBytes, formatNumber, formatRatio, formatPercent } from '@/lib/format'
import { repoHealth } from '@/lib/storage'
import type { ColumnDef } from '@/lib/table'
import type { RepoOverview, RepoView } from '@/api/types'

const router = useRouter()
const list = useResource(() => 'repositories', (s) => api.repositories.list(s), { refreshOn: ['repository'] })
const ov = useResource(() => 'overview', (s) => api.overview(s), { refreshOn: ['run', 'repository'], poll: 60_000 })

interface Row extends RepoView { o?: RepoOverview; health: ReturnType<typeof repoHealth> }
const rows = computed<Row[]>(() => (list.data.value ?? []).map((r) => {
  const o = ov.data.value?.repositories.find((x) => x.name === r.name)
  return { ...r, o, health: repoHealth(o, r.mode) }
}))

const VIEW_KEY = 'ib.repos.view'
const view = ref<'cards' | 'table'>('cards')
try { if (localStorage.getItem(VIEW_KEY) === 'table') view.value = 'table' } catch { /* storage blocked */ }
watch(view, (v) => { try { localStorage.setItem(VIEW_KEY, v) } catch { /* storage blocked */ } })

const used = (o?: RepoOverview) => (o && o.fs_total > 0 ? o.fs_total - o.fs_free : null)
const modeLabel = (m: string) => (m === 'online' ? 'Online' : m === 'read-only' ? 'Read-only' : 'Offline')
const cols: ColumnDef<Row>[] = [
  { key: 'name', label: 'Repository', required: true },
  { key: 'backend', label: 'Backend' },
  { key: 'mode', label: 'Mode' },
  { key: 'health', label: 'Health', value: (r) => r.health.label },
  { key: 'physical', label: 'Stored', align: 'right', value: (r) => r.o?.physical_bytes ?? null, format: (r) => formatBytes(r.o?.physical_bytes) },
  { key: 'dedup', label: 'Dedup', align: 'right', value: (r) => r.o?.dedup_ratio ?? null, format: (r) => formatRatio(r.o?.dedup_ratio) },
  { key: 'free', label: 'Free', align: 'right', value: (r) => (r.o && r.o.fs_total ? r.o.fs_free : null), format: (r) => (r.o && r.o.fs_total ? formatBytes(r.o.fs_free) : 'n/a') },
  { key: 'snapshots', label: 'Snapshots', align: 'right', value: (r) => r.o?.snapshots ?? null },
  { key: 'hosts', label: 'Hosts', align: 'right', value: (r) => r.o?.hosts ?? null },
  { key: 'last_verify', label: 'Last verify', value: (r) => r.o?.last_verify ?? '' },
  { key: 'last_gc', label: 'Last GC', value: (r) => r.o?.last_gc ?? '' },
  { key: 'location', label: 'Location', hidden: true, mono: true },
]
</script>

<template>
  <div class="repos">
    <header class="page-head">
      <div>
        <h1>Repositories</h1>
        <p class="lede">Where backups are stored. Each repository is a datastore (a directory on this server or an S3 bucket) with its own key domain, schedules and maintenance mode.</p>
      </div>
      <div class="actions">
        <div class="seg" role="group" aria-label="View">
          <button type="button" class="btn btn-sm" :aria-pressed="view === 'cards'" @click="view = 'cards'"><Icon name="layout-dashboard" :size="13" />Cards</button>
          <button type="button" class="btn btn-sm" :aria-pressed="view === 'table'" @click="view = 'table'"><Icon name="columns-3" :size="13" />Table</button>
        </div>
        <RouterLink v-if="hasRole('admin')" :to="{ name: 'repository-new' }" class="btn btn-primary" data-testid="repo-add"><Icon name="plus" />Add a repository</RouterLink>
      </div>
    </header>

    <Callout v-if="list.error.value && !list.data.value" status="critical" title="Could not load the repositories" :retry="list.refresh">{{ list.error.value.message }}</Callout>
    <Callout v-if="ov.error.value && list.data.value" status="warn" title="Statistics are unavailable" :retry="ov.refresh" class="mb">{{ ov.error.value.message }} Names and modes are current; sizes and times are missing.</Callout>

    <div v-if="list.loading.value && !list.data.value" class="cards">
      <div v-for="i in 2" :key="i" class="card panel"><Skeleton :lines="6" height="14px" /></div>
    </div>

    <EmptyState v-else-if="list.data.value && !rows.length" title="No repository yet" icon="database">
      A repository is where backups go: a directory on this server or an S3 bucket.
      <template v-if="hasRole('admin')" #actions><RouterLink :to="{ name: 'repository-new' }" class="btn btn-primary btn-sm"><Icon name="plus" :size="14" />Add a repository</RouterLink></template>
    </EmptyState>

    <div v-else-if="view === 'cards'" class="cards" data-testid="repo-cards">
      <article v-for="r in rows" :key="r.name" class="card panel" :class="`h-${r.health.status}`" :data-repo="r.name">
        <header class="ch">
          <Icon :name="r.backend === 's3' ? 'cloud' : 'hard-drive'" :size="18" class="bi" />
          <div class="title">
            <RouterLink :to="{ name: 'repository', params: { name: r.name } }" class="name">{{ r.name }}</RouterLink>
            <span class="loc mono truncate" :title="r.location">{{ r.location }}</span>
          </div>
          <span class="badge" :class="r.backend">{{ r.backend === 's3' ? 'S3' : 'Filesystem' }}</span>
        </header>

        <div class="pills">
          <StatusPill :state="r.mode" :label="modeLabel(r.mode)" size="sm" data-testid="mode-pill" />
          <Tooltip :text="r.health.reasons.join(' ') || 'Verified and collected recently, no corruption, no failing sync.'">
            <StatusPill :status="r.health.status" :label="r.health.label" size="sm" />
          </Tooltip>
          <span v-if="r.mode_reason" class="reason truncate" :title="r.mode_reason">“{{ r.mode_reason }}”</span>
        </div>

        <div class="stats">
          <div><span class="k">Stored</span><span class="v num">{{ formatBytes(r.o?.physical_bytes) }}</span></div>
          <div><span class="k">Logical</span><span class="v num">{{ formatBytes(r.o?.logical_bytes) }}</span></div>
          <div><span class="k">Dedup</span><span class="v num">{{ formatRatio(r.o?.dedup_ratio) }}</span></div>
          <div><span class="k">Snapshots</span><span class="v num">{{ formatNumber(r.o?.snapshots) }}</span></div>
          <div><span class="k">Objects</span><span class="v num">{{ formatNumber(r.o?.objects) }}</span></div>
          <div><span class="k">Hosts</span><span class="v num">{{ formatNumber(r.o?.hosts) }}</span></div>
        </div>

        <Meter v-if="r.o && r.o.fs_total > 0" :value="used(r.o)" :max="r.o.fs_total" :warn="0.8" :critical="0.9" label="Disk"
          :detail="`${formatBytes(r.o.fs_free)} free of ${formatBytes(r.o.fs_total)} (${formatPercent(used(r.o)! / r.o.fs_total, 0)} used)`" />
        <p v-else-if="r.backend === 's3'" class="muted small objnote"><Icon name="cloud" :size="13" />Object storage: usage only, free space cannot be known.</p>

        <dl class="times">
          <div><dt>Last backup</dt><dd><RelativeTime :time="r.o?.last_backup" /></dd></div>
          <div><dt>Last verify</dt><dd><RelativeTime :time="r.o?.last_verify" /></dd></div>
          <div><dt>Last GC</dt><dd><RelativeTime :time="r.o?.last_gc" /></dd></div>
          <div><dt>Last retention</dt><dd><RelativeTime :time="r.o?.last_retention" /></dd></div>
        </dl>

        <ul v-if="r.health.reasons.length" class="reasons">
          <li v-for="x in r.health.reasons" :key="x" :class="`s-${r.health.status}`"><Icon :name="r.health.status === 'critical' ? 'circle-alert' : 'triangle-alert'" :size="13" />{{ x }}</li>
        </ul>

        <footer class="cf">
          <RouterLink :to="{ name: 'repository', params: { name: r.name } }" class="btn btn-sm">Open</RouterLink>
          <RouterLink :to="{ name: 'repository', params: { name: r.name }, query: { tab: 'tasks' } }" class="btn btn-sm btn-ghost"><Icon name="play" :size="13" />Tasks</RouterLink>
          <RouterLink v-if="(r.o?.corrupt_chunks ?? 0) + (r.o?.failed_verify_snapshots ?? 0) > 0" :to="{ name: 'repository', params: { name: r.name }, query: { tab: 'integrity' } }" class="btn btn-sm btn-ghost crit"><Icon name="shield-alert" :size="13" />Integrity</RouterLink>
          <span class="spacer" />
          <span v-if="r.o?.sync_jobs" class="muted small"><Icon name="refresh-cw" :size="12" /> {{ r.o.sync_jobs }} sync {{ r.o.sync_jobs === 1 ? 'job' : 'jobs' }}</span>
        </footer>
      </article>
    </div>

    <DataTable v-else :rows="list.data.value ? rows : undefined" :columns="cols" row-key="name" storage-key="repos" csv-name="repositories" :loading="list.loading.value" row-clickable
      @row-click="(r) => router.push({ name: 'repository', params: { name: r.name } })">
      <template #cell-name="{ row }"><RouterLink :to="{ name: 'repository', params: { name: row.name } }" class="strong">{{ row.name }}</RouterLink></template>
      <template #cell-backend="{ row }"><span class="badge" :class="row.backend">{{ row.backend === 's3' ? 'S3' : 'Filesystem' }}</span></template>
      <template #cell-mode="{ row }"><StatusPill :state="row.mode" :label="modeLabel(row.mode)" size="sm" /></template>
      <template #cell-health="{ row }"><StatusPill :status="row.health.status" :label="row.health.label" variant="dot" /></template>
      <template #cell-last_verify="{ row }"><RelativeTime :time="row.o?.last_verify" /></template>
      <template #cell-last_gc="{ row }"><RelativeTime :time="row.o?.last_gc" /></template>
    </DataTable>
  </div>
</template>

<style scoped>
.mb { margin-bottom: var(--s-4); }
.seg { display: inline-flex; }
.seg .btn { border-radius: 0; }
.seg .btn:first-child { border-radius: var(--r) 0 0 var(--r); }
.seg .btn:last-child { border-radius: 0 var(--r) var(--r) 0; margin-left: -1px; }
.seg .btn[aria-pressed='true'] { background: var(--surface-3); color: var(--text); }
.cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 420px), 1fr)); gap: var(--s-4); }
.card { display: flex; flex-direction: column; gap: 12px; padding: 14px 16px; position: relative; min-width: 0; }
.card.h-warn { box-shadow: inset 3px 0 0 var(--warn); }
.card.h-critical { box-shadow: inset 3px 0 0 var(--critical); }
.ch { display: flex; align-items: flex-start; gap: 10px; min-width: 0; }
.bi { color: var(--text-muted); margin-top: 3px; }
.title { display: flex; flex-direction: column; min-width: 0; flex: 1; }
.name { font-size: var(--fs-lg); font-weight: var(--fw-semibold); color: var(--text); }
.loc { font-size: var(--fs-xs); color: var(--text-muted); }
.badge { display: inline-flex; align-items: center; height: 20px; padding: 0 7px; border-radius: var(--r-sm); font-size: var(--fs-xs); font-weight: var(--fw-semibold); border: 1px solid var(--border-strong); color: var(--text-2); background: var(--surface-2); white-space: nowrap; }
.badge.s3 { border-color: color-mix(in srgb, var(--series-1) 45%, transparent); color: var(--series-1); background: color-mix(in srgb, var(--series-1) 9%, var(--surface)); }
.pills { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; min-width: 0; }
.reason { font-size: var(--fs-sm); color: var(--text-2); max-width: 100%; }
.stats { display: grid; grid-template-columns: repeat(3, 1fr); gap: 8px 12px; }
.stats div { display: flex; flex-direction: column; min-width: 0; }
.k { font-size: var(--fs-xs); color: var(--text-muted); }
.v { font-size: var(--fs-lg); font-weight: var(--fw-semibold); }
.objnote { display: flex; align-items: center; gap: 6px; }
.times { display: grid; grid-template-columns: repeat(2, 1fr); gap: 4px 16px; margin: 0; font-size: var(--fs-sm); }
.times div { display: flex; justify-content: space-between; gap: 8px; border-bottom: 1px dashed var(--border); padding: 2px 0; }
.times dt { color: var(--text-muted); }
.times dd { margin: 0; }
.reasons { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 3px; font-size: var(--fs-sm); }
.reasons li { display: flex; gap: 6px; align-items: center; color: var(--text-2); }
.reasons .s-critical .icon { color: var(--critical); } .reasons .s-warn .icon { color: var(--warn); }
.cf { display: flex; gap: 6px; align-items: center; border-top: 1px solid var(--border); padding-top: 10px; margin-top: auto; flex-wrap: wrap; }
.crit { color: var(--critical-text); }
.strong { font-weight: var(--fw-semibold); }
@media (max-width: 520px) { .stats { grid-template-columns: repeat(2, 1fr); } .times { grid-template-columns: 1fr; } }
</style>
