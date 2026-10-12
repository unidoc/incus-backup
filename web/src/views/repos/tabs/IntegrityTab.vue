<script setup lang="ts">
// Integrity: quarantined chunks and the snapshots they break, snapshots that failed verification,
// and how healing works (the next backup of an affected object reads everything again and re-uploads
// the chunk; a synced copy can be repaired with resync-corrupt).
import { computed, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import EmptyState from '@/components/EmptyState.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import { list } from '@/api/endpoints'
import { useResource } from '@/api/cache'
import type { RepoApi } from '@/api/sections/storage'
import { selectRepo } from '@/stores/repos'
import { plural, shortId } from '@/lib/format'
import type { ColumnDef } from '@/lib/table'
import type { CorruptChunk, RepoView, SnapshotRef } from '@/api/types'

const props = defineProps<{ repo: RepoView; api: RepoApi }>()
const router = useRouter()
const healed = ref(false)
const corrupt = useResource(() => `corrupt:${props.repo.name}:${healed.value}`, (s) => props.api.corrupt(healed.value, s), { refreshOn: ['run'] })
const snaps = useResource(() => `snapshots:${props.repo.name}:all`, (s) => props.api.snapshots(s), { refreshOn: ['run'], ttl: 60_000 })
const failed = computed(() => list(snaps.data.value).filter((x) => x.verify_state === 'failed'))
const quarantined = computed(() => list(corrupt.data.value).filter((c) => c.state !== 'healed').length)

/** Snapshot pages read the top-bar repository: point it at this one first. */
function openSnapshot(group: string, snap: string) {
  selectRepo(props.repo.name)
  void router.push({ name: 'snapshot', params: { group, snap } })
}
const objectOf = (group: string) => {
  const s = list(snaps.data.value).find((x) => x.group_id === group)
  return s ? `${s.project ?? ''}/${s.object ?? ''}` : shortId(group, 12)
}
const ccols: ColumnDef<CorruptChunk>[] = [
  { key: 'chunk_id', label: 'Chunk', mono: true, required: true, format: (c) => shortId(c.chunk_id, 16) },
  { key: 'state', label: 'State' },
  { key: 'detected_at', label: 'Detected' },
  { key: 'healed_at', label: 'Healed', value: (c) => c.healed_at ?? '' },
  { key: 'detail', label: 'Finding' },
  { key: 'affected', label: 'Affected snapshots', value: (c) => (c.affected ?? []).join(' '), sortable: false },
]
const fcols: ColumnDef<SnapshotRef>[] = [
  { key: 'object', label: 'Object', value: (s) => `${s.project ?? ''}/${s.object ?? ''}`, required: true },
  { key: 'snapshot_id', label: 'Snapshot', mono: true },
  { key: 'host', label: 'Host' },
  { key: 'committed', label: 'Committed' },
  { key: 'protected', label: 'Protected', value: (s) => (s.protected ? 'yes' : '') },
]
</script>

<template>
  <div class="integ">
    <section class="panel">
      <div class="panel-head"><h2>Quarantined chunks</h2><span class="sub">{{ quarantined ? plural(quarantined, 'chunk') + ' quarantined' : 'none quarantined' }}</span><span class="spacer" />
        <SwitchInput v-model="healed" label="Include healed" /></div>
      <DataTable :rows="corrupt.data.value === undefined ? undefined : list(corrupt.data.value)" :columns="ccols" row-key="chunk_id" storage-key="corrupt" csv-name="corrupt-chunks" :loading="corrupt.loading.value" :error="corrupt.error.value" :retry="corrupt.refresh" dense>
        <template #cell-state="{ row }"><StatusPill :status="row.state === 'healed' ? 'ok' : 'critical'" :label="row.state" size="sm" /></template>
        <template #cell-detected_at="{ row }"><RelativeTime :time="row.detected_at" /></template>
        <template #cell-healed_at="{ row }"><RelativeTime :time="row.healed_at" never="–" /></template>
        <template #cell-affected="{ row }">
          <span class="aff">
            <button v-for="a in (row.affected ?? []).slice(0, 4)" :key="a" type="button" class="link mono" @click="openSnapshot(a.split('/')[0], a.split('/')[1])">{{ objectOf(a.split('/')[0]) }} @ {{ a.split('/')[1] }}</button>
            <span v-if="(row.affected ?? []).length > 4" class="muted small">+{{ (row.affected ?? []).length - 4 }} more</span>
          </span>
        </template>
        <template #empty><EmptyState title="No quarantined chunks" icon="shield-check" compact>Verification found no corrupt chunk{{ healed ? '' : ' that is still unhealed' }}.</EmptyState></template>
      </DataTable>
    </section>

    <section class="panel">
      <div class="panel-head"><h2>Snapshots that failed verification</h2><span class="sub">never used as an incremental base</span></div>
      <DataTable :rows="snaps.data.value === undefined ? undefined : failed" :columns="fcols" :row-key="(s) => `${s.group_id}/${s.snapshot_id}`" storage-key="failed-verify" csv-name="failed-verifications" :loading="snaps.loading.value" :error="snaps.error.value" :retry="snaps.refresh" dense
        row-clickable @row-click="(s) => openSnapshot(s.group_id, s.snapshot_id)">
        <template #cell-committed="{ row }"><RelativeTime :time="row.committed" /></template>
        <template #empty><EmptyState title="No failed verification" icon="shield-check" compact>Every verified snapshot passed its last verification.</EmptyState></template>
      </DataTable>
    </section>

    <section class="panel">
      <div class="panel-head"><h2>How healing works</h2></div>
      <ol class="panel-body steps">
        <li><b>Quarantine.</b> A corrupt chunk is moved aside so it no longer satisfies deduplication; every snapshot that uses it is marked failed.</li>
        <li><b>Next backup heals.</b> A failed snapshot is never an incremental base: the next backup of the object reads everything again and uploads a good copy of the chunk. Run that backup now on the host if you cannot wait for its schedule.</li>
        <li><b>Verify again.</b> Run <i>Verify</i> (Tasks tab, “new and stale”) to clear the failed state; the chunk shows as healed here.</li>
        <li><b>A copy elsewhere.</b> If another server holds this data, a pull job with <i>resync corrupt</i> repairs the snapshots from its healthy copy (<RouterLink :to="{ name: 'sync' }">Sync</RouterLink>).</li>
        <li><b>Root cause.</b> Repeated corruption on a filesystem repository points at the disk or memory: check SMART, ZFS scrub results and ECC logs.</li>
      </ol>
    </section>
  </div>
</template>

<style scoped>
.integ { display: flex; flex-direction: column; gap: var(--s-4); }
.integ .panel > :deep(.dt) { border: 0; border-radius: 0 0 var(--r-lg) var(--r-lg); }
.aff { display: inline-flex; flex-wrap: wrap; gap: 4px 10px; }
.link { border: 0; background: none; padding: 0; color: var(--accent-text); cursor: pointer; font-size: 12px; }
.link:hover { text-decoration: underline; }
.steps { margin: 0; padding-left: 36px; display: flex; flex-direction: column; gap: 8px; color: var(--text-2); }
</style>
