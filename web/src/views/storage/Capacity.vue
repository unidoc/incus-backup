<script setup lang="ts">
// Capacity: per repository what is stored (after deduplication) against what was backed up (before),
// how fast it grows (7/30/90-day trends), when the disk reaches 80 % and full, and where the data comes
// from (largest objects, share per host). Object storage has no free space to forecast: said plainly.
//
// GET /capacity takes a fresh capacity sample (it walks the datastore), so it is called once per
// repository when its tab opens, never polled; the cards for all repositories use the overview's
// last sample instead.
import { computed, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import StatTile from '@/components/StatTile.vue'
import StatusPill from '@/components/StatusPill.vue'
import Meter from '@/components/Meter.vue'
import Callout from '@/components/Callout.vue'
import EmptyState from '@/components/EmptyState.vue'
import Skeleton from '@/components/Skeleton.vue'
import DataTable from '@/components/DataTable.vue'
import ByteSize from '@/components/ByteSize.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AreaChart from '@/components/charts/AreaChart.vue'
import StackedBar from '@/components/charts/StackedBar.vue'
import KindIcon from '@/components/infra/KindIcon.vue'
import FilterChips from '@/components/infra/FilterChips.vue'
import { api, list } from '@/api/endpoints'
import { useResource } from '@/api/cache'
import { infraApi, type ObjectStat } from '@/api/sections/infra'
import { repos } from '@/stores/repos'
import { daysUntil, inDays, linearForecast, projectionStatus, shareBy } from '@/lib/infra'
import { formatBytes, formatDate, formatPercent, formatRate, formatRatio } from '@/lib/format'
import type { ColumnDef } from '@/lib/table'

const ov = useResource(() => 'overview', (s) => api.overview(s), { refreshOn: ['repository'], poll: 120_000 })
const route = useRoute()
const router = useRouter()
// ?repository= preselects (links from the overview); the choice is kept in the URL
const sel = ref(typeof route.query.repository === 'string' && route.query.repository ? route.query.repository : repos.current)
watch(sel, (r) => { if (r !== route.query.repository) router.replace({ query: { ...route.query, repository: r } }) })
watch(() => repos.current, (r) => { if (!sel.value || !repos.list.some((x) => x.name === sel.value)) sel.value = r })
const repoInfo = computed(() => repos.list.find((r) => r.name === sel.value) ?? null)
const ovRepo = computed(() => ov.data.value?.repositories.find((r) => r.name === sel.value) ?? null)

const cap = useResource(() => `capacity:${sel.value}`, (s) => infraApi.capacityOf(sel.value, s), { enabled: () => !!sel.value, ttl: 300_000 })
const range = ref('90')
const series = useResource(() => `capacity-series:${sel.value}:${range.value}`, (s) => infraApi.capacitySeriesOf(sel.value, Number(range.value), s), { enabled: () => !!sel.value, ttl: 300_000 })
const stats = useResource(() => `objects-stats-of:${sel.value}`, (s) => infraApi.objectStatsOf(sel.value, s), { enabled: () => !!sel.value, ttl: 120_000 })

const c = computed(() => cap.data.value ?? null)
const isS3 = computed(() => repoInfo.value?.backend === 's3' || (c.value ? c.value.fs_total === 0 : false))
const g = (d: string) => c.value?.growth_per_day?.[d] ?? null
const used = computed(() => (c.value && c.value.fs_total ? c.value.fs_total - c.value.fs_free : 0))
const d80 = computed(() => daysUntil(c.value?.projected80))
const dFull = computed(() => daysUntil(c.value?.projected_full))
const growthStatus = computed(() => (isS3.value ? 'neutral' : projectionStatus(d80.value)))

// ---- chart: samples + a 30-day-trend forecast as a second line of the same measure
const pts = computed(() => list(series.data.value))
// forecast as far ahead as there is history behind it (at least a week, at most a third of the range)
const spanDays = computed(() => (pts.value.length > 1 ? (Date.parse(pts.value[pts.value.length - 1].at) - Date.parse(pts.value[0].at)) / 86400_000 : 0))
const forecastDays = computed(() => Math.round(Math.min(Number(range.value) / 3, Math.max(7, spanDays.value))))
const fc = computed(() => {
  const last = pts.value[pts.value.length - 1]
  if (!last) return []
  return linearForecast(Date.parse(last.at), last.physical_bytes, g('30') ?? g('7'), forecastDays.value, Math.max(1, Math.round(forecastDays.value / 30)))
})
const chartX = computed(() => [...pts.value.map((p) => Date.parse(p.at)), ...fc.value.map((p) => p.at)])
const chartSeries = computed(() => {
  const n = pts.value.length
  const stored = [...pts.value.map((p) => p.physical_bytes), ...fc.value.map(() => null)]
  const out: Array<{ name: string; values: Array<number | null> }> = [{ name: 'Stored', values: stored }]
  if (fc.value.length) out.push({ name: 'Trend (30 days)', values: [...pts.value.map((p, i) => (i === n - 1 ? p.physical_bytes : null)), ...fc.value.map((p) => p.value)] })
  return out
})
const ranges = [{ value: '30', label: '30 days' }, { value: '90', label: '90 days' }, { value: '365', label: '1 year' }]

// ---- where the data comes from
const data = computed(() => (stats.data.value ?? []).filter((o) => o.kind !== 'instance'))
const latest = (o: ObjectStat) => o.latest?.logical_bytes ?? 0
const totalLatest = computed(() => data.value.reduce((a, o) => a + latest(o), 0))
const top = computed(() => data.value.slice().sort((a, b) => latest(b) - latest(a)).slice(0, 15))
const hostShare = computed(() => shareBy(data.value, (o) => o.host || '(no host)', latest, 4))
const hostRows = computed(() => {
  const m = new Map<string, { host: string; objects: number; latest: number; uploaded: number }>()
  for (const o of data.value) {
    const k = o.host || '(no host)'
    const r = m.get(k) ?? { host: k, objects: 0, latest: 0, uploaded: 0 }
    r.objects++; r.latest += latest(o); r.uploaded += o.uploaded_bytes
    m.set(k, r)
  }
  return [...m.values()].sort((a, b) => b.latest - a.latest)
})
const topCols: ColumnDef<ObjectStat>[] = [
  { key: 'name', label: 'Object', required: true },
  { key: 'host', label: 'Host' },
  { key: 'size', label: 'Newest snapshot', align: 'right', value: latest },
  { key: 'share', label: 'Share', value: (o) => (totalLatest.value ? latest(o) / totalLatest.value : 0), format: (o) => formatPercent(totalLatest.value ? latest(o) / totalLatest.value : 0) },
  { key: 'uploaded_bytes', label: 'New data', align: 'right' },
]
type HostShareRow = { host: string; objects: number; latest: number; uploaded: number }
const hostCols: ColumnDef<HostShareRow>[] = [
  { key: 'host', label: 'Host', required: true },
  { key: 'objects', label: 'Objects', align: 'right' },
  { key: 'latest', label: 'Newest snapshots', align: 'right', format: (r) => formatBytes(r.latest) },
  { key: 'uploaded', label: 'New data', align: 'right', format: (r) => formatBytes(r.uploaded) },
]
const dateAndIn = (t: string | null | undefined) => (t ? `${formatDate(t)}, ${inDays(daysUntil(t))}` : 'not in sight')
</script>

<template>
  <div class="cap-page">
    <header class="page-head">
      <div>
        <h1>Capacity</h1>
        <p class="lede">What each repository stores after deduplication, how fast it grows and when the disk fills up.</p>
      </div>
      <div class="actions"><button type="button" class="btn btn-sm" :disabled="cap.refreshing.value" @click="cap.refresh(); series.refresh(); stats.refresh()"><Icon name="refresh-cw" :size="13" />Measure now</button></div>
    </header>

    <EmptyState v-if="repos.loaded && !repos.list.length" title="No repository yet" icon="database">Capacity is measured per repository. <RouterLink :to="{ name: 'repository-new' }">Add a repository</RouterLink>.</EmptyState>

    <template v-else>
      <!-- every repository at a glance (overview's last sample, no new measurement) -->
      <div class="cards" data-testid="cap-cards">
        <button v-for="r in ov.data.value?.repositories ?? []" :key="r.name" type="button" class="card panel" :class="{ on: r.name === sel }" :aria-pressed="r.name === sel" :data-repo="r.name" @click="sel = r.name">
          <span class="cin"><span class="ch"><Icon :name="r.backend === 's3' ? 'cloud' : 'hard-drive'" :size="14" /><b>{{ r.name }}</b><span class="muted small">{{ r.backend === 's3' ? 'S3' : 'filesystem' }}</span></span>
          <span class="cv num"><ByteSize :bytes="r.physical_bytes" /><span class="muted small"> stored · {{ formatRatio(r.dedup_ratio) }} dedup</span></span>
          <Meter v-if="r.fs_total" :value="r.fs_total - r.fs_free" :max="r.fs_total" :warn="0.8" :critical="0.9" size="sm" :detail="`${formatPercent((r.fs_total - r.fs_free) / r.fs_total)} of ${formatBytes(r.fs_total)}`" label="Disk" />
          <span v-else class="muted small">Object storage: no fill level</span></span>
        </button>
        <template v-if="ov.loading.value && !ov.data.value"><div v-for="i in 2" :key="i" class="card panel"><Skeleton :lines="3" /></div></template>
      </div>


      <Callout v-if="cap.error.value && !c" status="critical" :title="`Could not measure ${sel}`" :retry="cap.refresh">{{ cap.error.value.message }}</Callout>

      <div class="tiles" data-testid="cap-tiles">
        <StatTile label="Stored" :value="c ? formatBytes(c.physical_bytes) : null" :loading="cap.loading.value" :hint="c ? `${c.chunks.toLocaleString('en')} chunks` : ''" />
        <StatTile label="Backed up (logical)" :value="c ? formatBytes(c.logical_bytes) : null" :loading="cap.loading.value" :hint="c ? `${c.snapshots} snapshots, before deduplication` : ''" />
        <StatTile label="Deduplication" :value="c ? formatRatio(c.dedup_ratio) : null" :loading="cap.loading.value" :hint="c && c.logical_bytes ? `saves ${formatBytes(c.logical_bytes - c.physical_bytes)}` : ''" />
        <StatTile label="Free space" :value="c ? (isS3 ? 'n/a' : formatBytes(c.fs_free)) : null" :loading="cap.loading.value" :status="!isS3 && c && c.fs_total && c.fs_free / c.fs_total < 0.1 ? 'critical' : !isS3 && c && c.fs_total && c.fs_free / c.fs_total < 0.2 ? 'warn' : undefined"
          :hint="isS3 ? 'object storage reports none' : c ? `of ${formatBytes(c.fs_total)} (${formatPercent(c.fs_total ? used / c.fs_total : 0)} used)` : ''" />
        <StatTile label="Growth" :value="c ? (g('30') === null ? 'unknown' : formatRate(g('30'))) : null" :loading="cap.loading.value" :hint="c ? `30-day trend; 7 d ${g('7') === null ? '–' : formatRate(g('7'))}, 90 d ${g('90') === null ? '–' : formatRate(g('90'))}` : ''" />
      </div>

      <div class="grid">
        <section class="panel" data-testid="cap-chart">
          <div class="panel-head"><h2>Stored data</h2><span class="sub">after deduplication<template v-if="fc.length">, with the 30-day trend for {{ forecastDays }} days</template></span><span class="ml"><FilterChips v-model="range" :options="ranges" label="Range" /></span></div>
          <div class="panel-body">
            <Skeleton v-if="series.loading.value && !series.data.value" :lines="5" />
            <AreaChart v-else-if="chartX.length > 1" :x="chartX" :series="chartSeries" :y-format="(v) => formatBytes(v)" bytes :height="220" />
            <EmptyState v-else title="Not enough samples yet" icon="gauge" compact>The maintenance schedule samples capacity; the chart needs two samples. Use Measure now to take one.</EmptyState>
          </div>
        </section>

        <section class="panel" data-testid="cap-forecast">
          <div class="panel-head"><h2>Forecast</h2><StatusPill v-if="c && !isS3" :status="growthStatus" :label="growthStatus === 'critical' ? 'Act now' : growthStatus === 'warn' ? 'Plan ahead' : growthStatus === 'ok' ? 'Comfortable' : 'Unknown'" size="sm" class="ml" /></div>
          <div class="panel-body stack">
            <Skeleton v-if="cap.loading.value && !c" :lines="4" />
            <template v-else-if="c && isS3">
              <Callout status="info" title="No free-space forecast for object storage">An S3 bucket reports no size limit, so no fill date can be computed. Watch the growth rate and your provider's quota or bill instead.</Callout>
              <dl class="fc num">
                <div><dt>Growth, 7 days</dt><dd>{{ g('7') === null ? 'not enough data' : formatRate(g('7')) }}</dd></div>
                <div><dt>Growth, 30 days</dt><dd>{{ g('30') === null ? 'not enough data' : formatRate(g('30')) }}</dd></div>
                <div><dt>Growth, 90 days</dt><dd>{{ g('90') === null ? 'not enough data' : formatRate(g('90')) }}</dd></div>
                <div><dt>In a year at the 30-day rate</dt><dd>{{ g('30') === null ? '–' : formatBytes(c.physical_bytes + (g('30') ?? 0) * 365) }}</dd></div>
              </dl>
            </template>
            <template v-else-if="c">
              <Meter :value="used" :max="c.fs_total" :warn="0.8" :critical="0.9" label="Disk" :detail="`${formatBytes(used)} of ${formatBytes(c.fs_total)}`" />
              <p class="muted small">The disk may hold other data than this repository; the fill level counts everything on it.</p>
              <dl class="fc num">
                <div><dt>80 % full</dt><dd :class="`s-${projectionStatus(d80)}`">{{ dateAndIn(c.projected80) }}</dd></div>
                <div><dt>Full</dt><dd :class="`s-${projectionStatus(dFull)}`">{{ dateAndIn(c.projected_full) }}</dd></div>
                <div><dt>Growth, 7 days</dt><dd>{{ g('7') === null ? 'not enough data' : formatRate(g('7')) }}</dd></div>
                <div><dt>Growth, 30 days</dt><dd>{{ g('30') === null ? 'not enough data' : formatRate(g('30')) }}</dd></div>
                <div><dt>Growth, 90 days</dt><dd>{{ g('90') === null ? 'not enough data' : formatRate(g('90')) }}</dd></div>
              </dl>
              <p class="muted small">Dates follow the growth trend of the stored data. Retention and garbage collection free space; new hosts add to it.</p>
            </template>
            <p v-if="cap.updatedAt.value" class="muted small">Measured <RelativeTime :time="cap.updatedAt.value" />.</p>
          </div>
        </section>
      </div>

      <div class="grid">
        <section data-testid="cap-top">
          <div class="sec-head"><h2>Largest objects</h2><span class="muted small">by the logical size of their newest snapshot</span></div>
          <DataTable :rows="stats.data.value === undefined ? undefined : top" :columns="topCols" row-key="group_id" :loading="stats.loading.value" :error="stats.error.value" :retry="stats.refresh" :toolbar="false" dense storage-key="cap-top">
            <template #cell-name="{ row }"><span class="on"><KindIcon :kind="row.kind" :name="row.name" /><RouterLink :to="{ name: 'object', params: { group: row.group_id } }" class="strong">{{ row.name }}</RouterLink><span class="muted small nowrap">{{ row.project }}</span></span></template>
            <template #cell-host="{ row }"><RouterLink v-if="row.host" :to="{ name: 'host', params: { name: row.host } }">{{ row.host }}</RouterLink></template>
            <template #cell-size="{ row }"><ByteSize :bytes="latest(row)" /></template>
            <template #cell-share="{ row }"><span class="nowrap"><span class="bar" :title="formatPercent(totalLatest ? latest(row) / totalLatest : 0)"><span class="fillb" :style="{ width: `${totalLatest ? (latest(row) / totalLatest) * 100 : 0}%` }" /></span><span class="small num">{{ formatPercent(totalLatest ? latest(row) / totalLatest : 0) }}</span></span></template>
            <template #cell-uploaded_bytes="{ row }"><ByteSize :bytes="row.uploaded_bytes" /></template>
            <template #empty><EmptyState title="No objects yet" icon="box" compact>Objects appear after the first backup.</EmptyState></template>
          </DataTable>
        </section>
        <section class="panel" data-testid="cap-hosts">
          <div class="panel-head"><h2>Share per host</h2></div>
          <div class="panel-body stack">
            <StackedBar v-if="hostShare.length" :segments="hostShare" :format="(v) => formatBytes(v)" :height="12" />
            <DataTable :rows="stats.data.value === undefined ? undefined : hostRows" :columns="hostCols" row-key="host" :toolbar="false" dense>
              <template #cell-host="{ row }"><RouterLink v-if="row.host !== '(no host)'" :to="{ name: 'host', params: { name: row.host } }" class="strong">{{ row.host }}</RouterLink><span v-else class="muted">{{ row.host }}</span></template>
            </DataTable>
          </div>
        </section>
      </div>
      <p class="muted small foot">Deduplication shares chunks between objects and snapshots, so the stored size of one object is not defined. "New data" sums what the hosts reported as newly uploaded for the snapshots that still exist: a guide to which objects bring new data, not an exact share.</p>
    </template>
  </div>
</template>

<style scoped>
.cap-page { display: flex; flex-direction: column; gap: 14px; }
.page-head { margin-bottom: 0; }
.cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(260px, 1fr)); gap: 10px; }
.card { display: flex; flex-direction: column; align-items: stretch; justify-content: flex-start; gap: 8px; padding: 12px 14px; text-align: left; cursor: pointer; color: inherit; font: inherit; }
.card:hover { border-color: var(--border-strong); }
.card.on { border-color: var(--accent); box-shadow: inset 0 0 0 1px var(--accent); }
.cin { display: flex; flex-direction: column; gap: 8px; width: 100%; text-align: left; }
.ch { display: flex; align-items: center; gap: 8px; }
.cv { font-size: var(--fs-lg); font-weight: var(--fw-semibold); }
.tiles { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: 10px; }
.grid { display: grid; grid-template-columns: minmax(0, 1.6fr) minmax(0, 1fr); gap: 14px; align-items: start; }
.panel-head { flex-wrap: wrap; }
.ml { margin-left: auto; }
.fc { display: grid; grid-template-columns: 1fr 1fr; gap: 10px 16px; margin: 0; }
.fc dt { font-size: var(--fs-xs); color: var(--text-muted); }
.fc dd { margin: 0; font-weight: var(--fw-medium); }
.fc dd.s-critical { color: var(--critical-text); } .fc dd.s-warn { color: var(--warn-text); }
.sec-head { display: flex; align-items: baseline; gap: 10px; margin-bottom: 8px; flex-wrap: wrap; }
.sec-head h2 { font-size: var(--fs-md); }
.on { display: inline-flex; align-items: center; gap: 7px; min-width: 0; }
.strong { font-weight: var(--fw-medium); color: var(--text); }
.bar { display: inline-block; width: 70px; height: 6px; border-radius: 3px; background: var(--surface-3); vertical-align: middle; margin-right: 6px; overflow: hidden; }
.fillb { display: block; height: 100%; background: var(--series-1); border-radius: 3px; }
.foot { max-width: 100ch; }
@media (max-width: 1200px) { .tiles { grid-template-columns: repeat(3, minmax(0, 1fr)); } }
@media (max-width: 1000px) { .grid { grid-template-columns: 1fr; } }
@media (max-width: 600px) { .tiles { grid-template-columns: repeat(2, minmax(0, 1fr)); } .fc { grid-template-columns: 1fr; } }
</style>
