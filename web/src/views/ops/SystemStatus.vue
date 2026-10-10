<script setup lang="ts">
// The health of the machine and process this server runs on: host, process, disks (state directory and
// every filesystem repository), PostgreSQL, listeners, the clock against this browser, charts of the
// last hours (one sample a minute, kept 7 days), the support report and the opt-in update check.
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import StatTile from '@/components/StatTile.vue'
import KeyValue from '@/components/KeyValue.vue'
import Meter from '@/components/Meter.vue'
import Callout from '@/components/Callout.vue'
import Modal from '@/components/Modal.vue'
import Spinner from '@/components/Spinner.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import FormField from '@/components/form/FormField.vue'
import AreaChart from '@/components/charts/AreaChart.vue'
import Skeleton from '@/components/Skeleton.vue'
import { useResource, invalidate } from '@/api/cache'
import { api, list } from '@/api/endpoints'
import { opsApi, saveBytes, type SystemStatus, type UpdateStatus } from '@/api/sections/ops'
import { hasRole } from '@/stores/session'
import { toast, errorText } from '@/stores/toast'
import { clockSkewMs, skewStatus } from '@/lib/tasks'
import { formatBytes, formatDuration, formatNumber, formatPercent } from '@/lib/format'

// status, with the request's timing for the clock comparison
const st = ref<SystemStatus | null>(null)
const stErr = ref('')
const skew = ref(0)
async function loadStatus() {
  const sent = Date.now()
  try {
    const s = await opsApi.system.status()
    skew.value = clockSkewMs(s.server_time, sent, Date.now())
    st.value = s
    stErr.value = ''
  } catch (e) {
    stErr.value = errorText(e)
  }
}
void loadStatus()
const timer = setInterval(loadStatus, 15_000)
onBeforeUnmount(() => clearInterval(timer))

const range = ref(24)
const samples = useResource(() => `system:samples:${range.value}`, (s) => opsApi.system.samples(range.value, 360, s), { ttl: 60_000, poll: 60_000 })
const pts = computed(() => list(samples.data.value))
const xs = computed(() => pts.value.map((p) => Date.parse(p.at)))
const memTotal = computed(() => st.value?.host.mem_total_bytes ?? 0)

const mem = computed(() => st.value ? st.value.host.mem_used_bytes / Math.max(1, st.value.host.mem_total_bytes) : 0)
const skewLabel = computed(() => {
  const a = Math.abs(skew.value)
  if (a < 1000) return 'in sync'
  return `${formatDuration(a)} ${skew.value > 0 ? 'ahead' : 'behind'}`
})

// ---- support report
const repOpen = ref(false)
const repHosts = ref(false)
const repBusy = ref(false)
async function downloadReport() {
  repBusy.value = true
  try {
    const b = await opsApi.system.supportReport(repHosts.value)
    const stamp = new Date().toISOString().replace(/[-:]/g, '').replace('T', '-').slice(0, 15)
    saveBytes(b, `incus-backup-support-${stamp}.tar.gz`)
    toast.success('Support report downloaded', 'Secrets are never included; read it before you send it.')
    repOpen.value = false
  } catch (e) {
    toast.error(e, 'Could not build the support report')
  } finally {
    repBusy.value = false
  }
}

// ---- update check (setting update_check, off by default)
const upd = useResource(() => 'system:update', (s) => opsApi.system.update(s), { ttl: 60_000 })
const settings = useResource(() => 'settings', (s) => api.settings.get(s), { enabled: () => hasRole('admin') })
const uc = ref({ on: false, url: '' })
watch(() => settings.data.value, (s) => { if (s) uc.value = { on: s.update_check === 'on', url: s.update_check_url ?? '' } }, { immediate: true })
const ucBusy = ref(false)
const urlErr = computed(() => (uc.value.url && !/^https?:\/\/[^/\s]+/.test(uc.value.url) ? 'Must be an http(s) URL' : ''))
async function saveUpdate() {
  ucBusy.value = true
  try {
    await api.settings.put({ update_check: uc.value.on ? 'on' : 'off', update_check_url: uc.value.url.trim() })
    invalidate('settings')
    toast.success(uc.value.on ? 'Update check turned on' : 'Update check turned off', uc.value.on ? 'The server asks the release feed once a day.' : 'The server contacts nothing.')
    if (uc.value.on) await checkNow()
    else void upd.refresh()
  } catch (e) {
    toast.error(e, 'Could not save the update check')
  } finally {
    ucBusy.value = false
  }
}
const checking = ref(false)
async function checkNow() {
  checking.value = true
  try {
    const r = await opsApi.system.checkUpdate()
    if (r.error) toast.warn('Update check failed', r.error)
    else if (r.status.update_available) toast.info(`Version ${r.status.latest} is available`, `This server runs ${r.status.current}.`)
    else toast.success('This server is up to date', `Latest release: ${r.status.latest}.`)
    invalidate('system:update')
    void upd.refresh()
  } catch (e) {
    toast.error(e, 'Could not check for updates')
  } finally {
    checking.value = false
  }
}
const u = computed<UpdateStatus | undefined>(() => upd.data.value)
</script>

