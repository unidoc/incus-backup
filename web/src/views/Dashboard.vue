<script setup lang="ts">
// Overview: judge health in five seconds. First the headline (how many objects are protected), then
// what needs attention, then hosts, repositories, recent runs, capacity and replication.
// Data: GET /overview (all repositories, one call) + GET /capacity/series and /runs for the selected one.
// This is a working first version built on the shared components; the dashboard section may extend it.
import { computed } from 'vue'
import { RouterLink, type RouteLocationRaw } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import StatusPill from '@/components/StatusPill.vue'
import SeverityBadge from '@/components/SeverityBadge.vue'
import StackedBar from '@/components/charts/StackedBar.vue'
import AreaChart from '@/components/charts/AreaChart.vue'
import Meter from '@/components/Meter.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import ByteSize from '@/components/ByteSize.vue'
import Skeleton from '@/components/Skeleton.vue'
import Callout from '@/components/Callout.vue'
import SetupHealthCard from '@/components/config/SetupHealthCard.vue'
import EmptyState from '@/components/EmptyState.vue'
import DataTable from '@/components/DataTable.vue'
import { api, list } from '@/api/endpoints'
import { useResource } from '@/api/cache'
import { repos, repoNone } from '@/stores/repos'
import { hasRole } from '@/stores/session'
import { formatBytes, formatNumber, formatRatio, plural } from '@/lib/format'
import { statusOf } from '@/lib/status'
import type { ColumnDef } from '@/lib/table'
import type { HostOverview, Problem } from '@/api/types'
import { infraApi } from '@/api/sections/infra'
import { certIndex, certKey, certStatus, hostProtection, jobTarget, jobTypeText, repoHealth } from '@/lib/infra'
import { worst, type Status } from '@/lib/status'

const ov = useResource(() => 'overview', (s) => api.overview(s), { refreshOn: ['job', 'run', 'host', 'repository', 'alert'], poll: 60_000 })
const series = useResource(() => `capacity-series:${repos.current}:30`, (s) => api.capacitySeries(30, s), { enabled: () => !!repos.current && !repoNone.value, ttl: 300_000 })
const runs = useResource(() => `runs:${repos.current}:8`, (s) => api.maintenance.runs(8, s), { enabled: () => !!repos.current && !repoNone.value, refreshOn: ['run'] })

const p = computed(() => ov.data.value?.protection)
const problems = computed<Problem[]>(() => [...(ov.data.value?.problems ?? [])].sort((a, b) => (a.severity === b.severity ? 0 : a.severity === 'critical' ? -1 : 1)))
const critical = computed(() => problems.value.filter((x) => x.severity === 'critical').length)
const headState = computed(() => (critical.value ? 'critical' : problems.value.length ? 'warn' : 'ok'))
const unprotected = computed(() => (p.value ? p.value.objects - p.value.protected : 0))

function problemLink(x: Problem): RouteLocationRaw {
  switch (x.kind) {
    case 'host_offline': case 'stale_backup': case 'no_backup': return { name: 'hosts' }
    case 'corrupt_chunks': case 'verify_failed': return { name: 'verify' }
    case 'sync_failing': return { name: 'sync' }
    default: return { name: 'repositories' }
  }
}
const kindLabel: Record<string, string> = {
  stale_backup: 'Backup overdue', host_offline: 'Host offline', corrupt_chunks: 'Corrupt chunks', verify_failed: 'Verification failed', sync_failing: 'Sync failing', no_backup: 'Never backed up',
}

const hostCols: ColumnDef<HostOverview>[] = [
  { key: 'name', label: 'Host', required: true },
  { key: 'online', label: 'State', value: (h) => (h.disabled ? 'disabled' : h.online ? 'online' : 'offline') },
  { key: 'objects', label: 'Objects', align: 'right' },
  { key: 'stale', label: 'Overdue', align: 'right' },
  { key: 'never_backed_up', label: 'Never', align: 'right' },
  { key: 'last_backup', label: 'Last backup', value: (h) => h.last_backup ?? '' },
  { key: 'last_run_status', label: 'Last run', value: (h) => h.last_run_status ?? '' },
  { key: 'repository', label: 'Repository', hidden: true },
]
const hosts = computed(() => ov.data.value?.hosts ?? [])
const offline = computed(() => hosts.value.filter((h) => !h.online && !h.disabled).length)
const runs24 = computed(() => ov.data.value?.runs_24h)
const capX = computed(() => (series.data.value ?? []).map((pt) => Date.parse(pt.at)))
// One measure per chart (one axis): what the datastore really holds. Logical size is a different scale.
const capSeries = computed(() => [{ name: 'Stored', values: (series.data.value ?? []).map((pt) => pt.physical_bytes) }])
const syncJobs = computed(() => (ov.data.value?.repositories ?? []).reduce((a, r) => a + r.sync_jobs, 0))
const syncFailing = computed(() => (ov.data.value?.repositories ?? []).reduce((a, r) => a + r.sync_failing, 0))

