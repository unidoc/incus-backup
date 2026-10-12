<script setup lang="ts">
// One storage object: is it backed up on schedule (calendar of the last 90 days), how its size
// develops, which snapshots exist, what retention would keep or remove, and the operator controls
// (notes, retention policy, transfer to another host, back up now, restore test).
import { computed, ref, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import ObjectDeleteDialog from '@/components/ops/ObjectDeleteDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import DataTable from '@/components/DataTable.vue'
import StatTile from '@/components/StatTile.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import ByteSize from '@/components/ByteSize.vue'
import Callout from '@/components/Callout.vue'
import EmptyState from '@/components/EmptyState.vue'
import Skeleton from '@/components/Skeleton.vue'
import Modal from '@/components/Modal.vue'
import FormField from '@/components/form/FormField.vue'
import CopyField from '@/components/CopyField.vue'
import AreaChart from '@/components/charts/AreaChart.vue'
import KindIcon from '@/components/infra/KindIcon.vue'
import FreshnessBadge from '@/components/infra/FreshnessBadge.vue'
import CalendarHeatmap from '@/components/infra/CalendarHeatmap.vue'
import RowMenu, { type MenuItem } from '@/components/infra/RowMenu.vue'
import BackupNowDialog from '@/components/infra/BackupNowDialog.vue'
import RestoreTestDialog from '@/components/infra/RestoreTestDialog.vue'
import { api, list } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import { infraApi } from '@/api/sections/infra'
import { confirm } from '@/composables/useConfirm'
import { toast } from '@/stores/toast'
import { hasRole } from '@/stores/session'
import { repos } from '@/stores/repos'
import { dailyCounts, kindInfo, planFor } from '@/lib/infra'
import { describeSchedule, formatBytes, formatRelative, plural } from '@/lib/format'
import type { ColumnDef } from '@/lib/table'
import type { PrunePlanEntry, PruneResult, SnapshotRef } from '@/api/types'

const props = defineProps<{ group: string }>()
const repo = computed(() => repos.current)
const canOperate = computed(() => hasRole('operator'))
const isAdmin = computed(() => hasRole('admin'))
const canRestore = computed(() => hasRole('restore'))

const stats = useResource(() => `objects-stats:${repo.value}:`, (s) => infraApi.objectStats(undefined, s), { enabled: () => !!repo.value, refreshOn: ['run'] })
const snaps = useResource(() => `snapshots:${repo.value}:${props.group}`, (s) => api.snapshots.list(props.group, s), { enabled: () => !!repo.value, refreshOn: ['run'] })
const policies = useResource(() => `policies:${repo.value}`, (s) => api.policies.list(s), { enabled: () => !!repo.value })
const hostsRes = useResource(() => `hosts:${repo.value}`, (s) => api.hosts.list(s), { enabled: () => !!repo.value && isAdmin.value })

const obj = computed(() => stats.data.value?.find((o) => o.group_id === props.group) ?? null)
const k = computed(() => (obj.value ? kindInfo(obj.value.kind, obj.value.name) : null))
const snapList = computed(() => list(snaps.data.value).slice().sort((a, b) => Date.parse(b.created) - Date.parse(a.created)))
const isRecord = computed(() => obj.value?.kind === 'instance')
const hostState = computed(() => hostsRes.data.value ? list(hostsRes.data.value).find((h) => h.name === obj.value?.host && h.scope === 'backup') : undefined)

// ---- charts
const days = computed(() => dailyCounts(snapList.value.map((s) => s.created), 91, new Date(), (i) => snapList.value[i]?.verify_state === 'failed'))
const asc = computed(() => snapList.value.slice().reverse())
const sizeX = computed(() => asc.value.map((s) => Date.parse(s.created)))
const sizeSeries = computed(() => [{ name: 'Logical size', values: asc.value.map((s) => s.logical_bytes ?? null) }])
const csvName = computed(() => `${(obj.value?.name ?? 'object').split('/').join('_')}-snapshots`)
const totalLogical = computed(() => snapList.value.reduce((a, s) => a + (s.logical_bytes ?? 0), 0))

// ---- policy
const effPolicyName = computed(() => obj.value?.retention_policy || obj.value?.declared_policy || '')
const effPolicy = computed(() => list(policies.data.value).find((p) => p.name === effPolicyName.value) ?? null)
const keepText = computed(() => {
  const p = effPolicy.value
  if (!p) return ''
  const parts = [['last', p.keep_last], ['hourly', p.keep_hourly], ['daily', p.keep_daily], ['weekly', p.keep_weekly], ['monthly', p.keep_monthly], ['yearly', p.keep_yearly]]
    .filter(([, n]) => (n as number) > 0).map(([l, n]) => `${n} ${l}`)
  return parts.length ? `Keeps ${parts.join(', ')}` : 'Keeps everything (no keep rule)'
})

// ---- snapshots table
const snapCols: ColumnDef<SnapshotRef>[] = [
  { key: 'created', label: 'Created', required: true },
  { key: 'snapshot_id', label: 'Snapshot', mono: true },
  { key: 'logical_bytes', label: 'Size', align: 'right', value: (s) => s.logical_bytes ?? null },
  { key: 'chunk_count', label: 'Chunks', align: 'right', hidden: true },
  { key: 'verify_state', label: 'Verified', value: (s) => s.verify_state ?? '' },
  { key: 'protected', label: 'Protected', value: (s) => (s.protected ? 'yes' : '') },
  { key: 'policy', label: 'Policy', hidden: true },
  { key: 'notes', label: 'Notes' },
]

// ---- notes
const notes = ref('')
const notesDirty = computed(() => notes.value !== (obj.value?.notes ?? ''))
watch(() => obj.value?.notes, (n) => { notes.value = n ?? '' }, { immediate: true })
const notesBusy = ref(false)
async function saveNotes() {
  notesBusy.value = true
  try {
    await api.objects.setNotes(props.group, { notes: notes.value })
    toast.success('Notes saved')
    invalidate('objects-stats'); await stats.refresh()
  } catch (e) { toast.error(e, 'Could not save the notes') } finally { notesBusy.value = false }
}

// ---- retention change
const retOpen = ref(false)
const retPolicy = ref('')
const retBusy = ref(false)
function openRetention() { retPolicy.value = effPolicyName.value; retOpen.value = true }
async function applyRetention() {
  const o = obj.value
  if (!o || !retPolicy.value) return
  const p = list(policies.data.value).find((x) => x.name === retPolicy.value)
  if (!(await confirm({
    title: `Govern ${o.name} by ${retPolicy.value}?`, typeToConfirm: o.name, confirmLabel: 'Change retention',
    message: `The next retention run keeps and removes snapshots of this object by ${retPolicy.value}${p ? ` (keep last ${p.keep_last}, daily ${p.keep_daily}, weekly ${p.keep_weekly}, monthly ${p.keep_monthly}, yearly ${p.keep_yearly})` : ''}. A stricter policy removes snapshots for good at that run. Preview the effect first if you are unsure.`,
  }))) return
  retBusy.value = true
  try {
    await api.objects.setRetention(props.group, { policy: retPolicy.value })
    toast.success(`Retention of ${o.name} set to ${retPolicy.value}`)
    retOpen.value = false
    plan.value = null
    invalidate('objects-stats'); await stats.refresh()
  } catch (e) { toast.error(e, 'Could not change the retention policy') } finally { retBusy.value = false }
}

// ---- retention preview (admin: POST /prune?dry_run=1 plans the whole repository; filtered here)
const plan = ref<PruneResult | null>(null)
const planBusy = ref(false)
const planErr = ref('')
const planned = computed(() => planFor<PrunePlanEntry>(plan.value?.plan, props.group))
async function preview() {
  planBusy.value = true
  planErr.value = ''
  try { plan.value = await api.maintenance.prune(true) } catch (e) { planErr.value = e instanceof Error ? e.message : String(e) } finally { planBusy.value = false }
}

// ---- transfer (admin)
const trOpen = ref(false)
const trHost = ref('')
const trBusy = ref(false)
const trTargets = computed(() => list(hostsRes.data.value).filter((h) => h.scope === 'backup' && h.name !== obj.value?.host))
function openTransfer() { trHost.value = trTargets.value[0]?.name ?? ''; trOpen.value = true }
async function transfer() {
  const o = obj.value
  if (!o || !trHost.value) return
  if (!(await confirm({
    title: `Transfer ${o.name} to ${trHost.value}?`, typeToConfirm: o.name, confirmLabel: 'Transfer object',
    message: `${trHost.value} becomes the only host that may write new snapshots of this object; ${o.host || 'the current host'} can no longer back it up. Existing snapshots stay. Use this when an instance moved to another host.`,
  }))) return
  trBusy.value = true
  try {
    await api.objects.transfer(props.group, { host: trHost.value })
    toast.success(`${o.name} now belongs to ${trHost.value}`)
    trOpen.value = false
    invalidate('objects-stats'); invalidate('infra-hosts'); invalidate('overview'); await stats.refresh()
  } catch (e) { toast.error(e, 'Could not transfer the object') } finally { trBusy.value = false }
}

// ---- jobs
const bnOpen = ref(false)
const rtOpen = ref(false)
const menu = computed<MenuItem[]>(() => [
  { label: 'Change retention policy', icon: 'calendar-clock', run: openRetention, disabled: canOperate.value ? false : 'Needs the operator role', testid: 'menu-retention' },
  { label: 'Transfer to another host', icon: 'server', run: openTransfer, disabled: isAdmin.value ? false : 'Needs the admin role', testid: 'menu-transfer' },
  { label: 'Delete object…', icon: 'trash-2', danger: true, run: () => { delOpen.value = true }, disabled: isAdmin.value ? false : 'Needs the admin role', testid: 'menu-delete' },
])
const delOpen = ref(false)
const router = useRouter()
const opReason = computed(() => (!canOperate.value ? 'Needs the operator role' : !obj.value?.host ? 'The object has no host' : ''))
</script>

<template>
  <div class="obj-page">
    <template v-if="stats.loading.value && !stats.data.value"><Skeleton :lines="4" height="22px" /></template>
    <Callout v-else-if="stats.error.value && !stats.data.value" status="critical" title="Could not load the object" :retry="stats.refresh">{{ stats.error.value.message }}</Callout>
    <template v-else-if="!obj">
      <header class="page-head"><div><h1>Object</h1></div></header>
      <div class="panel">
        <EmptyState title="No such object in this repository" icon="box">The object {{ group }} does not exist in {{ repo }}. It may belong to another repository (switch in the top bar).
          <template #actions><RouterLink :to="{ name: 'objects' }" class="btn btn-sm">All objects</RouterLink></template>
        </EmptyState>
      </div>
    </template>

    <template v-else>
      <header class="page-head">
        <div class="title">
          <h1><KindIcon :kind="obj.kind" :name="obj.name" /><span class="nm">{{ obj.name }}</span><FreshnessBadge :freshness="obj.freshness" :due="obj.due" :overdue-at="obj.overdue_at" :schedule="obj.schedule" size="md" /></h1>
          <p class="lede small">
            {{ k?.label }} · project <b>{{ obj.project || 'default' }}</b> · host
            <RouterLink v-if="obj.host" :to="{ name: 'host', params: { name: obj.host } }">{{ obj.host }}</RouterLink><span v-else>none</span>
            · first seen <RelativeTime :time="obj.first_seen" />
          </p>
        </div>
        <div class="actions">
          <RouterLink v-if="canRestore && obj.latest" :to="{ name: 'restore', query: { group: obj.group_id } }" class="btn"><Icon name="archive-restore" :size="14" />Restore</RouterLink>
          <button v-if="!isRecord" type="button" class="btn" :disabled="!!opReason || !obj.snapshots" :title="opReason || (!obj.snapshots ? 'No snapshot to test yet' : undefined)" data-testid="obj-restore-test" @click="rtOpen = true"><Icon name="shield-check" :size="14" />Run restore test</button>
          <button type="button" class="btn btn-primary" :disabled="!!opReason" :title="opReason || undefined" data-testid="obj-backup-now" @click="bnOpen = true"><Icon name="play" :size="14" />Back up now</button>
          <RowMenu :items="menu" label="More object actions" button="More" />
        </div>
      </header>

      <Callout v-if="obj.freshness === 'stale'" status="warn" title="This object is overdue">The newest snapshot is from <RelativeTime :time="obj.latest?.created" />; by its schedule ({{ describeSchedule(obj.schedule) }}) one was due <RelativeTime :time="obj.due" />. Check the host's last runs.</Callout>
      <Callout v-else-if="obj.freshness === 'never'" status="critical" title="Never backed up">The host declared this object but no snapshot exists. Its policy may exclude it, or every attempt failed.</Callout>
      <Callout v-if="isRecord" status="info" title="Instance record">The instance configuration is stored with each backup of the instance; its disks are separate objects with their own freshness.</Callout>

      <div class="tiles">
        <StatTile label="Snapshots" :value="obj.snapshots" :hint="obj.protected ? `${obj.protected} protected from pruning` : 'none protected'" />
        <StatTile label="Newest snapshot" :value="obj.latest ? formatRelative(obj.latest.created) : 'none'" :hint="obj.latest ? (isRecord ? 'instance configuration' : `${formatBytes(obj.latest.logical_bytes)} logical`) : 'no snapshot yet'" />
        <StatTile label="Next due" :value="obj.due ? formatRelative(obj.due) : '–'" :status="obj.freshness === 'stale' ? 'warn' : undefined" :hint="obj.schedule ? describeSchedule(obj.schedule) : 'no schedule known'" />
        <StatTile label="Policy" :value="effPolicyName || '–'" :hint="keepText || (effPolicyName ? 'unknown policy' : 'no policy')" />
        <StatTile label="Verification" :value="obj.failed_verify ? `${obj.failed_verify} failed` : 'ok'" :status="obj.failed_verify ? 'critical' : undefined" :hint="obj.latest?.verify_state ? `newest: ${obj.latest.verify_state}` : 'newest not verified yet'" />
      </div>

      <div class="grid">
        <div class="col">
          <section class="panel" data-testid="obj-calendar">
            <div class="panel-head"><h2>Snapshots per day, 90 days</h2></div>
            <div class="panel-body"><CalendarHeatmap :days="days" /></div>
          </section>
          <section class="panel" data-testid="obj-size">
            <div class="panel-head"><h2>Size history</h2><span class="sub">logical size of each snapshot, before deduplication</span></div>
            <div class="panel-body">
              <AreaChart v-if="sizeX.length > 1" :x="sizeX" :series="sizeSeries" :y-format="(v) => formatBytes(v)" bytes :height="180" :hours="sizeX.length > 1 && sizeX[sizeX.length - 1] - sizeX[0] < 3 * 86400_000" />
              <EmptyState v-else title="Not enough snapshots for a chart" icon="history" compact>The chart appears with the second snapshot.</EmptyState>
            </div>
          </section>
          <section>
            <div class="sec-head"><h2>Snapshots</h2><span class="muted small">{{ plural(snapList.length, 'snapshot') }} · {{ formatBytes(totalLogical) }} logical in total</span></div>
            <DataTable :rows="snaps.data.value === undefined ? undefined : snapList" :columns="snapCols" row-key="snapshot_id" :loading="snaps.loading.value" :error="snaps.error.value" :retry="snaps.refresh" storage-key="object-snapshots" :csv-name="csvName" dense data-testid="obj-snapshots">
              <template #cell-created="{ row }"><AbsoluteTime :time="row.created" /></template>
              <template #cell-snapshot_id="{ row }"><RouterLink :to="{ name: 'snapshot', params: { group: row.group_id, snap: row.snapshot_id } }" class="mono nowrap">{{ row.snapshot_id }}</RouterLink></template>
              <template #cell-logical_bytes="{ row }"><ByteSize :bytes="row.logical_bytes" /></template>
              <template #cell-verify_state="{ row }"><StatusPill v-if="row.verify_state" :state="row.verify_state" size="sm" variant="dot" /><span v-else class="muted small">not yet</span></template>
              <template #cell-protected="{ row }"><span v-if="row.protected" class="prot"><Icon name="lock" :size="12" />protected</span></template>
              <template #cell-notes="{ row }"><span class="truncate nt" :title="row.notes">{{ row.notes || '' }}</span></template>
              <template #empty><EmptyState title="No snapshot yet" icon="history">The first one appears after the host's next run.</EmptyState></template>
            </DataTable>
          </section>
        </div>

        <div class="col side">
          <section class="panel" data-testid="obj-retention">
            <div class="panel-head"><h2>Retention</h2><button v-if="canOperate" type="button" class="btn btn-sm btn-ghost ml" @click="openRetention">Change</button></div>
            <div class="panel-body stack">
              <div>
                <p><b>{{ effPolicyName || 'No policy' }}</b><span v-if="obj.retention_policy && obj.retention_policy !== obj.declared_policy" class="muted small"> (set by an operator; the host declares {{ obj.declared_policy || 'none' }})</span></p>
                <p class="muted small">{{ keepText }}<template v-if="effPolicy?.timezone">, periods cut in {{ effPolicy.timezone }}</template>.</p>
              </div>
              <div v-if="isAdmin">
                <button type="button" class="btn btn-sm" :disabled="planBusy" data-testid="obj-preview" @click="preview"><Icon :name="planBusy ? 'loader-circle' : 'eye'" :size="13" />{{ plan ? 'Preview again' : 'Preview retention' }}</button>
                <p class="muted small hint">Plans the whole repository exactly as the next retention run would, and shows this object's part. Nothing is removed.</p>
              </div>
              <p v-else class="muted small">The retention preview needs the admin role: it plans the whole repository.</p>
              <Callout v-if="planErr" status="critical" title="Could not compute the preview">{{ planErr }}</Callout>
              <div v-if="plan" class="plan" data-testid="obj-plan">
                <p class="small"><b class="num">{{ planned.keep }}</b> kept · <b class="num" :class="{ critv: planned.remove }">{{ planned.remove }}</b> removed<template v-if="planned.protectedCount"> · {{ planned.protectedCount }} protected</template></p>
                <ul v-if="planned.entries.length" class="pl">
                  <li v-for="e in planned.entries" :key="e.snapshot" :class="e.keep ? 'k' : 'r'">
                    <Icon :name="e.keep ? (e.protected ? 'lock' : 'check') : 'trash-2'" :size="12" />
                    <RouterLink :to="{ name: 'snapshot', params: { group: e.group, snap: e.snapshot } }" class="mono">{{ e.snapshot }}</RouterLink>
                    <span class="muted small">{{ e.keep ? e.reason || 'kept' : 'removed' }}</span>
                  </li>
                </ul>
                <p v-else class="muted small">The plan has no snapshot of this object.</p>
              </div>
            </div>
          </section>

          <section class="panel" data-testid="obj-notes">
            <div class="panel-head"><h2>Notes</h2></div>
            <div class="panel-body stack">
              <textarea v-model="notes" class="textarea" rows="4" :disabled="!canOperate" :placeholder="canOperate ? 'What this object is, who owns it, how to restore it' : 'No notes'" aria-label="Notes" maxlength="4000" data-testid="obj-notes-input" />
              <div v-if="canOperate" class="form-actions"><button type="button" class="btn btn-sm btn-primary" :disabled="!notesDirty || notesBusy" data-testid="obj-notes-save" @click="saveNotes">Save notes</button><button v-if="notesDirty" type="button" class="btn btn-sm btn-ghost" @click="notes = obj.notes">Discard</button></div>
            </div>
          </section>

          <section class="panel">
            <div class="panel-head"><h2>Identity</h2></div>
            <div class="panel-body stack">
              <CopyField :value="obj.group_id" label="Group id" />
              <p class="muted small">The group id names the object in the repository and in the API ({{ obj.project }}/{{ obj.name }}).</p>
              <div v-if="isAdmin" class="row"><button type="button" class="btn btn-sm" data-testid="obj-transfer" @click="openTransfer"><Icon name="server" :size="13" />Transfer to another host</button></div>
            </div>
          </section>
        </div>
      </div>

      <Modal v-model:open="retOpen" title="Change the retention policy" :description="`Which policy decides how long snapshots of ${obj.name} are kept.`">
        <div class="form-grid">
          <FormField label="Policy" v-slot="f">
            <select :id="f.id" v-model="retPolicy" class="select" data-testid="ret-select"><option v-for="p in list(policies.data.value)" :key="p.name" :value="p.name">{{ p.name }}: keep {{ p.keep_last }} last, {{ p.keep_daily }} daily, {{ p.keep_weekly }} weekly, {{ p.keep_monthly }} monthly</option></select>
          </FormField>
          <p class="muted small">This overrides what the host declares for retention only; the backup schedule stays the host's.</p>
        </div>
        <template #footer><button type="button" class="btn" @click="retOpen = false">Cancel</button><button type="button" class="btn btn-danger" :disabled="retBusy || !retPolicy || retPolicy === effPolicyName" data-testid="ret-apply" @click="applyRetention">Change retention</button></template>
      </Modal>

      <Modal v-model:open="trOpen" title="Transfer to another host" :description="`Re-pin ${obj.name} to another backup host of ${repo}.`">
        <div class="form-grid">
          <FormField label="New host" v-slot="f">
            <select :id="f.id" v-model="trHost" class="select" :disabled="!trTargets.length" data-testid="tr-select"><option v-for="h in trTargets" :key="h.name" :value="h.name">{{ h.name }}{{ h.disabled ? ' (disabled)' : '' }}</option></select>
          </FormField>
          <Callout v-if="!trTargets.length" status="info" title="No other backup host">Enrol the new host first.</Callout>
        </div>
        <template #footer><button type="button" class="btn" @click="trOpen = false">Cancel</button><button type="button" class="btn btn-danger" :disabled="trBusy || !trHost" data-testid="tr-apply" @click="transfer">Transfer</button></template>
      </Modal>

      <BackupNowDialog v-if="obj.host" v-model:open="bnOpen" :repository="repo" :host="obj.host" :objects="[obj]" :object="obj" :offline="hostState ? !hostState.last_seen || Date.now() - Date.parse(hostState.last_seen) > 6 * 3600_000 : false" />
      <RestoreTestDialog v-if="obj.host && !isRecord" v-model:open="rtOpen" :repository="repo" :host="obj.host" :objects="[obj]" :object="obj" />
      <ObjectDeleteDialog v-model:open="delOpen" :repository="repo" :group="group" :name="obj.name" :snapshots="snapList.length" :protected="snapList.filter((x) => x.protected).length" @deleted="router.push({ name: 'objects' })" />
    </template>
  </div>
</template>

<style scoped>
.obj-page { display: flex; flex-direction: column; gap: 14px; }
.page-head { margin-bottom: 0; }
.title h1 { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.nm { word-break: break-all; }
.tiles { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: 10px; }
.tilev { font-size: var(--fs-lg); font-weight: var(--fw-semibold); color: var(--text); }
.grid { display: grid; grid-template-columns: minmax(0, 1.8fr) minmax(280px, 1fr); gap: 14px; align-items: start; }
.col { display: flex; flex-direction: column; gap: 14px; min-width: 0; }
.sec-head { display: flex; align-items: baseline; gap: 10px; margin-bottom: 8px; flex-wrap: wrap; }
.sec-head h2 { font-size: var(--fs-md); }
.mono { font-family: var(--font-mono); font-size: 12.5px; }
.prot { display: inline-flex; gap: 4px; align-items: center; color: var(--text-2); }
.nt { display: inline-block; max-width: 220px; vertical-align: middle; }
.ml { margin-left: auto; }
.hint { margin-top: 6px; }
.critv { color: var(--critical-text); }
.pl { list-style: none; margin: 8px 0 0; padding: 0; max-height: 320px; overflow: auto; border: 1px solid var(--border); border-radius: var(--r); }
.pl li { display: flex; align-items: center; gap: 8px; padding: 5px 10px; border-bottom: 1px solid var(--border); }
.pl li:last-child { border-bottom: 0; }
.pl li.k svg { color: var(--ok); }
.pl li.r { background: var(--critical-bg); }
.pl li.r svg { color: var(--critical); }
@media (max-width: 1200px) { .tiles { grid-template-columns: repeat(3, minmax(0, 1fr)); } }
@media (max-width: 1000px) { .grid { grid-template-columns: 1fr; } }
@media (max-width: 600px) { .tiles { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
</style>
