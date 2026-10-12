<script setup lang="ts">
// Traffic control (docs/traffic.md): bandwidth rules of the agent API and the live throughput of every
// host. Rules are enforced by the server on chunk and index bodies; a host's transfer waits for every
// rule that matches it (the strictest wins).
import { computed, ref, watch } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import ByteSize from '@/components/ByteSize.vue'
import EmptyState from '@/components/EmptyState.vue'
import Modal from '@/components/Modal.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import StatTile from '@/components/StatTile.vue'
import Sparkline from '@/components/charts/Sparkline.vue'
import FormField from '@/components/form/FormField.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import { useResource } from '@/api/cache'
import { gap, parseRate, rateText, scheduleError, type HostRate, type TrafficRule, type TrafficRuleInput } from '@/api/sections/gapcore'
import { hasRole } from '@/stores/session'
import { toast, errorText } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import type { ColumnDef } from '@/lib/table'

const canEdit = computed(() => hasRole('admin'))
const rules = useResource(() => 'traffic:rules', (s) => gap.traffic.rules(s))
const current = useResource(() => 'traffic:current', (s) => gap.traffic.current(s), { poll: 2000, ttl: 1500 })
const ruleRows = computed(() => rules.data.value ?? [])
const hostRows = computed(() => current.data.value?.hosts ?? [])
const activeNow = computed(() => new Map((current.data.value?.rules ?? []).map((r) => [r.name, r.active])))

// total throughput history for the tiles (one point per poll, last 2 minutes)
const upHist = ref<number[]>([])
const downHist = ref<number[]>([])
watch(() => current.data.value?.at, () => {
  const h = hostRows.value
  upHist.value = [...upHist.value, h.reduce((a, x) => a + x.upload_bps, 0)].slice(-60)
  downHist.value = [...downHist.value, h.reduce((a, x) => a + x.download_bps, 0)].slice(-60)
})
const upNow = computed(() => upHist.value[upHist.value.length - 1] ?? 0)
const downNow = computed(() => downHist.value[downHist.value.length - 1] ?? 0)
const busy = computed(() => hostRows.value.filter((h) => h.upload_bps + h.download_bps > 0).length)

const hostCols: ColumnDef<HostRate>[] = [
  { key: 'host', label: 'Host', required: true, value: (h) => `${h.repository}/${h.host}` },
  { key: 'upload_bps', label: 'Upload now', align: 'right', width: '130px' },
  { key: 'download_bps', label: 'Download now', align: 'right', width: '130px' },
  { key: 'upload_bytes_total', label: 'Uploaded', align: 'right', width: '120px' },
  { key: 'download_bytes_total', label: 'Downloaded', align: 'right', width: '120px' },
  { key: 'last_seen', label: 'Last transfer', width: '130px' },
]
const ruleCols: ColumnDef<TrafficRule>[] = [
  { key: 'name', label: 'Rule', required: true, value: (r) => `${r.name} ${r.comment}` },
  { key: 'state', label: 'State', width: '120px', value: (r) => (!r.enabled ? 'disabled' : activeNow.value.get(r.name) ? 'active' : 'waiting') },
  { key: 'direction', label: 'Direction', width: '100px' },
  { key: 'rate_bps', label: 'Limit', align: 'right', width: '120px' },
  { key: 'scope', label: 'Applies to', value: (r) => `${r.scope} ${r.hosts}` },
  { key: 'schedule', label: 'When', value: (r) => r.schedule || 'always' },
  { key: 'actions', label: '', sortable: false, noExport: true, align: 'right', width: '170px', required: true },
]