// ---- host health strip: every backup host, worst first (overview + certificate states)
const certs = useResource(() => 'certificates', (s) => infraApi.certificates(s), { ttl: 60_000 })
const strip = computed(() => {
  const ci = certIndex(certs.data.value?.report.hosts)
  const rank: Record<Status, number> = { critical: 0, warn: 1, info: 2, neutral: 3, ok: 4 }
  return hosts.value.map((h) => {
    const prot = hostProtection(h)
    const cert = ci.get(certKey(h.repository, h.name, 'backup'))
    const state = h.disabled ? 'neutral' : !h.online ? 'critical' : 'ok'
    const run: Status = h.last_run_status === 'failed' ? 'critical' : h.last_run_status === 'partial' ? 'warn' : 'ok'
    const status: Status = h.disabled ? 'neutral' : worst(state as Status, prot.status === 'neutral' ? 'ok' : prot.status, run, cert ? certStatus(cert.state) : 'ok')
    const why = h.disabled ? 'disabled' : !h.online ? 'offline' : status === 'ok' ? 'protected' : [prot.status !== 'ok' && prot.status !== 'neutral' ? prot.label : '', run !== 'ok' ? `last run ${h.last_run_status}` : '', cert && cert.state !== 'ok' ? `certificate ${cert.state}` : ''].filter(Boolean).join(', ')
    return { h, status, why }
  }).sort((a, b) => rank[a.status] - rank[b.status] || a.h.name.localeCompare(b.h.name))
})

// ---- latest jobs and active alerts (all repositories)
const jobs = useResource(() => 'jobs:dashboard', (s) => infraApi.jobs({ limit: 6 }, s), { refreshOn: ['job'], poll: 30_000 })
const alerts = useResource(() => 'alerts:dashboard', (s) => infraApi.alerts({ active: 1, limit: 6 }, s), { refreshOn: ['alert'], poll: 60_000 })
</script>