<template>
  <div class="page">
    <header class="page-head">
      <div>
        <h1>Server status</h1>
        <p class="lede">The machine and process behind this console: load, memory, disks, PostgreSQL and the listeners. Charts keep one sample a minute for 7 days.</p>
      </div>
      <div class="actions">
        <RouterLink v-if="hasRole('admin')" :to="{ name: 'server-logs' }" class="btn"><Icon name="file" />Server log</RouterLink>
        <button v-if="hasRole('admin')" type="button" class="btn" data-testid="support-report" @click="repOpen = true"><Icon name="download" />Support report</button>
      </div>
    </header>

    <Callout v-if="stErr && !st" status="critical" title="Could not load the server status" :retry="loadStatus">{{ stErr }}</Callout>
    <Callout v-if="u?.update_available" status="info" :title="`incus-backup ${u.latest} is available`">
      This server runs {{ u.current }}. <a v-if="u.release_url" :href="u.release_url" target="_blank" rel="noopener noreferrer">Read the release notes</a>, then upgrade the server package and the hosts.
    </Callout>
    <Callout v-if="st && skewStatus(skew) !== 'ok'" :status="skewStatus(skew) === 'critical' ? 'critical' : 'warn'" title="The server clock differs from this computer">
      The server is {{ skewLabel }}. Sign-in codes (TOTP), certificates and schedules depend on the time; check NTP on the server{{ skewStatus(skew) === 'critical' ? ' now' : '' }}.
    </Callout>

    <div class="tiles">
      <StatTile label="Version" :value="st?.version" :hint="st ? `Go ${st.go_version.replace(/^go/, '')}` : ''" :loading="!st" />
      <StatTile label="Server up" :value="st ? formatDuration(st.uptime_seconds * 1000, { units: 2 }) : null" :hint="st ? `machine ${formatDuration(st.host.uptime_seconds * 1000, { units: 1 })}` : ''" :loading="!st" />
      <StatTile label="CPU" :value="st ? (st.host.cpu_percent < 0 ? '…' : `${formatNumber(st.host.cpu_percent, 0)} %`) : null" :hint="st ? (st.host.cpu_percent < 0 ? `measuring · ${st.host.cpus} CPUs` : `load ${st.host.load1.toFixed(2)} · ${st.host.cpus} CPUs`) : ''" :status="st && st.host.load1 > st.host.cpus * 1.5 ? 'warn' : undefined" :loading="!st" />
      <StatTile label="Memory" :value="st ? formatPercent(mem, 0) : null" :hint="st ? `${formatBytes(st.host.mem_used_bytes)} of ${formatBytes(st.host.mem_total_bytes)}` : ''" :status="mem > 0.9 ? 'critical' : mem > 0.8 ? 'warn' : undefined" :loading="!st" />
      <StatTile label="Clock" :value="st ? skewLabel : null" hint="server against this browser" :status="st ? skewStatus(skew) : undefined" :loading="!st" />
      <StatTile label="Database" :value="st ? formatBytes(st.database.size_bytes) : null" :hint="st ? (st.database.ok ? `${st.database.connections} connections · ${st.database.latency_ms.toFixed(1)} ms` : 'not answering') : ''" :status="st && !st.database.ok ? 'critical' : undefined" :loading="!st" />
    </div>

    <section class="panel">
      <div class="panel-head"><Icon name="activity" :size="16" /><h2>History</h2>
        <span class="sub">one sample a minute</span>
        <div class="seg" role="group" aria-label="Range">
          <button v-for="h in [6, 24, 168]" :key="h" type="button" class="btn btn-sm" :class="{ on: range === h }" :aria-pressed="range === h" @click="range = h">{{ h === 168 ? '7 d' : `${h} h` }}</button>
        </div>
      </div>
      <div class="panel-body">
        <Skeleton v-if="samples.loading.value" :lines="4" />
        <Callout v-else-if="samples.error.value" status="critical" title="Could not load the samples" :retry="samples.refresh">{{ samples.error.value.message }}</Callout>
        <p v-else-if="pts.length < 2" class="muted">Not enough samples yet: the server takes one a minute; charts appear after a few minutes.</p>
        <div v-else class="charts">
          <AreaChart :x="xs" :series="[{ name: 'CPU %', values: pts.map((p) => p.cpu_percent) }]" title="CPU" :y-format="(v) => `${formatNumber(v, 0)} %`" hours :height="150" />
          <AreaChart :x="xs" :series="[{ name: 'Memory used', values: pts.map((p) => p.mem_used_bytes) }]" title="Memory" :subtitle="memTotal ? `of ${formatBytes(memTotal)}` : ''" bytes :y-format="(v) => formatBytes(v)" hours :height="150" />
          <AreaChart :x="xs" :series="[{ name: 'Used', values: pts.map((p) => p.disk_used_bytes) }]" title="State directory disk" :subtitle="pts.length ? `of ${formatBytes(pts[pts.length - 1].disk_total_bytes)}` : ''" bytes :y-format="(v) => formatBytes(v)" hours :height="150" :zero="false" />
          <AreaChart :x="xs" :series="[{ name: 'Database', values: pts.map((p) => p.db_bytes) }]" title="Database size" bytes :y-format="(v) => formatBytes(v)" hours :height="150" :zero="false" />
        </div>
      </div>
    </section>

    <div v-if="st" class="grid">
      <section class="panel">
        <div class="panel-head"><Icon name="hard-drive" :size="16" /><h2>Disks</h2></div>
        <div class="panel-body disks">
          <div v-for="d in [st.state_dir, ...st.datastores]" :key="d.label + d.path" class="disk">
            <Meter v-if="!d.error" :value="d.used_bytes" :max="d.total_bytes" :warn="0.8" :critical="0.9" :label="d.label" :detail="`${formatBytes(d.free_bytes)} free of ${formatBytes(d.total_bytes)}`" />
            <div v-else class="small"><b>{{ d.label }}</b> <span class="muted">{{ d.error }}</span></div>
            <code class="mono path">{{ d.path }}</code>
          </div>
        </div>
      </section>

      <section class="panel">
        <div class="panel-head"><Icon name="server" :size="16" /><h2>Host</h2></div>
        <div class="panel-body">
          <KeyValue :items="[
            { key: 'h', label: 'Hostname', value: st.host.hostname },
            { key: 'os', label: 'System', value: st.host.os },
            { key: 'k', label: 'Kernel', value: st.host.kernel, mono: true },
            { key: 'l', label: 'Load (1/5/15 min)', value: `${st.host.load1.toFixed(2)} / ${st.host.load5.toFixed(2)} / ${st.host.load15.toFixed(2)}` },
            { key: 'm', label: 'Memory available', value: formatBytes(st.host.mem_available_bytes) },
            { key: 's', label: 'Swap used', value: st.host.swap_total_bytes ? `${formatBytes(st.host.swap_total_bytes - st.host.swap_free_bytes)} of ${formatBytes(st.host.swap_total_bytes)}` : 'none' },
          ]" />
        </div>
      </section>

      <section class="panel">
        <div class="panel-head"><Icon name="activity" :size="16" /><h2>Server process</h2></div>
        <div class="panel-body">
          <KeyValue :items="[
            { key: 'pid', label: 'PID', value: st.process.pid },
            { key: 'rss', label: 'Resident memory', value: formatBytes(st.process.rss_bytes) },
            { key: 'heap', label: 'Go heap', value: `${formatBytes(st.process.heap_bytes)} (of ${formatBytes(st.process.heap_sys_bytes)} reserved)` },
            { key: 'g', label: 'Goroutines', value: formatNumber(st.process.goroutines) },
            { key: 'fd', label: 'Open files', value: formatNumber(st.process.open_fds) },
            { key: 'gc', label: 'GC cycles', value: `${formatNumber(st.process.gc_cycles)} (${formatNumber(st.process.gc_pause_total_seconds * 1000, 1)} ms paused)` },
          ]" />
        </div>
      </section>

      <section class="panel">
        <div class="panel-head"><Icon name="database" :size="16" /><h2>PostgreSQL</h2><StatusPill :status="st.database.ok ? 'ok' : 'critical'" :label="st.database.ok ? 'answering' : 'not answering'" size="sm" /></div>
        <div class="panel-body">
          <Callout v-if="!st.database.ok" status="critical" title="The database does not answer">{{ st.database.error }}</Callout>
          <KeyValue :items="[
            { key: 'v', label: 'Version', value: st.database.version },
            { key: 'sz', label: 'Size', value: formatBytes(st.database.size_bytes) },
            { key: 'c', label: 'Connections', value: `${st.database.connections} of ${st.database.max_connections} allowed` },
            { key: 'p', label: 'This server\'s pool', value: `${st.database.pool_in_use} in use, ${st.database.pool_idle} idle, max ${st.database.pool_max}` },
            { key: 'lat', label: 'Query latency', value: `${st.database.latency_ms.toFixed(1)} ms` },
            { key: 'mig', label: 'Schema', value: st.database.migrations.length ? `${st.database.migrations.length} migrations, latest ${st.database.migrations[st.database.migrations.length - 1].replace(/\.sql$/, '')}` : 'unknown' },
          ]" />
          <details class="mig"><summary class="small">Applied migrations</summary><ul class="mono small"><li v-for="m in st.database.migrations" :key="m">{{ m }}</li></ul></details>
        </div>
      </section>

      <section class="panel">
        <div class="panel-head"><Icon name="wifi" :size="16" /><h2>Listeners</h2></div>
        <div class="panel-body">
          <p v-if="!st.listeners.length" class="muted small">This process reports no listeners (it is not the daemon).</p>
          <ul v-else class="ls">
            <li v-for="l in st.listeners" :key="l.name"><b>{{ l.name }}</b><code class="mono">{{ l.addr }}</code><span class="muted small">{{ l.detail }}</span></li>
          </ul>
        </div>
      </section>

      <section class="panel">
        <div class="panel-head"><Icon name="refresh-cw" :size="16" /><h2>Updates</h2>
          <StatusPill v-if="u" :status="!u.enabled ? 'neutral' : u.update_available ? 'info' : u.error ? 'warn' : 'ok'" :label="!u.enabled ? 'off' : u.update_available ? `${u.latest} available` : u.error ? 'check failed' : 'up to date'" size="sm" />
        </div>
        <div class="panel-body form-grid">
          <p class="small muted">Off by default: the server contacts nothing. When on, it asks the release feed once a day for the latest version number and sends nothing about itself. A newer release shows here, as a banner and as an informational alert.</p>
          <p v-if="u?.enabled" class="small">Running {{ u.current }}<template v-if="u.latest">, latest {{ u.latest }}</template><template v-if="u.checked_at">, checked <RelativeTime :time="u.checked_at" /></template>.<span v-if="u.error" class="err"> Last check failed: {{ u.error }}</span></p>
          <template v-if="hasRole('admin')">
            <SwitchInput v-model="uc.on" label="Check for new releases once a day" data-testid="update-check-toggle" />
            <FormField v-slot="f" label="Release feed" optional :error="urlErr" hint="Empty: https://github.com/unidoc/incus-backup/releases/latest. A URL answering JSON {&quot;version&quot;: &quot;x.y.z&quot;} also works (an internal mirror).">
              <input :id="f.id" v-model="uc.url" class="input" placeholder="https://github.com/unidoc/incus-backup/releases/latest" :aria-invalid="f.invalid" />
            </FormField>
            <div class="form-actions">
              <button type="button" class="btn btn-primary" :disabled="ucBusy || !!urlErr" @click="saveUpdate"><Spinner v-if="ucBusy" :size="13" />Save</button>
              <button type="button" class="btn" :disabled="checking || !u?.enabled" :title="!u?.enabled ? 'Turn the check on first' : ''" @click="checkNow"><Spinner v-if="checking" :size="13" /><Icon v-else name="refresh-cw" />Check now</button>
            </div>
          </template>
        </div>
      </section>
    </div>

    <Modal v-model:open="repOpen" title="Download a support report" description="A .tar.gz for a support ticket">
      <div class="form-grid">
        <p class="small">It holds the version, the server configuration with every secret removed, this status, the recent server log, the dashboard overview, repositories (without credentials), recent tasks, alerts, certificates (public data) and settings. Database passwords, S3 keys and tokens are never included.</p>
        <SwitchInput v-model="repHosts" label="Include host names (otherwise replaced by host-1, host-2, …)" />
        <div class="form-actions">
          <button type="button" class="btn btn-primary" :disabled="repBusy" data-testid="support-download" @click="downloadReport"><Spinner v-if="repBusy" :size="13" /><Icon v-else name="download" />Download</button>
          <button type="button" class="btn" @click="repOpen = false">Cancel</button>
        </div>
      </div>
    </Modal>
  </div>
</template>

<style scoped>
.page { display: flex; flex-direction: column; gap: var(--s-4); }
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 170px), 1fr)); gap: var(--s-3); }
.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 420px), 1fr)); gap: var(--s-4); align-items: start; }
.charts { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 420px), 1fr)); gap: var(--s-4); }
.seg { margin-left: auto; display: flex; gap: 4px; }
.seg .on { background: var(--accent-soft); border-color: var(--accent); color: var(--accent-text); }
.disks { display: flex; flex-direction: column; gap: 14px; }
.disk { display: flex; flex-direction: column; gap: 4px; min-width: 0; }
.path { font-size: 12px; color: var(--text-muted); overflow-wrap: anywhere; }
.ls { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 8px; }
.ls li { display: grid; grid-template-columns: 70px auto 1fr; gap: 10px; align-items: baseline; }
.mig ul { margin: 6px 0 0; padding-left: 18px; max-height: 200px; overflow: auto; }
.muted { color: var(--text-muted); }
.small { font-size: var(--fs-sm); }
.err { color: var(--critical-text); }
@media (max-width: 600px) { .ls li { grid-template-columns: 1fr; gap: 2px; } }
</style>