// ---- editor
const zone = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
type Form = { orig: string; name: string; enabled: boolean; direction: 'upload' | 'download'; scope: 'host' | 'global'; hosts: string; rate: string; burst: string; schedule: string; timezone: string; comment: string; busy: boolean; error: string }
const ed = ref<Form | null>(null)
function openNew() {
  ed.value = { orig: '', name: '', enabled: true, direction: 'upload', scope: 'host', hosts: '*', rate: '50 MiB/s', burst: '', schedule: 'mon-fri 08:00-18:00', timezone: zone, comment: '', busy: false, error: '' }
}
function openEdit(r: TrafficRule) {
  ed.value = { orig: r.name, name: r.name, enabled: r.enabled, direction: r.direction, scope: r.scope, hosts: r.hosts, rate: rateText(r.rate_bps), burst: r.burst_bytes ? String(r.burst_bytes) : '', schedule: r.schedule, timezone: r.timezone, comment: r.comment, busy: false, error: '' }
}
const rateBps = computed(() => (ed.value ? parseRate(ed.value.rate) : NaN))
const burstBytes = computed(() => (ed.value && ed.value.burst.trim() ? parseRate(ed.value.burst) : 0))
const schedErr = computed(() => (ed.value ? scheduleError(ed.value.schedule) : ''))
const formErr = computed(() => {
  const e = ed.value
  if (!e) return ''
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(e.name)) return 'Name: 1-64 letters, digits, dots, dashes or underscores.'
  if (!Number.isFinite(rateBps.value)) return 'Limit: write a rate like “50 MiB/s”, “800 KiB/s” or “2 MB/s”.'
  if (rateBps.value < 65536) return 'Limit: at least 64 KiB/s, so a large chunk still finishes within the hosts’ 30-minute request timeout.'
  if (!Number.isFinite(burstBytes.value)) return 'Burst: write a size like “4 MiB”, or leave it empty.'
  return schedErr.value
})
const preview = computed(() => {
  const e = ed.value
  if (!e || formErr.value) return ''
  const who = e.scope === 'host' ? `each host matching “${e.hosts || '*'}” gets its own` : `all hosts matching “${e.hosts || '*'}” share one`
  const when = e.schedule.trim() ? `during “${e.schedule.trim()}” (${e.timezone})` : 'at all times'
  return `${e.direction === 'upload' ? 'Backups (uploads)' : 'Restores (downloads)'}: ${who} ${rateText(rateBps.value)}, ${when}.`
})
async function save() {
  const e = ed.value
  if (!e || formErr.value) return
  e.busy = true
  e.error = ''
  const body: TrafficRuleInput = { name: e.name.trim(), enabled: e.enabled, direction: e.direction, scope: e.scope, hosts: e.hosts.trim() || '*', rate_bps: rateBps.value, burst_bytes: burstBytes.value, schedule: e.schedule.trim(), timezone: e.timezone.trim() || 'UTC', comment: e.comment.trim() }
  try {
    if (e.orig) await gap.traffic.update(e.orig, body)
    else await gap.traffic.create(body)
    toast.success(e.orig ? 'Rule saved' : 'Rule added', 'Hosts get the new limit with their next transfer.')
    ed.value = null
    void rules.refresh()
    void current.refresh()
  } catch (x) { e.error = errorText(x) } finally { if (ed.value) ed.value.busy = false }
}
async function toggle(r: TrafficRule) {
  try {
    await gap.traffic.update(r.name, { ...r, enabled: !r.enabled })
    toast.success(r.enabled ? 'Rule disabled' : 'Rule enabled')
    void rules.refresh()
  } catch (e) { toast.error(e, 'Could not change the rule') }
}
async function remove(r: TrafficRule) {
  if (!(await confirm({ title: `Delete the rule “${r.name}”?`, message: 'Hosts it limited transfer at full speed from their next request.', confirmLabel: 'Delete rule', danger: true, typeToConfirm: r.name }))) return
  try { await gap.traffic.remove(r.name); toast.success('Rule deleted'); void rules.refresh() } catch (e) { toast.error(e, 'Could not delete the rule') }
}
const examples = ['mon-fri 08:00-18:00', '22:00-06:00', 'sat,sun', 'mon-fri 08:00-12:00; mon-fri 13:00-18:00']
</script>

