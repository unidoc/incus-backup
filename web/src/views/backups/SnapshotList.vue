<script setup lang="ts">
// Every snapshot of the selected repository, across all objects. Filters (host, project, object, kind,
// policy, protection, verification, date range) live in the URL so other pages link straight to
// "failed" or "never verified". Bulk protect/unprotect (operator) and bulk forget (admin, typed
// confirmation; protected snapshots are refused and listed with the reason).
import { computed, reactive, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import DataTable from '@/components/DataTable.vue'
import EmptyState from '@/components/EmptyState.vue'
import Callout from '@/components/Callout.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import ByteSize from '@/components/ByteSize.vue'
import DurationText from '@/components/DurationText.vue'
import Tooltip from '@/components/Tooltip.vue'
import StatTile from '@/components/StatTile.vue'
import Modal from '@/components/Modal.vue'
import PIcon from '@/components/protect/PIcon.vue'
import VerifyBadge from '@/components/protect/VerifyBadge.vue'
import { useSnapRows } from '@/components/protect/useSnapRows'
import { api } from '@/api/endpoints'
import { invalidate } from '@/api/cache'
import { repos, repoNone } from '@/stores/repos'
import { hasRole } from '@/stores/session'
import { toast } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import { distinct, filterSnapshots, forgetPlan, kindLabel, verifySummary, type SnapFilter, type SnapRow } from '@/lib/protect/snapshots'
import { formatNumber } from '@/lib/format'
import { noun } from '@/lib/protect/text'
import type { ColumnDef } from '@/lib/table'

const route = useRoute()
const router = useRouter()
const { rows, snaps, statsMissing, refresh } = useSnapRows()

// ---- filters <-> URL
const KEYS = ['host', 'project', 'object', 'kind', 'policy', 'protected', 'verify', 'from', 'to'] as const
const f = reactive<SnapFilter>({})
function fromQuery() {
  for (const k of KEYS) (f as Record<string, string>)[k] = typeof route.query[k] === 'string' ? (route.query[k] as string) : ''
}
fromQuery()
watch(() => route.query, fromQuery)
watch(f, () => {
  const q: Record<string, string> = {}
  for (const k of KEYS) { const v = (f as Record<string, string>)[k]; if (v) q[k] = v }
  if (JSON.stringify(q) !== JSON.stringify(Object.fromEntries(Object.entries(route.query).filter(([k]) => (KEYS as readonly string[]).includes(k))))) void router.replace({ query: q })
}, { deep: true })
const active = computed(() => KEYS.filter((k) => (f as Record<string, string>)[k]).length)
function clearFilters() { for (const k of KEYS) (f as Record<string, string>)[k] = '' }

const all = computed(() => rows.value ?? [])
const shown = computed(() => filterSnapshots(all.value, f))
const opts = computed(() => ({
  host: distinct(all.value, (r) => r.host), project: distinct(all.value, (r) => r.project), kind: distinct(all.value, (r) => r.kind), policy: distinct(all.value, (r) => r.policy),
}))
const sum = computed(() => verifySummary(all.value))
const objects = computed(() => new Set(all.value.map((r) => r.group_id)).size)
const protectedCount = computed(() => all.value.filter((r) => r.protected).length)

const cols: ColumnDef<SnapRow>[] = [
  { key: 'object', label: 'Object', value: (r) => `${r.project}/${r.object}`, required: true },
  { key: 'created', label: 'Created', value: (r) => r.created, width: '120px' },
  { key: 'host', label: 'Host', width: '100px' },
  { key: 'kind', label: 'Kind', format: (r) => kindLabel(r.kind), hidden: true },
  { key: 'policy', label: 'Policy', hidden: true },
  { key: 'logical_bytes', label: 'Size', align: 'right', width: '90px' },
  { key: 'dirty', label: 'Changed', value: (r) => r.stats?.dirty_bytes, align: 'right', width: '90px' },
  { key: 'uploaded', label: 'Uploaded', value: (r) => r.stats?.uploaded_bytes, align: 'right', width: '90px' },
  { key: 'duration', label: 'Duration', value: (r) => r.stats?.duration_ms, align: 'right', width: '80px' },
  { key: 'full', label: 'Read', value: (r) => (r.stats ? (r.stats.full_read ? 'full' : 'incremental') : ''), width: '96px', hidden: false },
  { key: 'verify', label: 'Verification', value: (r) => r.verifyClass, width: '130px' },
  { key: 'flags', label: 'Flags', value: (r) => `${r.protected ? 'protected ' : ''}${r.notes ? 'notes' : ''}`, width: '70px', sortable: false },
  { key: 'snapshot_id', label: 'Snapshot', mono: true, hidden: true },
  { key: 'chunk_count', label: 'Chunks', align: 'right', hidden: true },
]

// ---- selection and bulk actions
const selected = ref<string[]>([])
const selRows = computed(() => { const s = new Set(selected.value); return all.value.filter((r) => s.has(r.key)) })
watch(() => repos.current, () => { selected.value = [] })
const canProtect = computed(() => hasRole('operator'))
const canForget = computed(() => hasRole('admin'))
const busy = ref(false)
const progress = ref({ done: 0, total: 0 })

async function eachSel<T>(items: SnapRow[], fn: (r: SnapRow) => Promise<T>): Promise<{ ok: SnapRow[]; failed: { r: SnapRow; err: string }[] }> {
  busy.value = true
  progress.value = { done: 0, total: items.length }
  const ok: SnapRow[] = []
  const failed: { r: SnapRow; err: string }[] = []
  for (const r of items) {
    try { await fn(r); ok.push(r) } catch (e) { failed.push({ r, err: (e as Error).message }) }
    progress.value.done++
  }
  busy.value = false
  invalidate(`snapshots:${repos.current}`)
  invalidate(`snapshot-stats:${repos.current}`)
  await refresh()
  return { ok, failed }
}

async function bulkProtect(on: boolean) {
  const items = selRows.value.filter((r) => !!r.protected !== on)
  if (!items.length) { toast.info(on ? 'Already protected' : 'None of them is protected'); return }
  const res = await eachSel(items, (r) => api.snapshots.setProtected(r.group_id, r.snapshot_id, { protected: on }))
  if (res.failed.length) toast.error(new Error(res.failed.map((x) => `${x.r.snapshot_id}: ${x.err}`).slice(0, 3).join('; ')), `Could not ${on ? 'protect' : 'unprotect'} ${res.failed.length} ${noun(res.failed.length, 'snapshot')}`)
  if (res.ok.length) toast.success(`${on ? 'Protected' : 'Unprotected'} ${res.ok.length} ${noun(res.ok.length, 'snapshot')}`, on ? 'Retention and forgetting skip them until they are unprotected.' : 'Retention may remove them at its next run.')
}

const forgetOpen = ref(false)
const plan = computed(() => forgetPlan(selRows.value))
async function bulkForget() {
  const p = plan.value
  forgetOpen.value = false
  if (!p.allowed.length) return
  const n = p.allowed.length
  if (!(await confirm({ title: `Forget ${n} ${noun(n, 'snapshot')}?`, message: `They are removed from the repository for good. Their chunks are freed by the next garbage collection unless other snapshots still use them. This cannot be undone.${p.refused.length ? ` ${p.refused.length} protected ${noun(p.refused.length, 'snapshot is', 'snapshots are')} kept.` : ''}`, confirmLabel: `Forget ${n}`, danger: true, typeToConfirm: `forget ${n}` }))) return
  const res = await eachSel(p.allowed, (r) => api.snapshots.forget(r.group_id, r.snapshot_id))
  selected.value = []
  if (res.failed.length) toast.error(new Error(res.failed.map((x) => `${x.r.snapshot_id}: ${x.err}`).slice(0, 3).join('; ')), `Could not forget ${res.failed.length} ${noun(res.failed.length, 'snapshot')}`)
  if (res.ok.length) toast.success(`Forgot ${res.ok.length} ${noun(res.ok.length, 'snapshot')}`, 'Their space is reclaimed by the next garbage collection.')
}
const open = (r: SnapRow) => router.push({ name: 'snapshot', params: { group: r.group_id, snap: r.snapshot_id } })
</script>

<template>
  <div>
    <header class="page-head">
      <div>
        <h1>Snapshots</h1>
        <p class="lede">Every restore point in <strong>{{ repos.current || 'this repository' }}</strong>, newest first. Open one to browse its files, restore it or see its verification history.</p>
      </div>
      <div class="actions">
        <RouterLink class="btn" :to="{ name: 'verify' }"><PIcon name="shield-check" :size="15" />Verification</RouterLink>
        <RouterLink v-if="hasRole('restore')" class="btn btn-primary" :to="{ name: 'restore' }"><PIcon name="archive-restore" :size="15" />Restore…</RouterLink>
      </div>
    </header>

    <EmptyState v-if="repoNone" title="No repository yet" icon="database">Snapshots appear once a repository exists and a host backed up into it.</EmptyState>
    <template v-else>
      <div class="tiles">
        <StatTile label="Snapshots" :value="formatNumber(sum.total)" :hint="`${formatNumber(objects)} ${noun(objects, 'object')}`" :loading="rows === undefined" />
        <StatTile label="Verified" :value="formatNumber(sum.ok)" status="ok" :loading="rows === undefined" :to="{ name: 'snapshots', query: { verify: 'ok' } }" />
        <StatTile label="Stale verification" :value="formatNumber(sum.stale)" :status="sum.stale ? 'warn' : undefined" :loading="rows === undefined" :to="{ name: 'snapshots', query: { verify: 'stale' } }" />
        <StatTile label="Never verified" :value="formatNumber(sum.never)" :status="sum.never ? 'warn' : undefined" :loading="rows === undefined" :to="{ name: 'snapshots', query: { verify: 'never' } }" />
        <StatTile label="Failed verification" :value="formatNumber(sum.failed)" :status="sum.failed ? 'critical' : 'ok'" :loading="rows === undefined" :to="{ name: 'snapshots', query: { verify: 'failed' } }" />
        <StatTile label="Protected" :value="formatNumber(protectedCount)" :loading="rows === undefined" :to="{ name: 'snapshots', query: { protected: 'yes' } }" />
      </div>

      <form class="filters panel" aria-label="Filter snapshots" @submit.prevent>
        <label><span>Host</span><select v-model="f.host" class="select sm"><option value="">Any</option><option v-for="o in opts.host" :key="o" :value="o">{{ o }}</option></select></label>
        <label><span>Project</span><select v-model="f.project" class="select sm"><option value="">Any</option><option v-for="o in opts.project" :key="o" :value="o">{{ o }}</option></select></label>
        <label class="grow"><span>Object</span><input v-model="f.object" class="input sm" placeholder="Name or group id" data-testid="filter-object" /></label>
        <label><span>Kind</span><select v-model="f.kind" class="select sm"><option value="">Any</option><option v-for="o in opts.kind" :key="o" :value="o">{{ kindLabel(o) }}</option></select></label>
        <label><span>Policy</span><select v-model="f.policy" class="select sm"><option value="">Any</option><option v-for="o in opts.policy" :key="o" :value="o">{{ o }}</option></select></label>
        <label><span>Protection</span><select v-model="f.protected" class="select sm"><option value="">Any</option><option value="yes">Protected</option><option value="no">Not protected</option></select></label>
        <label><span>Verification</span><select v-model="f.verify" class="select sm" data-testid="filter-verify"><option value="">Any</option><option value="ok">Verified</option><option value="stale">Stale</option><option value="never">Never verified</option><option value="failed">Failed</option></select></label>
        <label><span>From</span><input v-model="f.from" type="date" class="input sm" /></label>
        <label><span>To</span><input v-model="f.to" type="date" class="input sm" /></label>
        <button v-if="active" type="button" class="btn btn-sm btn-ghost clr" @click="clearFilters"><PIcon name="x" :size="13" />Clear {{ active }} {{ noun(active, 'filter') }}</button>
      </form>

      <Callout v-if="statsMissing" status="info" class="note">This server does not report the run statistics (changed and uploaded bytes, duration); those columns stay empty.</Callout>

      <DataTable :rows="rows === undefined ? undefined : shown" :columns="cols" row-key="key" storage-key="snapshots" csv-name="snapshots" :loading="snaps.loading.value" :error="snaps.error.value" :retry="refresh"
        :initial-sort="{ key: 'created', dir: 'desc' }" selectable :selected="selected" row-clickable filter-placeholder="Search the rows shown" max-height="min(68vh, 760px)" data-testid="snapshot-table"
        @update:selected="selected = $event" @row-click="open">
        <template #cell-object="{ row }">
          <RouterLink class="obj" :to="{ name: 'snapshot', params: { group: row.group_id, snap: row.snapshot_id } }">
            <PIcon :name="row.kind === 'block' ? 'hard-drive' : row.kind === 'instance' ? 'box' : 'folder'" :size="14" class="k" />
            <span class="truncate"><span class="muted">{{ row.project }}/</span>{{ row.object }}</span>
          </RouterLink>
        </template>
        <template #cell-created="{ row }"><RelativeTime :time="row.created" /></template>
        <template #cell-logical_bytes="{ row }"><ByteSize :bytes="row.logical_bytes" /></template>
        <template #cell-dirty="{ row }"><ByteSize v-if="row.stats" :bytes="row.stats.dirty_bytes" /><span v-else class="muted">–</span></template>
        <template #cell-uploaded="{ row }"><ByteSize v-if="row.stats" :bytes="row.stats.uploaded_bytes" /><span v-else class="muted">–</span></template>
        <template #cell-duration="{ row }"><DurationText v-if="row.stats && row.stats.duration_ms" :ms="row.stats.duration_ms" /><span v-else class="muted">–</span></template>
        <template #cell-full="{ row }">
          <Tooltip v-if="row.stats?.full_read" text="This run read the whole object (first backup, a lost or doubtful change map, or a verification failure of the base). Incremental runs read only what changed.">
            <span class="full">full read</span>
          </Tooltip>
          <span v-else-if="row.stats" class="muted">incremental</span>
          <span v-else class="muted">–</span>
        </template>
        <template #cell-verify="{ row }"><VerifyBadge :state="row.verifyClass" :at="row.verifiedAt" /></template>
        <template #cell-flags="{ row }">
          <span class="flags">
            <Tooltip v-if="row.protected" text="Protected: retention and forget skip it."><PIcon name="lock" :size="14" class="lock" label="Protected" /></Tooltip>
            <Tooltip v-if="row.notes" :text="row.notes"><PIcon name="scroll-text" :size="14" label="Has notes" /></Tooltip>
          </span>
        </template>
        <template #bulk>
          <template v-if="busy"><span class="muted small">Working… {{ progress.done }} of {{ progress.total }}</span></template>
          <template v-else>
            <Tooltip v-if="!canProtect" text="Protecting needs the operator role."><button type="button" class="btn btn-sm" disabled><PIcon name="lock" :size="13" />Protect</button></Tooltip>
            <template v-else>
              <button type="button" class="btn btn-sm" data-testid="bulk-protect" @click="bulkProtect(true)"><PIcon name="lock" :size="13" />Protect</button>
              <button type="button" class="btn btn-sm" data-testid="bulk-unprotect" @click="bulkProtect(false)"><PIcon name="lock-open" :size="13" />Unprotect</button>
            </template>
            <Tooltip v-if="!canForget" text="Forgetting snapshots needs the admin role."><button type="button" class="btn btn-sm" disabled><PIcon name="trash-2" :size="13" />Forget</button></Tooltip>
            <button v-else type="button" class="btn btn-sm btn-danger" data-testid="bulk-forget" @click="forgetOpen = true"><PIcon name="trash-2" :size="13" />Forget…</button>
          </template>
        </template>
        <template #empty>
          <EmptyState v-if="active" title="No snapshot matches these filters" icon="filter">
            <template #actions><button type="button" class="btn" @click="clearFilters">Clear the filters</button></template>
          </EmptyState>
          <EmptyState v-else title="No snapshots yet" icon="history">A host creates snapshots on its policy schedule. Hosts that are not enrolled yet are added under Hosts.
            <template #actions><RouterLink class="btn" :to="{ name: 'hosts' }">Hosts</RouterLink></template>
          </EmptyState>
        </template>
      </DataTable>
    </template>

    <Modal v-model:open="forgetOpen" title="Forget snapshots" :description="`${selRows.length} selected`" width="560px">
      <div class="stack">
        <p v-if="plan.allowed.length">{{ plan.allowed.length }} {{ noun(plan.allowed.length, 'snapshot') }} can be forgotten. Forgetting removes the restore point for good; its space is freed by the next garbage collection.</p>
        <Callout v-if="plan.refused.length" status="warn" :title="`${plan.refused.length} protected ${noun(plan.refused.length, 'snapshot is', 'snapshots are')} kept`" data-testid="forget-refused">
          Protected snapshots are never forgotten. Unprotect them first if you really mean it:
          <ul class="ref"><li v-for="r in plan.refused.slice(0, 8)" :key="r.key"><span class="mono">{{ r.snapshot_id }}</span> of {{ r.project }}/{{ r.object }}<template v-if="r.notes"> ("{{ r.notes }}")</template></li></ul>
          <span v-if="plan.refused.length > 8">and {{ plan.refused.length - 8 }} more.</span>
        </Callout>
        <p v-if="!plan.allowed.length" class="muted">Nothing to forget.</p>
      </div>
      <template #footer>
        <button type="button" class="btn" @click="forgetOpen = false">Cancel</button>
        <button type="button" class="btn btn-danger" :disabled="!plan.allowed.length" data-testid="forget-continue" @click="bulkForget">Forget {{ plan.allowed.length }}…</button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: var(--s-3); margin-bottom: var(--s-4); }
