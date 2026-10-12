<script setup lang="ts">
// The audit log: every state-changing request and every sign-in, newest first. Filter by actor and
// action (server side), by result (on the loaded entries), page back with "Load older", export CSV.
import { computed, ref, watch } from 'vue'
import CIcon from '@/components/config/CIcon.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import EmptyState from '@/components/EmptyState.vue'
import Modal from '@/components/Modal.vue'
import KeyValue from '@/components/KeyValue.vue'
import Spinner from '@/components/Spinner.vue'
import Callout from '@/components/Callout.vue'
import { cfg, list } from '@/api/sections/config'
import { errorText } from '@/stores/toast'
import { toCSV } from '@/lib/table'
import { downloadText } from '@/composables/useClipboard'
import type { AuditEntry } from '@/api/types'
import type { ColumnDef } from '@/lib/table'
import '@/components/config/section.css'

const PAGE = 200
const actor = ref('')
const action = ref('')
const result = ref<'all' | 'ok' | 'failed'>('all')
const entries = ref<AuditEntry[]>([])
const loading = ref(false)
const more = ref(true)
const error = ref('')
let gen = 0

async function load(reset: boolean) {
  const my = ++gen
  loading.value = true
  error.value = ''
  try {
    const before = reset || !entries.value.length ? undefined : entries.value[entries.value.length - 1].id
    const l = list(await cfg.audit({ actor: actor.value.trim() || undefined, action: action.value.trim() || undefined, before, limit: PAGE }))
    if (my !== gen) return
    entries.value = reset ? l : [...entries.value, ...l]
    more.value = l.length === PAGE
  } catch (e) { if (my === gen) error.value = errorText(e) } finally { if (my === gen) loading.value = false }
}
let t: ReturnType<typeof setTimeout> | null = null
watch([actor, action], () => { if (t) clearTimeout(t); t = setTimeout(() => void load(true), 300) })
void load(true)

const rows = computed(() => entries.value.filter((e) => result.value === 'all' || (result.value === 'ok') === e.ok))
const failedLoaded = computed(() => entries.value.filter((e) => !e.ok).length)
const actions = ['login', 'setup', 'post /users', 'put /users', 'delete /users', 'post /me/tokens', 'put /settings', 'post /notifications', 'put /notifications', 'post /certificates', 'put /certificates', 'post /prune', 'post /gc', 'delete /snapshots', 'put /repositories', 'post /jobs']
const cols: ColumnDef<AuditEntry>[] = [
  { key: 'at', label: 'When', width: '170px', value: (e) => e.at },
  { key: 'ok', label: 'Result', width: '96px', format: (e) => (e.ok ? 'ok' : 'failed') },
  { key: 'actor', label: 'Who', width: '150px', format: (e) => `${e.actor} (${e.actor_kind})` },
  { key: 'action', label: 'Action', mono: true },
  { key: 'target', label: 'Target', mono: true },
  { key: 'ip', label: 'Address', mono: true, width: '130px' },
  { key: 'id', label: 'ID', hidden: true, align: 'right' },
]
const open = ref<AuditEntry | null>(null)
const detailText = (d: unknown) => {
  if (!d || (typeof d === 'object' && !Object.keys(d as object).length)) return ''
  return JSON.stringify(d, null, 2)
}
function exportCSV() {
  downloadText(`audit-${new Date().toISOString().slice(0, 10)}.csv`, toCSV(rows.value, cols), 'text/csv')
}
</script>

