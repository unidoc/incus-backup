<script setup lang="ts">
// The retention plan (POST /prune?dry_run=1, or a finished retention run's stats): every snapshot with
// keep / remove / protected and the rule that kept it, per object. The preview IS what apply does
// (same code path, same server time), as long as nothing changes in between.
import { computed, ref } from 'vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import { formatNumber } from '@/lib/format'
import type { ColumnDef } from '@/lib/table'
import type { PrunePlanEntry } from '@/api/types'

const props = defineProps<{ plan: PrunePlanEntry[] | null; dryRun?: boolean }>()
type F = 'all' | 'remove' | 'keep' | 'protected'
const filter = ref<F>(props.plan?.some((p) => !p.keep) ? 'remove' : 'all')
const rows = computed(() => (props.plan ?? []).map((p) => ({ ...p, key: `${p.group}/${p.snapshot}`, decision: p.protected ? 'protected' : p.keep ? 'keep' : 'remove', at: snapTime(p.snapshot) })))
const shown = computed(() => rows.value.filter((r) => filter.value === 'all' || r.decision === filter.value))
const counts = computed(() => ({ all: rows.value.length, remove: rows.value.filter((r) => r.decision === 'remove').length, keep: rows.value.filter((r) => r.decision === 'keep').length, protected: rows.value.filter((r) => r.decision === 'protected').length }))
const perObject = computed(() => {
  const m = new Map<string, { object: string; policy: string; tz: string; keep: number; remove: number; protected: number }>()
  for (const r of rows.value) {
    let x = m.get(r.group)
    if (!x) m.set(r.group, (x = { object: r.object, policy: r.policy, tz: r.timezone, keep: 0, remove: 0, protected: 0 }))
    x[r.decision as 'keep' | 'remove' | 'protected']++
  }
  return [...m.values()].filter((x) => x.remove > 0).sort((a, b) => b.remove - a.remove)
})

/** "20261004030000-0000009a" -> ISO time (snapshot ids start with the host's UTC creation time). */
function snapTime(id: string): string {
  const m = /^(\d{4})(\d\d)(\d\d)(\d\d)(\d\d)(\d\d)/.exec(id)
  return m ? `${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}Z` : ''
}
const reasonText = (r: string) => (r ? r.replace(/,/g, ', ') : '–')
const cols: ColumnDef<(typeof rows.value)[number]>[] = [
  { key: 'object', label: 'Object', required: true },
  { key: 'snapshot', label: 'Snapshot', mono: true },
  { key: 'at', label: 'Created (UTC)', format: (r) => (r.at ? r.at.replace('T', ' ').replace(':00Z', '') : '–') },
  { key: 'decision', label: 'Decision' },
  { key: 'reason', label: 'Kept by', format: (r) => reasonText(r.reason) },
  { key: 'policy', label: 'Policy' },
  { key: 'timezone', label: 'Zone', hidden: true },
]
</script>

<template>
  <div class="ppt" data-testid="prune-plan">
    <div class="chips" role="group" aria-label="Show">
      <button v-for="k in (['remove', 'keep', 'protected', 'all'] as F[])" :key="k" type="button" class="chip" :class="[{ on: filter === k }, `c-${k}`]" :aria-pressed="filter === k" @click="filter = k">
        {{ k === 'remove' ? (dryRun ? 'Would remove' : 'Removed') : k === 'keep' ? 'Kept' : k === 'protected' ? 'Protected' : 'All' }} <b class="num">{{ formatNumber(counts[k]) }}</b>
      </button>
    </div>
    <ul v-if="perObject.length" class="objs">
      <li v-for="o in perObject.slice(0, 8)" :key="o.object"><b>{{ o.object }}</b> <span class="muted">{{ o.policy || 'no policy' }} · {{ o.tz }}</span>
        <span class="spacer" /><span class="rm num">−{{ o.remove }}</span><span class="kp num">{{ o.keep + o.protected }} kept</span></li>
      <li v-if="perObject.length > 8" class="muted small">and {{ perObject.length - 8 }} more objects</li>
    </ul>
    <DataTable :rows="shown" :columns="cols" row-key="key" storage-key="prune-plan" csv-name="retention-plan" dense :initial-sort="{ key: 'object', dir: 'asc' }" max-height="420px"
      :empty-title="filter === 'remove' ? 'Nothing to remove: every snapshot is kept' : 'No snapshots'">
      <template #cell-decision="{ row }">
        <StatusPill :status="row.decision === 'remove' ? 'critical' : row.decision === 'protected' ? 'info' : 'ok'" :label="row.decision === 'remove' ? (dryRun ? 'would remove' : 'removed') : row.decision" size="sm" />
      </template>
    </DataTable>
  </div>
</template>

<style scoped>
.ppt { display: flex; flex-direction: column; gap: 10px; }
.chips { display: flex; gap: 6px; flex-wrap: wrap; }
.chip { display: inline-flex; gap: 6px; align-items: center; height: 26px; padding: 0 10px; border: 1px solid var(--border-strong); border-radius: var(--r-pill); background: var(--surface); color: var(--text-2); cursor: pointer; font-size: var(--fs-sm); }
.chip.on { border-color: var(--accent); background: var(--accent-soft); color: var(--text); }
.chip.c-remove b { color: var(--critical-text); }
.objs { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 2px; font-size: var(--fs-sm); }
.objs li { display: flex; gap: 8px; align-items: center; padding: 3px 0; border-bottom: 1px dashed var(--border); }
.rm { color: var(--critical-text); font-weight: var(--fw-semibold); }
.kp { color: var(--text-muted); min-width: 64px; text-align: right; }
</style>