<template>
  <div class="dash">
    <header class="page-head">
      <div><h1>Overview</h1></div>
      <div class="actions">
        <span v-if="ov.updatedAt.value" class="upd muted small">Updated <RelativeTime :time="ov.updatedAt.value" /></span>
        <button type="button" class="btn btn-sm" :disabled="ov.refreshing.value" @click="ov.refresh()"><Icon name="refresh-cw" :size="13" />Refresh</button>
      </div>
    </header>

    <Callout v-if="ov.error.value && !ov.data.value" status="critical" title="Could not load the overview" :retry="ov.refresh">{{ ov.error.value.message }}</Callout>

    <!-- 1. Headline: protected objects + what needs attention -->
    <section class="headline panel" :class="`s-${headState}`" data-testid="headline">
      <template v-if="ov.loading.value && !p"><Skeleton :lines="2" height="20px" /></template>
      <template v-else-if="p">
        <div class="hl-main">
          <p class="big num"><b>{{ formatNumber(p.protected) }}</b><span class="of"> / {{ formatNumber(p.objects) }}</span></p>
          <p class="hl-label">objects protected</p>
        </div>
        <div class="hl-bar">
          <StackedBar :segments="[
            { label: 'Protected', value: p.protected, status: 'ok' },
            { label: 'Overdue', value: p.stale, status: 'warn' },
            { label: 'Never backed up', value: p.never_backed_up, status: 'critical' },
            { label: 'No schedule', value: p.unscheduled, status: 'neutral' },
          ]" :height="12" />
        </div>
        <RouterLink :to="problems.length ? '#problems' : { name: 'objects' }" class="hl-state" :class="`s-${headState}`" data-testid="problem-count">
          <Icon :name="headState === 'ok' ? 'circle-check' : headState === 'warn' ? 'triangle-alert' : 'circle-alert'" :size="18" />
          <span v-if="problems.length"><b>{{ plural(problems.length, 'problem') }}</b> {{ problems.length === 1 ? 'requires' : 'require' }} attention</span>
          <span v-else-if="unprotected"><b>{{ plural(unprotected, 'object') }}</b> not fully protected</span>
          <span v-else><b>All clear.</b> Every object has a current backup.</span>
        </RouterLink>
      </template>
    </section>

    <section v-if="problems.length" id="problems" class="panel">
      <div class="panel-head"><h2>Needs attention</h2><span class="sub">{{ critical }} critical · {{ problems.length - critical }} warning</span></div>
      <ul class="problems">
        <li v-for="(x, i) in problems" :key="i">
          <SeverityBadge :severity="x.severity" />
          <span class="pk">{{ kindLabel[x.kind] ?? x.kind }}</span>
          <span class="ps truncate"><b>{{ x.subject }}</b> <span class="muted">{{ x.detail }}</span></span>
          <span v-if="repos.list.length > 1" class="pr muted small">{{ x.repository }}</span>
          <RouterLink :to="problemLink(x)" class="btn btn-sm btn-ghost">Open<Icon name="chevron-right" :size="13" /></RouterLink>
        </li>
      </ul>
    </section>
    <SetupHealthCard overview />

    <!-- host health strip -->
    <section v-if="strip.length" class="strip" aria-label="Host health" data-testid="host-strip">
      <RouterLink v-for="x in strip" :key="`${x.h.repository}/${x.h.name}`" :to="{ name: 'host', params: { name: x.h.name }, query: x.h.repository !== repos.current ? { repository: x.h.repository } : {} }"
        class="hchip" :class="`s-${x.status}`" :title="`${x.h.name}: ${x.why}`" :data-host="x.h.name" :data-status="x.status">
        <span class="hd" aria-hidden="true" /><span class="hname">{{ x.h.name }}</span><span class="hwhy">{{ x.why }}</span>
      </RouterLink>
    </section>

    <div class="grid">
      <div class="col">
      <!-- 2. Hosts -->
      <section class="hosts">
        <div class="sec-head"><h2>Hosts</h2><span class="sub muted">{{ plural(hosts.length, 'host') }}<template v-if="offline"> · <span class="crit">{{ offline }} offline</span></template></span><RouterLink :to="{ name: 'hosts' }" class="more">All hosts</RouterLink></div>
        <DataTable :rows="ov.data.value ? hosts : undefined" :columns="hostCols" :row-key="(h) => `${h.repository}/${h.name}`" :loading="ov.loading.value" :filterable="hosts.length > 8" :toolbar="hosts.length > 8" storage-key="dash-hosts" :initial-sort="{ key: 'online', dir: 'asc' }" dense>
          <template #cell-name="{ row }"><RouterLink :to="{ name: 'host', params: { name: row.name } }" class="strong">{{ row.name }}</RouterLink></template>
          <template #cell-online="{ row }"><StatusPill :state="row.disabled ? 'disabled' : row.online ? 'online' : 'offline'" size="sm" /></template>
          <template #cell-stale="{ row }"><span :class="{ warnv: row.stale }">{{ row.stale }}</span></template>
          <template #cell-never_backed_up="{ row }"><span :class="{ critv: row.never_backed_up }">{{ row.never_backed_up }}</span></template>
          <template #cell-last_backup="{ row }"><RelativeTime :time="row.last_backup" /></template>
          <template #cell-last_run_status="{ row }"><StatusPill v-if="row.last_run_status" :state="row.last_run_status" variant="dot" /><span v-else class="muted">–</span></template>
          <template #empty>
            <EmptyState title="No hosts yet" icon="server">Add an Incus host to start protecting its instances.
              <template v-if="hasRole('admin')" #actions><RouterLink :to="{ name: 'host-onboard' }" class="btn btn-primary btn-sm"><Icon name="plus" :size="14" />Add a host</RouterLink></template>
            </EmptyState>
          </template>
        </DataTable>
      </section>

      <!-- 5. Capacity -->
      <section class="cap panel">
        <div class="panel-head"><h2>Stored data, 30 days</h2><span class="sub">{{ repos.current }}</span><RouterLink :to="{ name: 'capacity' }" class="more">Forecast</RouterLink></div>
        <div class="panel-body">
          <AreaChart v-if="capX.length > 1" :x="capX" :series="capSeries" :y-format="(v) => formatBytes(v)" bytes :height="180" />
          <EmptyState v-else title="Not enough samples yet" icon="gauge" compact>Capacity is sampled by the maintenance schedule; the chart appears after two samples.</EmptyState>
        </div>
      </section>

      <!-- Alerts -->
      <section class="panel" data-testid="dash-alerts">
        <div class="panel-head"><h2>Active alerts</h2><RouterLink :to="{ name: 'alerts' }" class="more">All alerts</RouterLink></div>
        <ul v-if="list(alerts.data.value).length" class="wl">
          <li v-for="a in list(alerts.data.value)" :key="a.id"><SeverityBadge :severity="a.severity" /><span class="truncate wt" :title="a.detail"><b>{{ a.title }}</b><span v-if="a.state !== 'firing'" class="muted small"> · {{ a.state }}</span></span><RelativeTime :time="a.first_seen" class="ml small muted" /></li>
        </ul>
        <p v-else-if="alerts.error.value" class="panel-body muted small">Alerts are not available: {{ alerts.error.value.message }}</p>
        <p v-else class="panel-body muted small">{{ alerts.loading.value ? 'Loading…' : 'No active alert.' }}</p>
      </section>

      <!-- Jobs -->
      <section class="panel" data-testid="dash-jobs">
        <div class="panel-head"><h2>Latest jobs</h2><RouterLink :to="{ name: 'jobs' }" class="more">All jobs</RouterLink></div>
        <ul v-if="list(jobs.data.value).length" class="wl">
          <li v-for="j in list(jobs.data.value)" :key="j.id">
            <StatusPill :state="j.state" variant="dot" :label="jobTypeText[j.type] ?? j.type" />
            <span class="truncate muted small" :title="jobTarget(j.params)">{{ j.host }}<template v-if="jobTarget(j.params)"> · {{ jobTarget(j.params) }}</template></span>
            <RouterLink :to="{ name: 'job', params: { id: j.id } }" class="ml small"><RelativeTime :time="j.created_at" /></RouterLink>
          </li>
        </ul>
        <p v-else-if="jobs.error.value" class="panel-body muted small">Jobs are not available: {{ jobs.error.value.message }}</p>
        <p v-else class="panel-body muted small">{{ jobs.loading.value ? 'Loading…' : 'No job yet. Jobs are work the server asks a host to do (back up now, restore tests, restores).' }}</p>
      </section>

      </div>
      <div class="col">
      <!-- 3. Repositories -->
      <section class="repos">
        <div class="sec-head"><h2>Repositories</h2><RouterLink :to="{ name: 'repositories' }" class="more">Manage</RouterLink></div>
        <div v-if="ov.loading.value && !ov.data.value" class="panel panel-body"><Skeleton :lines="4" /></div>
        <div v-else-if="!ov.data.value?.repositories.length" class="panel">
          <EmptyState title="No repository yet" icon="database" compact>A repository is where encrypted backups are stored (a directory or an S3 bucket).
            <template v-if="hasRole('admin')" #actions><RouterLink :to="{ name: 'repository-new' }" class="btn btn-primary btn-sm">Add a repository</RouterLink></template>
          </EmptyState>
        </div>
        <article v-for="r in ov.data.value?.repositories ?? []" :key="r.name" class="panel repo" :data-repo="r.name">
          <div class="repo-head">
            <RouterLink :to="{ name: 'repository', params: { name: r.name } }" class="strong">{{ r.name }}</RouterLink>
            <span class="muted small">{{ r.backend }}</span>
            <StatusPill :status="repoHealth(r).status" :label="repoHealth(r).status === 'ok' ? 'Healthy' : repoHealth(r).status === 'critical' ? 'Critical' : 'Warning'" size="sm" class="ml" data-testid="repo-health" />
          </div>
          <ul v-if="repoHealth(r).reasons.length" class="reasons">
            <li v-for="(x, i) in repoHealth(r).reasons" :key="i" :class="`s-${x.status}`"><Icon :name="x.status === 'critical' ? 'circle-alert' : 'triangle-alert'" :size="13" />{{ x.text }}</li>
          </ul>
          <Meter v-if="r.fs_total" :value="r.fs_total - r.fs_free" :max="r.fs_total" :warn="0.8" :critical="0.9" label="Disk" :detail="`${formatBytes(r.fs_total - r.fs_free)} of ${formatBytes(r.fs_total)}`" />
          <p v-else class="muted small">Object storage: no fill level</p>
          <dl class="facts num">
            <div><dt>Stored</dt><dd><ByteSize :bytes="r.physical_bytes" /></dd></div>
            <div><dt>Dedup</dt><dd>{{ formatRatio(r.dedup_ratio) }}</dd></div>
            <div><dt>Snapshots</dt><dd>{{ formatNumber(r.snapshots) }}</dd></div>
            <div><dt>Verified</dt><dd><RelativeTime :time="r.last_verify" /></dd></div>
          </dl>
        </article>
      </section>

      <!-- 4. Recent runs -->
      <section class="runs panel">
        <div class="panel-head"><h2>Backup runs, last 24 h</h2></div>
        <div class="panel-body">
          <StackedBar v-if="runs24" :segments="[{ label: 'OK', value: runs24.ok, status: 'ok' }, { label: 'Partial', value: runs24.partial, status: 'warn' }, { label: 'Failed', value: runs24.failed, status: 'critical' }]" />
          <Skeleton v-else :lines="2" />
        </div>
        <div class="panel-head sub-head"><h2>Maintenance</h2><span class="sub">{{ repos.current }}</span><RouterLink :to="{ name: 'jobs' }" class="more">Jobs</RouterLink></div>
        <ul v-if="list(runs.data.value).length" class="runlist">
          <li v-for="(r, i) in list(runs.data.value)" :key="i"><StatusPill :state="r.status" variant="dot" :label="r.kind" /><span class="muted small" :title="r.error">{{ r.error || r.status }}</span><RelativeTime :time="r.started" class="ml small" /></li>
        </ul>
        <p v-else class="panel-body muted small">{{ runs.loading.value ? 'Loading…' : 'No maintenance runs yet.' }}</p>
      </section>

      <!-- 6. Replication -->
      <section class="sync panel">
        <div class="panel-head"><h2>Replication</h2><RouterLink :to="{ name: 'sync' }" class="more">Sync jobs</RouterLink></div>
        <div class="panel-body syncbody">
          <template v-if="syncJobs">
            <StatusPill :status="syncFailing ? 'critical' : 'ok'" :label="syncFailing ? `${syncFailing} of ${syncJobs} failing` : `${plural(syncJobs, 'sync job')} healthy`" />
          </template>
          <p v-else class="muted small">No offsite copy is configured. A sync job keeps a second copy on another server.</p>
        </div>
      </section>
      </div>
    </div>
  </div>