<template>
  <div>
    <header class="page-head">
      <div>
        <h1>Traffic control</h1>
        <p class="lede">Limit how fast hosts upload backups and download restores, per host or shared, at all times or only in office hours.</p>
      </div>
      <div class="actions"><button v-if="canEdit" type="button" class="btn btn-primary" data-testid="traffic-new" @click="openNew"><Icon name="plus" :size="14" />Add a rule</button></div>
    </header>

    <div class="tiles">
      <StatTile label="Upload now" :value="rateText(upNow)" hint="all hosts, 5-second average" :loading="current.loading.value"><Sparkline :values="upHist" label="Upload, last 2 minutes" /></StatTile>
      <StatTile label="Download now" :value="rateText(downNow)" hint="all hosts, 5-second average" :loading="current.loading.value"><Sparkline :values="downHist" label="Download, last 2 minutes" /></StatTile>
      <StatTile label="Hosts transferring" :value="busy" :hint="`${hostRows.length} seen since the server started`" :loading="current.loading.value" />
      <StatTile label="Rules active now" :value="[...activeNow.values()].filter(Boolean).length" :hint="`${ruleRows.filter((r) => r.enabled).length} enabled`" :loading="rules.loading.value" />
    </div>

    <section class="panel block">
      <div class="panel-head"><h2>Rules</h2><span class="muted small">A transfer waits for every rule that matches it: the strictest wins.</span></div>
      <DataTable :rows="ruleRows" :columns="ruleCols" row-key="id" :loading="rules.loading.value" :error="rules.error.value" :retry="rules.refresh" storage-key="traffic-rules" csv-name="traffic-rules" data-testid="traffic-rules">
        <template #cell-name="{ row }"><div class="cx"><span class="t mono">{{ row.name }}</span><span v-if="row.comment" class="d">{{ row.comment }}</span></div></template>
        <template #cell-state="{ row }"><StatusPill :status="!row.enabled ? 'neutral' : activeNow.get(row.name) ? 'ok' : 'info'" :label="!row.enabled ? 'disabled' : activeNow.get(row.name) ? 'active now' : 'outside window'" size="sm" /></template>
        <template #cell-direction="{ row }"><span class="dir"><Icon :name="row.direction === 'upload' ? 'arrow-up' : 'arrow-down'" :size="13" />{{ row.direction }}</span></template>
        <template #cell-rate_bps="{ row }"><span class="num">{{ rateText(row.rate_bps) }}</span></template>
        <template #cell-scope="{ row }"><span>{{ row.scope === 'host' ? 'each host' : 'shared by' }}</span> <code>{{ row.hosts }}</code></template>
        <template #cell-schedule="{ row }"><span v-if="row.schedule">{{ row.schedule }} <span class="muted small">{{ row.timezone }}</span></span><span v-else class="muted">always</span></template>
        <template #cell-actions="{ row }">
          <span v-if="canEdit" class="acts">
            <button type="button" class="btn btn-sm btn-ghost" :data-testid="`traffic-edit-${row.name}`" @click="openEdit(row)"><Icon name="pencil" :size="14" />Edit</button>
            <button type="button" class="btn btn-sm btn-ghost" @click="toggle(row)">{{ row.enabled ? 'Disable' : 'Enable' }}</button>
            <button type="button" class="btn btn-sm btn-ghost btn-icon" :aria-label="`Delete ${row.name}`" @click="remove(row)"><Icon name="trash-2" :size="14" /></button>
          </span>
        </template>
        <template #empty>
          <EmptyState title="No limits" icon="gauge">Hosts upload and download at full speed. Add a rule to keep backups from saturating a link during working hours.
            <template v-if="canEdit" #actions><button type="button" class="btn btn-primary btn-sm" @click="openNew">Add a rule</button></template>
          </EmptyState>
        </template>
      </DataTable>
    </section>

    <section class="panel block">
      <div class="panel-head"><h2>Live throughput</h2><span class="muted small">Refreshes every 2 seconds. Counters start when the server starts.</span></div>
      <DataTable :rows="hostRows" :columns="hostCols" :row-key="(h: HostRate) => `${h.repository}/${h.host}`" :loading="current.loading.value" :error="current.error.value" :retry="current.refresh" storage-key="traffic-hosts" csv-name="traffic" data-testid="traffic-hosts">
        <template #cell-host="{ row }"><span class="mono">{{ row.host }}</span> <span class="muted small">{{ row.repository }}</span></template>
        <template #cell-upload_bps="{ row }"><span class="num" :class="{ quiet: !row.upload_bps }">{{ rateText(row.upload_bps) }}</span></template>
        <template #cell-download_bps="{ row }"><span class="num" :class="{ quiet: !row.download_bps }">{{ rateText(row.download_bps) }}</span></template>
        <template #cell-upload_bytes_total="{ row }"><ByteSize :bytes="row.upload_bytes_total" /></template>
        <template #cell-download_bytes_total="{ row }"><ByteSize :bytes="row.download_bytes_total" /></template>
        <template #cell-last_seen="{ row }"><RelativeTime :time="row.last_seen" /></template>
        <template #empty><EmptyState title="No transfers yet" icon="activity">Hosts appear here with their first chunk upload or download since the server started.</EmptyState></template>
      </DataTable>
    </section>

    <Modal :open="!!ed" :title="ed?.orig ? `Edit ${ed.orig}` : 'Add a traffic rule'" width="620px" @update:open="(v: boolean) => { if (!v) ed = null }">
      <form v-if="ed" id="traffic-form" class="form" data-testid="traffic-form" @submit.prevent="save">
        <div class="cols">
          <FormField v-slot="f" label="Name"><input :id="f.id" v-model="ed.name" class="input mono" autocomplete="off" spellcheck="false" data-testid="traffic-name" /></FormField>
          <FormField v-slot="f" label="Direction"><SelectInput :id="f.id" v-model="ed.direction" :options="[{ value: 'upload', label: 'Upload (backups)' }, { value: 'download', label: 'Download (restores)' }]" data-testid="traffic-direction" /></FormField>
        </div>
        <div class="cols">
          <FormField v-slot="f" label="Limit" hint="e.g. 50 MiB/s, 800 KiB/s, 2 MB/s"><input :id="f.id" v-model="ed.rate" class="input" data-testid="traffic-rate" /></FormField>
          <FormField v-slot="f" label="Applies"><SelectInput :id="f.id" v-model="ed.scope" :options="[{ value: 'host', label: 'To each host separately' }, { value: 'global', label: 'Shared by all matching hosts' }]" data-testid="traffic-scope" /></FormField>
        </div>
        <FormField v-slot="f" label="Hosts" hint="Comma-separated globs on the host name, e.g. web-*, db-1; use repository/host to name one repository's host."><input :id="f.id" v-model="ed.hosts" class="input mono" data-testid="traffic-hosts-input" /></FormField>
        <div class="cols">
          <FormField v-slot="f" label="When" optional :hint="'Empty = always. Examples: ' + examples.join(' · ')"><input :id="f.id" v-model="ed.schedule" class="input mono" placeholder="always" data-testid="traffic-schedule" /></FormField>
          <FormField v-slot="f" label="Time zone"><input :id="f.id" v-model="ed.timezone" class="input" list="tz-list" /></FormField>
        </div>
        <datalist id="tz-list"><option :value="zone" /><option value="UTC" /></datalist>
        <div class="cols">
          <FormField v-slot="f" label="Burst" optional hint="Bucket size; empty = one second of the limit"><input :id="f.id" v-model="ed.burst" class="input" placeholder="one second" /></FormField>
          <FormField v-slot="f" label="Comment" optional><input :id="f.id" v-model="ed.comment" class="input" /></FormField>
        </div>
        <SwitchInput v-model="ed.enabled" label="Enabled" />
        <Callout v-if="formErr" status="warn">{{ formErr }}</Callout>
        <Callout v-else status="info" data-testid="traffic-preview">{{ preview }}</Callout>
        <Callout v-if="ed.error" status="critical" title="Could not save the rule">{{ ed.error }}</Callout>
      </form>
      <template #footer><button type="button" class="btn btn-ghost" @click="ed = null">Cancel</button><button type="submit" form="traffic-form" class="btn btn-primary" :disabled="!!formErr || ed?.busy" data-testid="traffic-save"><Spinner v-if="ed?.busy" :size="13" />{{ ed?.orig ? 'Save rule' : 'Add rule' }}</button></template>
    </Modal>
  </div>
</template>

<style scoped>
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 12px; margin-bottom: 16px; }
.block { margin-bottom: 16px; }
.panel-head { display: flex; align-items: baseline; gap: 12px; flex-wrap: wrap; }
.cx { display: grid; }
.cx .d { color: var(--text-muted); font-size: var(--fs-sm); }
.dir { display: inline-flex; gap: 4px; align-items: center; }
.acts { display: inline-flex; gap: 4px; }
.quiet { color: var(--text-faint); }
.muted { color: var(--text-muted); }
.small { font-size: var(--fs-sm); }
.form { display: grid; gap: 12px; }
.cols { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
@media (max-width: 640px) { .cols { grid-template-columns: 1fr; } }
</style>