<template>
  <div>
    <header class="page-head">
      <div>
        <h1>Audit log</h1>
        <p class="lede">Every change and every sign-in: who, what, from where, and whether it worked. Request bodies and secrets are never logged.</p>
      </div>
      <div class="actions"><button type="button" class="btn" :disabled="!rows.length" data-testid="audit-csv" @click="exportCSV"><CIcon name="inbox" :size="14" />Export CSV</button></div>
    </header>

    <div class="cx-bar">
      <input v-model="actor" class="input f" placeholder="Who (exact name)" aria-label="Actor" spellcheck="false" data-testid="audit-actor" />
      <input v-model="action" class="input f" list="audit-actions" placeholder="Action starts with…" aria-label="Action prefix" spellcheck="false" data-testid="audit-action" />
      <datalist id="audit-actions"><option v-for="a in actions" :key="a" :value="a" /></datalist>
      <div class="cx-seg" role="group" aria-label="Result">
        <button type="button" :aria-pressed="result === 'all'" @click="result = 'all'">All</button>
        <button type="button" :aria-pressed="result === 'ok'" @click="result = 'ok'">Succeeded</button>
        <button type="button" :aria-pressed="result === 'failed'" data-testid="audit-failed" @click="result = 'failed'">Failed<span class="n">{{ failedLoaded }}</span></button>
      </div>
      <span class="grow" />
      <span class="muted small">{{ rows.length }} of {{ entries.length }} loaded</span>
    </div>
    <p v-if="result !== 'all'" class="cx-help note">The result filter applies to the entries loaded so far; load older ones to search further back.</p>
    <Callout v-if="error" status="critical" title="Could not load the audit log" :retry="() => load(!entries.length)">{{ error }}</Callout>

    <DataTable :rows="rows" :columns="cols" row-key="id" :loading="loading && !entries.length" :toolbar="false" row-clickable data-testid="audit-table" @row-click="(e: AuditEntry) => (open = e)">
      <template #cell-at="{ row }"><AbsoluteTime :time="row.at" seconds /></template>
      <template #cell-ok="{ row }"><StatusPill :status="row.ok ? 'ok' : 'critical'" :label="row.ok ? 'ok' : 'failed'" size="sm" /></template>
      <template #cell-actor="{ row }"><span class="who"><strong>{{ row.actor || '–' }}</strong><span class="cx-tag">{{ row.actor_kind || 'anonymous' }}</span></span></template>
      <template #cell-target="{ row }"><span class="tg" :title="row.target">{{ row.target }}</span></template>
      <template #empty><EmptyState title="No entries" icon="scroll-text">{{ actor || action || result !== 'all' ? 'Nothing matches the filters.' : 'Nothing was changed yet.' }}</EmptyState></template>
    </DataTable>
    <div class="morebar">
      <button v-if="more && entries.length" type="button" class="btn" :disabled="loading" data-testid="audit-more" @click="load(false)"><Spinner v-if="loading" :size="13" />Load older entries</button>
      <span v-else-if="entries.length" class="muted small">The beginning of the log.</span>
    </div>

    <Modal :open="!!open" drawer title="Audit entry" width="520px" @update:open="(v: boolean) => { if (!v) open = null }">
      <div v-if="open" class="cx-form" data-testid="audit-detail">
        <KeyValue :items="[
          { key: 'at', label: 'When', value: open.at },
          { key: 'ok', label: 'Result', value: open.ok ? 'ok' : 'failed' },
          { key: 'actor', label: 'Who', value: `${open.actor} (${open.actor_kind || 'anonymous'})` },
          { key: 'action', label: 'Action', value: open.action, mono: true },
          { key: 'target', label: 'Target', value: open.target, mono: true },
          { key: 'ip', label: 'Address', value: open.ip, mono: true },
          { key: 'id', label: 'Entry', value: open.id },
        ]">
          <template #value-at><AbsoluteTime :time="open.at" seconds /> · <RelativeTime :time="open.at" /></template>
          <template #value-ok><StatusPill :status="open.ok ? 'ok' : 'critical'" :label="open.ok ? 'ok' : 'failed'" size="sm" /></template>
        </KeyValue>
        <div v-if="detailText(open.detail)"><h3 class="dh">Detail</h3><pre class="dt">{{ detailText(open.detail) }}</pre></div>
        <p v-if="!open.ok" class="cx-help">A failed entry is a refused or failed request: a wrong password, a missing role, invalid input. Many failed sign-ins from one address are worth a look.</p>
      </div>
    </Modal>
  </div>
</template>

<style scoped>
.f { width: 200px; }
.note { margin: -4px 0 10px; }
.who { display: inline-flex; gap: 6px; align-items: center; }
.tg { display: block; max-width: 44ch; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.morebar { display: flex; justify-content: center; padding: 14px 0; }
.dh { font-size: var(--fs-md); margin-bottom: 6px; }
.dt { white-space: pre-wrap; font-family: var(--font-mono); font-size: var(--fs-sm); background: var(--surface-2); border: 1px solid var(--border); border-radius: var(--r); padding: 10px 12px; margin: 0; overflow-wrap: anywhere; }
@media (max-width: 640px) { .f { width: 100%; } }
</style>