.filters { display: grid; grid-template-columns: repeat(auto-fill, minmax(132px, 1fr)); gap: var(--s-2) var(--s-3); padding: var(--s-3); margin-bottom: var(--s-3); align-items: end; }
.filters label { display: flex; flex-direction: column; gap: 3px; font-size: var(--fs-xs); color: var(--text-muted); min-width: 0; }
.filters label.grow { grid-column: span 2; }
.filters .input, .filters .select { width: 100%; }
.filters .sm { height: var(--control-h-sm); font-size: var(--fs-sm); }
.clr { align-self: flex-end; }
.note { margin-bottom: var(--s-3); }
.obj { display: inline-flex; align-items: center; gap: 6px; max-width: 100%; color: var(--text); text-decoration: none; font-weight: var(--fw-medium); }
.obj:hover .truncate { text-decoration: underline; }
.obj .k { color: var(--text-muted); }
.full { font-size: var(--fs-sm); color: var(--warn-text); font-weight: var(--fw-medium); }
.flags { display: inline-flex; gap: 6px; color: var(--text-muted); }
.lock { color: var(--accent-text); }
.small { font-size: var(--fs-sm); }
.mono { font-family: var(--font-mono); font-size: var(--fs-sm); }
.ref { margin: var(--s-2) 0 0; padding-left: 18px; }
</style>