</template>

<style scoped>
.dash { display: flex; flex-direction: column; gap: 16px; }
.page-head { margin-bottom: 0; align-items: center; }
.upd { display: inline-flex; gap: 4px; align-items: center; }
.headline { display: grid; grid-template-columns: auto minmax(220px, 1fr) auto; gap: 28px; align-items: center; padding: 18px 20px; border-left: 3px solid var(--c); }
.headline.s-ok { --c: var(--ok); } .headline.s-warn { --c: var(--warn); } .headline.s-critical { --c: var(--critical); }
.big { font-size: var(--fs-3xl); line-height: 1; letter-spacing: -0.02em; }
.big b { font-weight: var(--fw-semibold); }
.of { color: var(--text-muted); font-weight: var(--fw-regular); }
.hl-label { color: var(--text-muted); margin-top: 4px; }
.hl-state { display: flex; align-items: center; gap: 8px; padding: 10px 14px; border-radius: var(--r); background: var(--bg-s); color: var(--fg-s); text-decoration: none; font-size: var(--fs-md); white-space: nowrap; }
.hl-state:hover { text-decoration: none; filter: brightness(0.98); }
.hl-state.s-ok { --bg-s: var(--ok-bg); --fg-s: var(--ok-text); }
.hl-state.s-warn { --bg-s: var(--warn-bg); --fg-s: var(--warn-text); }
.hl-state.s-critical { --bg-s: var(--critical-bg); --fg-s: var(--critical-text); }
.problems { list-style: none; margin: 0; padding: 0; }
.problems li { display: flex; align-items: center; gap: 12px; padding: 0 8px 0 16px; min-height: 40px; border-bottom: 1px solid var(--border); }
.problems li:last-child { border-bottom: 0; }
.pk { font-weight: var(--fw-medium); width: 150px; flex: none; }
.ps { flex: 1; }
.grid { display: grid; grid-template-columns: minmax(0, 1.65fr) minmax(0, 1fr); gap: 16px; align-items: start; }
.col { display: flex; flex-direction: column; gap: 16px; min-width: 0; }
.repos { display: flex; flex-direction: column; gap: 10px; }
.sec-head { display: flex; align-items: baseline; gap: 10px; margin-bottom: 8px; }
.sec-head h2, .panel-head h2 { font-size: var(--fs-md); }
.more { margin-left: auto; font-size: var(--fs-sm); }
.panel-head .more { margin-left: auto; }
.strong { font-weight: var(--fw-medium); color: var(--text); }
.warnv { color: var(--warn-text); font-weight: var(--fw-semibold); }
.critv, .crit { color: var(--critical-text); font-weight: var(--fw-semibold); }
.repo { padding: 12px 14px; display: flex; flex-direction: column; gap: 10px; }
.repo-head { display: flex; align-items: center; gap: 8px; }
.ml { margin-left: auto; }
.facts { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 8px; margin: 0; }
.facts dt { font-size: var(--fs-xs); color: var(--text-muted); }
.facts dd { margin: 0; font-weight: var(--fw-medium); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.bad { display: flex; gap: 6px; align-items: center; color: var(--critical-text); font-size: var(--fs-sm); }
.sub-head { border-top: 1px solid var(--border); }
.runlist { list-style: none; margin: 0; padding: 6px 16px 10px; }
.runlist li { display: flex; align-items: center; gap: 10px; min-height: 30px; }
.syncbody { display: flex; align-items: center; gap: 10px; }
.strip { display: flex; flex-wrap: wrap; gap: 6px; }
.hchip { display: inline-flex; align-items: center; gap: 7px; height: 30px; padding: 0 10px; border: 1px solid var(--border); border-radius: var(--r); background: var(--surface); color: var(--text); text-decoration: none; max-width: 100%; font-size: var(--fs-sm); }
.hchip:hover { border-color: var(--border-strong); background: var(--surface-2); text-decoration: none; }
.hd { width: 8px; height: 8px; border-radius: 50%; background: var(--neutral); flex: none; }
.hchip.s-ok .hd { background: var(--ok); } .hchip.s-warn .hd { background: var(--warn); } .hchip.s-critical .hd { background: var(--critical); }
.hchip.s-critical { border-color: var(--critical-border); } .hchip.s-warn { border-color: var(--warn-border); }
.hname { font-weight: var(--fw-medium); }
.hwhy { color: var(--text-muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.hchip.s-ok .hwhy { display: none; }
.reasons { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 3px; }
.reasons li { display: flex; gap: 6px; align-items: center; font-size: var(--fs-sm); }
.reasons li.s-critical { color: var(--critical-text); } .reasons li.s-warn { color: var(--warn-text); }
.wl { list-style: none; margin: 0; padding: 6px 16px 10px; }
.wl li { display: flex; align-items: center; gap: 10px; min-height: 32px; min-width: 0; }
.wt { min-width: 0; }
@media (max-width: 1100px) {
  .grid { grid-template-columns: 1fr; }
  .headline { grid-template-columns: 1fr; gap: 14px; }
  .hl-state { white-space: normal; }
}
@media (max-width: 600px) { .problems li { display: grid; grid-template-columns: auto 1fr auto; padding: 8px 8px 8px 12px; gap: 4px 10px; } .pk { width: auto; } .ps { grid-column: 1 / -1; grid-row: 2; white-space: normal; } .pr { display: none; } .problems .btn { grid-column: 3; grid-row: 1; } .facts { grid-template-columns: repeat(2, 1fr); } }
</style>
