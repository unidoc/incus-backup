<script setup lang="ts">
// Silences: what is muted, why, by whom and until when; how many open alerts each one holds now.
// Expire one now; create one with the matcher builder.
import { computed, ref } from 'vue'
import CIcon from '@/components/config/CIcon.vue'
import SubNav from '@/components/config/SubNav.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import EmptyState from '@/components/EmptyState.vue'
import SilenceForm from '@/components/config/SilenceForm.vue'
import { useResource } from '@/api/cache'
import { cfg, list, canCfg, type Silence } from '@/api/sections/config'
import { silenceMatches } from '@/lib/glob'
import { session } from '@/stores/session'
import { toast } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import { useNow } from '@/composables/useNow'
import type { ColumnDef } from '@/lib/table'
import '@/components/config/section.css'

const now = useNow()
const showExpired = ref(false)
const sil = useResource(() => `cfg:silences:${showExpired.value}`, (s) => cfg.silences.list(showExpired.value, s), { refreshOn: ['alert'], ttl: 5000 })
const open = useResource(() => 'cfg:alerts:open', (s) => cfg.alerts.list({ active: 1, limit: 1000 }, s), { refreshOn: ['alert'], ttl: 5000 })
const canEdit = computed(() => canCfg(session.role, 'POST /silences'))

type Row = Silence & { phase: 'active' | 'scheduled' | 'expired'; holds: number }
const rows = computed<Row[]>(() => list(sil.data.value).map((s) => {
  const phase: Row['phase'] = Date.parse(s.until) <= now.value ? 'expired' : Date.parse(s.starts_at) > now.value ? 'scheduled' : 'active'
  return { ...s, phase, holds: phase === 'expired' ? 0 : list(open.data.value).filter((a) => silenceMatches(s, a)).length }
}))
const activeCount = computed(() => rows.value.filter((r) => r.phase === 'active').length)

const cols: ColumnDef<Row>[] = [
  { key: 'phase', label: 'State', width: '110px' },
  { key: 'matchers', label: 'Matches', required: true, value: (r) => [r.type_glob, r.key_glob, r.host_glob, r.repo_glob, r.object_glob].filter(Boolean).join(' ') },
  { key: 'holds', label: 'Holds now', align: 'right', width: '90px' },
  { key: 'reason', label: 'Reason' },
  { key: 'author', label: 'By', width: '110px' },
  { key: 'until', label: 'Ends', width: '150px', value: (r) => Date.parse(r.until) },
  { key: 'created', label: 'Created', hidden: true },
  { key: 'actions', label: '', sortable: false, noExport: true, align: 'right', width: '120px', required: true },
]
const matchers = (s: Silence) => ([['type', s.type_glob], ['host', s.host_glob], ['repository', s.repo_glob], ['object', s.object_glob], ['key', s.key_glob]] as const).filter(([, v]) => v)

const formOpen = ref(false)
async function expire(s: Row) {
  if (!(await confirm({ title: 'Expire this silence now?', message: `${s.holds} open alert${s.holds === 1 ? '' : 's'} it holds will be announced at the next evaluation if they still fire.`, confirmLabel: 'Expire now' }))) return
  try { await cfg.silences.expire(s.id); toast.success('Silence expired', 'Matching alerts notify again.'); void sil.refresh(); void open.refresh() } catch (e) { toast.error(e, 'Could not expire the silence') }
}
</script>

<template>
  <div>
    <header class="page-head">
      <div>
        <h1>Silences</h1>
        <p class="lede">A silence holds every message about matching alerts until it ends, for planned work. The alerts stay visible.</p>
      </div>
      <div class="actions">
        <button v-if="canEdit" type="button" class="btn btn-primary" data-testid="silence-new" @click="formOpen = true"><CIcon name="bell-off" :size="14" />New silence</button>
      </div>
    </header>
    <SubNav group="alerts" />

    <DataTable :rows="rows" :columns="cols" row-key="id" :loading="sil.loading.value" :error="sil.error.value" :retry="sil.refresh" storage-key="cfg-silences" csv-name="silences"
      :initial-sort="{ key: 'until', dir: 'desc' }" data-testid="silence-table">
      <template #toolbar>
        <label class="exp"><input v-model="showExpired" type="checkbox" /> Show expired</label>
        <span class="muted small">{{ activeCount }} active</span>
      </template>
      <template #cell-phase="{ row }"><StatusPill :status="row.phase === 'active' ? 'info' : row.phase === 'scheduled' ? 'neutral' : 'neutral'" :label="row.phase" size="sm" /></template>
      <template #cell-matchers="{ row }">
        <span class="mt"><span v-for="[k, v] in matchers(row)" :key="k" class="cx-tag mono"><span class="muted">{{ k }}</span>&nbsp;{{ v }}</span></span>
      </template>
      <template #cell-holds="{ row }"><span class="num">{{ row.phase === 'expired' ? '–' : row.holds }}</span></template>
      <template #cell-reason="{ row }"><span class="rs" :title="row.reason">{{ row.reason }}</span></template>
      <template #cell-until="{ row }"><span v-if="row.phase !== 'expired'">in <RelativeTime :time="row.until" /></span><AbsoluteTime v-else :time="row.until" /></template>
      <template #cell-created="{ row }"><AbsoluteTime :time="row.created" /></template>
      <template #cell-actions="{ row }">
        <button v-if="row.phase !== 'expired'" type="button" class="btn btn-sm" :disabled="!canEdit" :title="canEdit ? '' : 'Needs the operator role'" data-testid="silence-expire" @click="expire(row)">Expire now</button>
      </template>
      <template #empty>
        <EmptyState title="No active silences" icon="circle-check">Every alert notifies its rules. Silence alerts before planned maintenance, so nobody is paged for it.
          <template v-if="canEdit" #actions><button type="button" class="btn btn-sm" @click="formOpen = true">New silence</button></template>
        </EmptyState>
      </template>
    </DataTable>
    <SilenceForm v-model:open="formOpen" @created="() => { void sil.refresh(); void open.refresh() }" />
  </div>
</template>

<style scoped>
.exp { display: inline-flex; align-items: center; gap: 6px; font-size: var(--fs-sm); color: var(--text-2); }
.mt { display: inline-flex; flex-wrap: wrap; gap: 4px; }
.rs { display: block; max-width: 40ch; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
