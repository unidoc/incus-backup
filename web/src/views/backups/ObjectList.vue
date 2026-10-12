<script setup lang="ts">
// Objects: every storage object (instance records, VM disks, container filesystems, custom volumes)
// with its policy, snapshots and freshness against its schedule (the overview's rule, from
// GET /objects/stats). Filter by kind, host, project, policy and freshness; group by host or
// project; CSV of what is shown.
import { computed, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import DataTable from '@/components/DataTable.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import ByteSize from '@/components/ByteSize.vue'
import EmptyState from '@/components/EmptyState.vue'
import Callout from '@/components/Callout.vue'
import Tooltip from '@/components/Tooltip.vue'
import Sparkline from '@/components/charts/Sparkline.vue'
import KindIcon from '@/components/infra/KindIcon.vue'
import FreshnessBadge from '@/components/infra/FreshnessBadge.vue'
import FilterChips from '@/components/infra/FilterChips.vue'
import { useResource } from '@/api/cache'
import { infraApi, type ObjectStat } from '@/api/sections/infra'
import { repos } from '@/stores/repos'
import { downloadText } from '@/composables/useClipboard'
import { freshnessRank, freshnessText, kindInfo, matchesKind, type KindFilter } from '@/lib/infra'
import { toCSV, filterRows, type ColumnDef } from '@/lib/table'
import { formatBytes, formatNumber, plural } from '@/lib/format'

const route = useRoute()
const router = useRouter()
const stats = useResource(() => `objects-stats:${repos.current}:`, (s) => infraApi.objectStats(undefined, s), { enabled: () => !!repos.current, refreshOn: ['run'] })
const all = computed(() => stats.data.value ?? [])

// ---- filters, mirrored in the URL so a filtered view can be shared
const qv = (k: string, d: string) => (typeof route.query[k] === 'string' ? (route.query[k] as string) : d)
const fresh = ref(qv('freshness', 'all'))
const kind = ref<KindFilter>(qv('kind', 'data') as KindFilter)
const host = ref(qv('host', ''))
const project = ref(qv('project', ''))
const policy = ref(qv('policy', ''))
const group = ref(qv('group', 'none'))
const text = ref(qv('q', ''))
watch([fresh, kind, host, project, policy, group, text], () => {
  const query: Record<string, string> = {}
  if (fresh.value !== 'all') query.freshness = fresh.value
  if (kind.value !== 'data') query.kind = kind.value
  if (host.value) query.host = host.value
  if (project.value) query.project = project.value
  if (policy.value) query.policy = policy.value
  if (group.value !== 'none') query.group = group.value
  if (text.value) query.q = text.value
  router.replace({ query })
})
const effPolicy = (o: ObjectStat) => o.retention_policy || o.declared_policy
const pre = computed(() => all.value.filter((o) => matchesKind(o.kind, o.name, kind.value) && (!host.value || o.host === host.value) && (!project.value || o.project === project.value) && (!policy.value || effPolicy(o) === policy.value)))
const fcount = (f: string) => pre.value.filter((o) => o.freshness === f).length
const chips = computed(() => [
  { value: 'all', label: 'All', count: pre.value.length },
  { value: 'stale', label: 'Overdue', count: fcount('stale'), status: 'warn' as const },
  { value: 'never', label: 'Never backed up', count: fcount('never'), status: 'critical' as const },
  { value: 'ok', label: 'Current', count: fcount('ok'), status: 'ok' as const },
  { value: 'unscheduled', label: 'No schedule', count: fcount('unscheduled') },
])
// worst first until a header is clicked
const rows = computed(() => (stats.data.value ? pre.value.filter((o) => fresh.value === 'all' || o.freshness === fresh.value).sort((a, b) => (freshnessRank[a.freshness] ?? 9) - (freshnessRank[b.freshness] ?? 9) || a.host.localeCompare(b.host) || a.name.localeCompare(b.name)) : undefined))
const uniq = (f: (o: ObjectStat) => string) => [...new Set(all.value.map(f).filter(Boolean))].sort()
const hosts = computed(() => uniq((o) => o.host))
const projects = computed(() => uniq((o) => o.project))
const policies = computed(() => uniq(effPolicy))
const kinds: Array<{ value: KindFilter; label: string }> = [
  { value: 'data', label: 'Data objects' }, { value: 'all', label: 'Everything, with instance records' }, { value: 'disk', label: 'VM disks' }, { value: 'filesystem', label: 'Container filesystems' },
  { value: 'volume', label: 'Custom volumes' }, { value: 'instance', label: 'Instance records' },
]
const anyFilter = computed(() => fresh.value !== 'all' || kind.value !== 'data' || !!host.value || !!project.value || !!policy.value)
function clearFilters() { fresh.value = 'all'; kind.value = 'data'; host.value = ''; project.value = ''; policy.value = ''; text.value = '' }

const cols: ColumnDef<ObjectStat>[] = [
  { key: 'name', label: 'Object', required: true },
  { key: 'kind', label: 'Kind', value: (o) => kindInfo(o.kind, o.name).label, hidden: true },
  { key: 'host', label: 'Host' },
  { key: 'project', label: 'Project' },
  { key: 'policy', label: 'Policy', value: (o) => o.declared_policy },
  { key: 'retention_policy', label: 'Retention', value: (o) => o.retention_policy, hidden: true },
  { key: 'snapshots', label: 'Snapshots', align: 'right' },
  { key: 'latest', label: 'Latest', value: (o) => o.latest?.created ?? '' },
  { key: 'size', label: 'Size', align: 'right', value: (o) => o.latest?.logical_bytes ?? null },
  { key: 'trend', label: 'Trend', sortable: false, noExport: true, value: () => '', hidden: true },
  { key: 'freshness', label: 'Freshness', value: (o) => freshnessText[o.freshness] ?? o.freshness },
  { key: 'protected', label: 'Protected', align: 'right' },
  { key: 'notes', label: 'Notes', hidden: true },
  { key: 'group_id', label: 'Group id', hidden: true, mono: true },
]

// ---- grouping
const filteredText = computed(() => (rows.value ? filterRows(rows.value, cols, text.value) : undefined))
const groups = computed(() => {
  if (group.value === 'none' || !filteredText.value) return []
  const key = group.value === 'host' ? (o: ObjectStat) => o.host || '(no host)' : (o: ObjectStat) => o.project || '(default)'
  const m = new Map<string, ObjectStat[]>()
  for (const o of filteredText.value) m.set(key(o), [...(m.get(key(o)) ?? []), o])
  return [...m.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([k, v]) => ({
    key: k, rows: v, stale: v.filter((o) => o.freshness === 'stale').length, never: v.filter((o) => o.freshness === 'never').length,
    bytes: v.reduce((a, o) => a + (o.latest?.logical_bytes ?? 0), 0),
  }))
})
const collapsed = ref<Set<string>>(new Set())
function toggle(k: string) { const s = new Set(collapsed.value); s.has(k) ? s.delete(k) : s.add(k); collapsed.value = s }
const groupCols = computed(() => cols.filter((c) => c.key !== group.value))
function exportAll() { downloadText(`objects-${repos.current}.csv`, toCSV(filteredText.value ?? [], cols), 'text/csv') }

const summary = computed(() => {
  const data = all.value.filter((o) => o.kind !== 'instance')
  return { objects: data.length, stale: data.filter((o) => o.freshness === 'stale').length, never: data.filter((o) => o.freshness === 'never').length, bytes: data.reduce((a, o) => a + (o.latest?.logical_bytes ?? 0), 0) }
})
</script>

<template>
  <div class="objs">
    <header class="page-head">
      <div>
        <h1>Objects</h1>
        <p class="lede">Everything the hosts back up: instance records, VM disks, container filesystems and custom volumes. Freshness compares the newest snapshot with the policy's schedule.</p>
      </div>
      <div class="actions"><button type="button" class="btn btn-sm" :disabled="stats.refreshing.value" @click="stats.refresh()"><Icon name="refresh-cw" :size="13" />Refresh</button></div>
    </header>

    <p v-if="stats.data.value" class="sum small" data-testid="object-summary">
      <b class="num">{{ formatNumber(summary.objects) }}</b> data objects ·
      <span :class="{ warnv: summary.stale }"><b class="num">{{ summary.stale }}</b> overdue</span> ·
      <span :class="{ critv: summary.never }"><b class="num">{{ summary.never }}</b> never backed up</span> ·
      <b class="num">{{ formatBytes(summary.bytes) }}</b> in their newest snapshots (before deduplication)
    </p>

    <div class="filters" data-testid="object-filters">
      <FilterChips v-model="fresh" :options="chips" label="Freshness" />
      <div class="sels">
        <select v-model="kind" class="select" aria-label="Kind" data-testid="f-kind"><option v-for="k in kinds" :key="k.value" :value="k.value">{{ k.label }}</option></select>
        <select v-model="host" class="select" aria-label="Host" data-testid="f-host"><option value="">All hosts</option><option v-for="h in hosts" :key="h" :value="h">{{ h }}</option></select>
        <select v-model="project" class="select" aria-label="Project" data-testid="f-project"><option value="">All projects</option><option v-for="p in projects" :key="p" :value="p">{{ p }}</option></select>
        <select v-model="policy" class="select" aria-label="Policy" data-testid="f-policy"><option value="">All policies</option><option v-for="p in policies" :key="p" :value="p">{{ p }}</option></select>
        <select v-model="group" class="select" aria-label="Group by" data-testid="f-group"><option value="none">No grouping</option><option value="host">Group by host</option><option value="project">Group by project</option></select>
        <button v-if="anyFilter" type="button" class="btn btn-sm btn-ghost" @click="clearFilters"><Icon name="x" :size="13" />Clear filters</button>
      </div>
    </div>

    <Callout v-if="stats.error.value && !stats.data.value" status="critical" title="Could not load the objects" :retry="stats.refresh">{{ stats.error.value.message }}</Callout>

    <!-- flat -->
    <DataTable v-if="group === 'none'" :rows="rows" :columns="cols" row-key="group_id" :loading="stats.loading.value" :error="stats.error.value" :retry="stats.refresh"
      storage-key="objects" :csv-name="`objects-${repos.current}`" filter-placeholder="Filter (name, host, project; col:term, -term)" data-testid="object-table">
      <template #cell-name="{ row }"><span class="on"><KindIcon :kind="row.kind" :name="row.name" /><RouterLink :to="{ name: 'object', params: { group: row.group_id } }" class="strong" :data-object="row.name">{{ row.name }}</RouterLink><Icon v-if="row.notes" name="scroll-text" :size="12" class="muted" :title="row.notes" /></span></template>
      <template #cell-kind="{ row }"><KindIcon :kind="row.kind" :name="row.name" label /></template>
      <template #cell-host="{ row }"><RouterLink v-if="row.host" :to="{ name: 'host', params: { name: row.host } }">{{ row.host }}</RouterLink><span v-else class="muted">–</span></template>
      <template #cell-project="{ row }"><span class="nowrap">{{ row.project || '–' }}</span></template>
      <template #cell-policy="{ row }"><span class="nowrap">{{ row.declared_policy || '–' }}<Tooltip v-if="row.retention_policy && row.retention_policy !== row.declared_policy" :text="`Retention by ${row.retention_policy}, set by an operator`"><span class="ovr small"> → {{ row.retention_policy }}</span></Tooltip></span></template>
      <template #cell-retention_policy="{ row }"><span v-if="row.retention_policy && row.retention_policy !== row.declared_policy" class="ovr nowrap" title="Set by an operator">{{ row.retention_policy }}</span><span v-else class="muted nowrap">{{ row.retention_policy || '–' }}</span></template>
      <template #cell-latest="{ row }"><RelativeTime :time="row.latest?.created" /></template>
      <template #cell-size="{ row }"><ByteSize v-if="row.latest && row.kind !== 'instance'" :bytes="row.latest.logical_bytes" /><span v-else class="muted">–</span></template>
      <template #cell-trend="{ row }"><Sparkline v-if="row.history.length > 1 && row.kind !== 'instance'" :values="row.history.map((h) => h.logical_bytes)" /></template>
      <template #cell-freshness="{ row }"><FreshnessBadge :freshness="row.freshness" :due="row.due" :overdue-at="row.overdue_at" :schedule="row.schedule" /></template>
      <template #cell-protected="{ row }"><span v-if="row.protected" class="prot"><Icon name="lock" :size="12" />{{ row.protected }}</span><span v-else class="muted">0</span></template>
      <template #empty><EmptyState title="No objects yet" icon="box">Objects appear when a host reports its instances and volumes on its first backup run. <RouterLink :to="{ name: 'hosts' }">See the hosts</RouterLink>.</EmptyState></template>
    </DataTable>

    <!-- grouped -->
    <template v-else>
      <div class="gbar">
        <label class="search"><Icon name="search" :size="14" /><span class="sr-only">Filter objects</span><input v-model="text" type="search" class="q" placeholder="Filter (name, host, project)" /></label>
        <span class="muted small num">{{ plural(filteredText?.length ?? 0, 'object') }} in {{ plural(groups.length, 'group') }}</span>
        <span class="spacer" />
        <button type="button" class="btn btn-sm" :disabled="!filteredText?.length" data-testid="export-csv" @click="exportAll"><Icon name="file-down" :size="14" />CSV</button>
      </div>
      <div v-if="!groups.length && stats.data.value" class="panel"><EmptyState title="No objects match" icon="filter" compact>Clear the filters to see every object.</EmptyState></div>
      <section v-for="g in groups" :key="g.key" class="grp" :data-group="g.key">
        <button type="button" class="ghead" :aria-expanded="!collapsed.has(g.key)" @click="toggle(g.key)">
          <Icon :name="collapsed.has(g.key) ? 'chevron-right' : 'chevron-down'" :size="14" />
          <Icon :name="group === 'host' ? 'server' : 'layers'" :size="14" class="muted" />
          <b>{{ g.key }}</b><span class="muted small num">{{ plural(g.rows.length, 'object') }} · {{ formatBytes(g.bytes) }}</span>
          <span v-if="g.stale" class="tag warn num">{{ g.stale }} overdue</span>
          <span v-if="g.never" class="tag crit num">{{ g.never }} never</span>
          <RouterLink v-if="group === 'host' && g.key !== '(no host)'" :to="{ name: 'host', params: { name: g.key } }" class="ml small" @click.stop>Open host</RouterLink>
        </button>
        <DataTable v-if="!collapsed.has(g.key)" :rows="g.rows" :columns="groupCols" row-key="group_id" :toolbar="false" storage-key="objects-grouped" dense :initial-sort="{ key: 'name', dir: 'asc' }">
          <template #cell-name="{ row }"><span class="on"><KindIcon :kind="row.kind" :name="row.name" /><RouterLink :to="{ name: 'object', params: { group: row.group_id } }" class="strong">{{ row.name }}</RouterLink></span></template>
          <template #cell-kind="{ row }"><KindIcon :kind="row.kind" :name="row.name" label /></template>
          <template #cell-host="{ row }"><RouterLink v-if="row.host" :to="{ name: 'host', params: { name: row.host } }">{{ row.host }}</RouterLink></template>
          <template #cell-latest="{ row }"><RelativeTime :time="row.latest?.created" /></template>
          <template #cell-size="{ row }"><ByteSize v-if="row.latest && row.kind !== 'instance'" :bytes="row.latest.logical_bytes" /><span v-else class="muted">–</span></template>
          <template #cell-freshness="{ row }"><FreshnessBadge :freshness="row.freshness" :due="row.due" :overdue-at="row.overdue_at" :schedule="row.schedule" /></template>
          <template #cell-protected="{ row }"><span v-if="row.protected" class="prot"><Icon name="lock" :size="12" />{{ row.protected }}</span><span v-else class="muted">0</span></template>
        </DataTable>
      </section>
    </template>
    <p class="muted small foot"><Tooltip text="Logical size: the bytes of the object as the host read them, before deduplication and compression. What the repository really stores is on the Capacity page."><span class="u">Sizes</span></Tooltip> are logical sizes of the newest snapshot.</p>
  </div>
</template>

<style scoped>
.objs { display: flex; flex-direction: column; gap: 12px; }
.page-head { margin-bottom: 0; }
.sum { color: var(--text-2); }
.warnv { color: var(--warn-text); } .critv { color: var(--critical-text); }
.filters { display: flex; flex-direction: column; gap: 8px; }
.sels { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; }
.sels .select { width: auto; min-width: 130px; height: var(--control-h-sm); font-size: var(--fs-sm); }
.on { display: inline-flex; align-items: center; gap: 7px; min-width: 0; }
.strong { font-weight: var(--fw-medium); color: var(--text); }
.ovr { font-weight: var(--fw-medium); }
.prot { display: inline-flex; gap: 4px; align-items: center; color: var(--text-2); }
.gbar { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.search { display: flex; align-items: center; gap: 6px; height: var(--control-h-sm); padding: 0 8px; border: 1px solid var(--border-strong); border-radius: var(--r); background: var(--surface); color: var(--text-muted); width: 260px; max-width: 100%; }
.q { flex: 1; min-width: 0; border: 0; outline: 0; background: transparent; font-size: var(--fs-sm); color: var(--text); }
.grp { display: flex; flex-direction: column; gap: 6px; }
.ghead { display: flex; align-items: center; gap: 8px; padding: 6px 8px; border: 0; background: none; text-align: left; cursor: pointer; border-radius: var(--r); color: var(--text); flex-wrap: wrap; }
.ghead:hover { background: var(--surface-3); }
.tag { font-size: var(--fs-xs); padding: 1px 6px; border-radius: 999px; font-weight: var(--fw-medium); }
.tag.warn { background: var(--warn-bg); color: var(--warn-text); } .tag.crit { background: var(--critical-bg); color: var(--critical-text); }
.ml { margin-left: auto; }
.foot { margin-top: 4px; }
.u { text-decoration: underline dotted; text-underline-offset: 2px; }
@media (max-width: 600px) { .sels .select { flex: 1 1 45%; min-width: 0; } }
</style>
