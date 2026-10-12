<script setup lang="ts">
// Alerts: what needs a human now. Open alerts first (critical, then warning), each with its state,
// age, count and where it is; filters; a detail drawer (timeline, deliveries, what to do);
// acknowledge with a comment (and undo), silence from an alert. Live through the 'alert' events.
import { computed, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import CIcon from '@/components/config/CIcon.vue'
import SubNav from '@/components/config/SubNav.vue'
import DataTable from '@/components/DataTable.vue'
import SeverityBadge from '@/components/SeverityBadge.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import EmptyState from '@/components/EmptyState.vue'
import StatTile from '@/components/StatTile.vue'
import Modal from '@/components/Modal.vue'
import Spinner from '@/components/Spinner.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import AckDialog from '@/components/config/AckDialog.vue'
import SilenceForm from '@/components/config/SilenceForm.vue'
import AlertDetailPanel from '@/components/config/AlertDetailPanel.vue'
import { refreshAlertCounts } from '@/components/config/alertCounts'
import { useResource } from '@/api/cache'
import { cfg, list, canCfg, type Alert, type AlertQuery, type SilenceInput } from '@/api/sections/config'
import { ALERT_TYPES, ALERT_GROUPS, alertType, alertOrder, statusOfAlert, shortAge } from '@/lib/alertTypes'
import { session } from '@/stores/session'
import { toast, push } from '@/stores/toast'
import { useNow } from '@/composables/useNow'
import type { ColumnDef } from '@/lib/table'
import '@/components/config/section.css'

const route = useRoute()
const router = useRouter()
const now = useNow()

type View = 'active' | 'firing' | 'acknowledged' | 'silenced' | 'resolved' | 'all'
const views: { id: View; label: string }[] = [
  { id: 'active', label: 'Open' }, { id: 'firing', label: 'Firing' }, { id: 'acknowledged', label: 'Acknowledged' },
  { id: 'silenced', label: 'Silenced' }, { id: 'resolved', label: 'Resolved' }, { id: 'all', label: 'All' },
]
const q = (k: string) => (typeof route.query[k] === 'string' ? (route.query[k] as string) : '')
const view = ref<View>((views.find((v) => v.id === q('view'))?.id ?? 'active') as View)
const severity = ref(q('severity'))
const type = ref(q('type'))
const host = ref(q('host'))
watch([view, severity, type, host], () => {
  void router.replace({ query: { ...route.query, view: view.value === 'active' ? undefined : view.value, severity: severity.value || undefined, type: type.value || undefined, host: host.value || undefined } })
})

const query = computed<AlertQuery>(() => {
  const x: AlertQuery = { limit: 1000, severity: (severity.value || undefined) as AlertQuery['severity'], type: type.value || undefined, host: host.value || undefined }
  if (view.value === 'active') x.active = 1
  else if (view.value !== 'all') x.state = view.value
  return x
})
const alerts = useResource(() => `cfg:alerts:${JSON.stringify(query.value)}`, (s) => cfg.alerts.list(query.value, s), { refreshOn: ['alert'], ttl: 5000 })
// counts for the strip and the segmented control: always the whole open set
const open = useResource(() => 'cfg:alerts:open', (s) => cfg.alerts.list({ active: 1, limit: 1000 }, s), { refreshOn: ['alert'], ttl: 5000 })
const resolved24 = useResource(() => 'cfg:alerts:resolved', (s) => cfg.alerts.list({ state: 'resolved', limit: 1000 }, s), { refreshOn: ['alert'], ttl: 30_000 })

const rows = computed(() => [...list(alerts.data.value)].sort(alertOrder))
const openRows = computed(() => list(open.data.value))
const counts = computed(() => {
  const c = { critical: 0, warning: 0, info: 0, acknowledged: 0, silenced: 0, firing: 0 }
  for (const a of openRows.value) {
    if (a.state === 'firing') { c.firing++; if (a.severity === 'critical') c.critical++; else if (a.severity === 'warning') c.warning++; else c.info++ }
    if (a.state === 'acknowledged') c.acknowledged++
    if (a.state === 'silenced') c.silenced++
  }
  return c
})
const resolvedToday = computed(() => list(resolved24.data.value).filter((a) => a.resolved_at && now.value - Date.parse(a.resolved_at) < 86400e3).length)
const viewCount = (v: View) => v === 'active' ? openRows.value.length : v === 'firing' ? counts.value.firing : v === 'acknowledged' ? counts.value.acknowledged : v === 'silenced' ? counts.value.silenced : undefined

const typeOptions = computed(() => [{ value: '', label: 'Every type' }, ...ALERT_GROUPS.flatMap((g) => ALERT_TYPES.filter((t) => t.group === g).map((t) => ({ value: t.type, label: `${g}: ${t.label}` })))])
const sevOptions = [{ value: '', label: 'Any severity' }, { value: 'warning', label: 'Warning and critical' }, { value: 'critical', label: 'Critical only' }]

const cols: ColumnDef<Alert>[] = [
  { key: 'severity', label: 'Severity', width: '110px', value: (r) => ({ critical: 2, warning: 1, info: 0 } as Record<string, number>)[r.severity] ?? 0, format: (r) => r.severity },
  { key: 'title', label: 'Alert', required: true, value: (r) => `${r.title} ${r.detail}` },
  { key: 'state', label: 'State', width: '150px' },
  { key: 'where', label: 'Where', value: (r) => [r.host, r.repository, r.object].filter(Boolean).join(' ') },
  { key: 'type', label: 'Type', hidden: true, format: (r) => alertType(r.type).label },
  { key: 'first_seen', label: 'Age', width: '90px', value: (r) => -Date.parse(r.first_seen) },
  { key: 'last_seen', label: 'Last seen', width: '110px', hidden: true },
  { key: 'count', label: 'Count', align: 'right', width: '70px' },
  { key: 'actions', label: '', sortable: false, noExport: true, align: 'right', width: '130px', required: true },
]

const canAck = computed(() => canCfg(session.role, 'POST /alerts/{id}/ack'))
const canSilence = computed(() => canCfg(session.role, 'POST /silences'))
const canTick = computed(() => canCfg(session.role, 'POST /notifications/tick'))

const ackFor = ref<Alert | null>(null)
const silOpen = ref(false)
const silPrefill = ref<Partial<SilenceInput> | null>(null)
const drawer = ref<number | null>(null)
const busyId = ref<number | null>(null)

function refreshAll() { void alerts.refresh(); void open.refresh(); void resolved24.refresh(); refreshAlertCounts(0) }
function afterAck(a: Alert) {
  refreshAll()
  push({ kind: 'success', title: 'Alert acknowledged', message: `${a.title}. Reminders stop until it resolves.`, timeout: 8000, action: { label: 'Undo', run: () => void unack(a) } })
}
async function unack(a: Alert) {
  busyId.value = a.id
  try { await cfg.alerts.unack(a.id); toast.success('Acknowledgement removed', 'Reminders resume.'); refreshAll() } catch (e) { toast.error(e, 'Could not remove the acknowledgement') } finally { busyId.value = null }
}
function silence(a: Alert) {
  silPrefill.value = { type_glob: a.type, host_glob: a.host, repo_glob: a.repository, object_glob: a.object, reason: '' }
  silOpen.value = true
}
/** Plain click opens the drawer; ctrl/cmd/middle click keeps the link (a new tab with the alert page). */
function openDrawer(e: MouseEvent, id: number) {
  if (e.ctrlKey || e.metaKey || e.shiftKey || e.button !== 0) return
  e.preventDefault()
  e.stopPropagation()
  drawer.value = id
}
function newSilence() { silPrefill.value = null; silOpen.value = true }
const ticking = ref(false)
async function tick() {
  ticking.value = true
  try {
    const r = await cfg.notify.tick()
    if (r.skipped) toast.info('Another evaluation is running', 'Its results appear here in a moment.')
    else toast.success('Conditions evaluated', `${r.fired} fired, ${r.resolved} resolved, ${r.delivered} notification${r.delivered === 1 ? '' : 's'} sent${r.failed ? `, ${r.failed} failed` : ''}.`)
    if (r.errors?.length) toast.warn('Some conditions could not be evaluated', r.errors.join('; '))
    refreshAll()
  } catch (e) { toast.error(e, 'Could not evaluate the conditions') } finally { ticking.value = false }
}
const ageOf = (a: Alert) => shortAge((a.resolved_at ? Date.parse(a.resolved_at) : now.value) - Date.parse(a.first_seen))
const emptyTitle = computed(() => view.value === 'active' || view.value === 'firing' ? 'No open alerts' : view.value === 'resolved' ? 'No resolved alerts kept' : `No ${view.value} alerts`)
</script>

<template>
  <div>
    <header class="page-head">
      <div>
        <h1>Alerts</h1>
        <p class="lede">Conditions that need a human. They resolve by themselves when the condition clears; acknowledge what you work on.</p>
      </div>
      <div class="actions">
        <button v-if="canTick" type="button" class="btn" :disabled="ticking" data-testid="tick" title="Evaluate every condition and deliver queued notifications now (the server does this every minute)" @click="tick"><Spinner v-if="ticking" :size="13" /><Icon v-else name="refresh-cw" :size="14" />Evaluate now</button>
        <button v-if="canSilence" type="button" class="btn" data-testid="new-silence" @click="newSilence"><CIcon name="bell-off" :size="14" />New silence</button>
      </div>
    </header>
    <SubNav group="alerts" />

    <div class="cx-tiles">
      <StatTile label="Critical, firing" :value="counts.critical" :status="counts.critical ? 'critical' : 'ok'" :loading="open.loading.value" data-testid="tile-critical" />
      <StatTile label="Warning, firing" :value="counts.warning" :status="counts.warning ? 'warn' : 'ok'" :loading="open.loading.value" />
      <StatTile label="Acknowledged" :value="counts.acknowledged" hint="someone is on it" :loading="open.loading.value" />
      <StatTile label="Silenced" :value="counts.silenced" :to="{ name: 'alert-silences' }" :loading="open.loading.value" />
      <StatTile label="Resolved, 24 h" :value="resolvedToday" :loading="resolved24.loading.value" />
    </div>

    <div class="cx-bar">
      <div class="cx-seg" role="group" aria-label="Alert state" data-testid="alert-views">
        <button v-for="v in views" :key="v.id" type="button" :aria-pressed="view === v.id" @click="view = v.id">{{ v.label }}<span v-if="viewCount(v.id) !== undefined" class="n">{{ viewCount(v.id) }}</span></button>
      </div>
      <SelectInput v-model="severity" :options="sevOptions" size="sm" aria-label="Minimum severity" />
      <SelectInput v-model="type" :options="typeOptions" size="sm" aria-label="Alert type" />
      <input v-model.lazy="host" class="input hostf" placeholder="Host (glob)" aria-label="Host filter, glob" spellcheck="false" />
    </div>

    <DataTable :rows="rows" :columns="cols" :row-key="(r: Alert) => String(r.id)" :loading="alerts.loading.value" :error="alerts.error.value" :retry="alerts.refresh"
      storage-key="cfg-alerts" csv-name="alerts" row-clickable filter-placeholder="Filter: title, host, object" data-testid="alert-table" @row-click="(r: Alert) => (drawer = r.id)">
      <template #cell-severity="{ row }"><SeverityBadge :severity="row.severity" /></template>
      <template #cell-title="{ row }">
        <div class="cx-title"><a :href="`/alerts/${row.id}`" class="t" @click="openDrawer($event, row.id)">{{ row.title }}</a><span v-if="row.detail" class="d" :title="row.detail">{{ row.detail }}</span></div>
      </template>
      <template #cell-state="{ row }">
        <span class="stc"><StatusPill :status="statusOfAlert(row.state, row.severity)" :label="row.state" size="sm" /><span v-if="row.flapping" class="cx-tag warn" title="Flapping: nothing is sent until it settles">flapping</span></span>
        <div v-if="row.state === 'acknowledged'" class="by muted small" :title="row.ack_comment">by {{ row.acked_by }}</div>
      </template>
      <template #cell-where="{ row }">
        <div class="cx-title wh">
          <span class="t"><Icon :name="row.object ? 'box' : row.host ? 'server' : 'database'" :size="12" />{{ row.object || row.host || row.repository || 'server' }}</span>
          <span v-if="row.object || row.host" class="d">{{ [row.object ? row.host : '', row.repository].filter(Boolean).join(' · ') }}</span>
        </div>
      </template>
      <template #cell-first_seen="{ row }"><span class="nowrap" :title="`Since ${new Date(row.first_seen).toLocaleString()}`">{{ ageOf(row) }}</span></template>
      <template #cell-last_seen="{ row }"><RelativeTime :time="row.last_seen" /></template>
      <template #cell-count="{ row }"><span class="num">{{ row.count }}</span></template>
      <template #cell-actions="{ row }">
        <span class="cx-actions">
          <button v-if="row.state === 'firing' || row.state === 'silenced'" type="button" class="btn btn-sm" :disabled="!canAck" :title="canAck ? 'Acknowledge: I am on it' : 'Needs the operator role'" data-testid="row-ack" @click.stop="ackFor = row"><CIcon name="check-check" :size="14" />Ack</button>
          <button v-else-if="row.state === 'acknowledged'" type="button" class="btn btn-sm btn-ghost" :disabled="!canAck || busyId === row.id" title="Undo the acknowledgement" data-testid="row-unack" @click.stop="unack(row)"><CIcon name="undo-2" :size="14" />Undo</button>
          <button v-if="row.state !== 'resolved'" type="button" class="btn btn-sm btn-ghost btn-icon" :disabled="!canSilence" :aria-label="`Silence ${row.title}`" :title="canSilence ? 'Silence similar alerts' : 'Needs the operator role'" data-testid="row-silence" @click.stop="silence(row)"><CIcon name="bell-off" :size="14" /></button>
        </span>
      </template>
      <template #empty>
        <EmptyState :title="emptyTitle" icon="circle-check">
          <template v-if="view === 'active'">Every condition is fine. Conditions are checked every minute; alerts open and resolve by themselves.</template>
          <template v-else>Try another filter.</template>
          <template v-if="canCfg(session.role, 'GET /notifications/rules')" #actions><RouterLink :to="{ name: 'notification-rules' }" class="btn btn-sm">Who is notified</RouterLink></template>
        </EmptyState>
      </template>
    </DataTable>

    <Modal :open="drawer !== null" drawer title="Alert" width="620px" @update:open="(v: boolean) => { if (!v) drawer = null }">
      <AlertDetailPanel v-if="drawer !== null" :id="drawer" @changed="refreshAll" />
    </Modal>
    <AckDialog :alert="ackFor" @close="ackFor = null" @done="afterAck" />
    <SilenceForm v-model:open="silOpen" :prefill="silPrefill" :title="silPrefill ? 'Silence similar alerts' : 'New silence'" @created="refreshAll" />
  </div>
</template>

<style scoped>
.hostf { width: 160px; }
.stc { display: inline-flex; gap: 6px; align-items: center; }
.by { margin-top: 2px; }
.wh .t { display: inline-flex; align-items: center; gap: 5px; font-weight: var(--fw-regular, 400); white-space: nowrap; }
.wh .t .icon { color: var(--text-faint); }
@media (max-width: 640px) { .hostf { width: 100%; } }
</style>
