<script setup lang="ts">
// One snapshot: what it is, how the run went, what changed against the previous snapshot, whether it
// is intact (verification history, quarantined chunks), protection and notes, and every way back:
// browse files, restore, recover files on the host. Neighbours (previous/next) are one click away.
import { computed, ref, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import KeyValue from '@/components/KeyValue.vue'
import StatTile from '@/components/StatTile.vue'
import Callout from '@/components/Callout.vue'
import EmptyState from '@/components/EmptyState.vue'
import Skeleton from '@/components/Skeleton.vue'
import ByteSize from '@/components/ByteSize.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import StatusPill from '@/components/StatusPill.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import Tooltip from '@/components/Tooltip.vue'
import PIcon from '@/components/protect/PIcon.vue'
import VerifyBadge from '@/components/protect/VerifyBadge.vue'
import { useSnapRows } from '@/components/protect/useSnapRows'
import { api, list } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import { protectApi } from '@/api/sections/protect'
import { repos } from '@/stores/repos'
import { hasRole } from '@/stores/session'
import { toast } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import { changeSummary, kindLabel } from '@/lib/protect/snapshots'
import { formatBytes, formatDuration, formatNumber } from '@/lib/format'
import { noun } from '@/lib/protect/text'

const props = defineProps<{ group: string; snap: string }>()
const router = useRouter()
const { rows, snaps, statsMissing, refresh } = useSnapRows(() => props.group)
const row = computed(() => rows.value?.find((r) => r.snapshot_id === props.snap) ?? null)
const prev = computed(() => (row.value?.prevId ? rows.value?.find((r) => r.snapshot_id === row.value!.prevId) ?? null : null))
const next = computed(() => (row.value?.nextId ? rows.value?.find((r) => r.snapshot_id === row.value!.nextId) ?? null : null))
const siblings = computed(() => (rows.value ?? []).length)
const position = computed(() => (rows.value ?? []).filter((r) => r.snapshot_id <= props.snap).length)

const ver = useResource(() => `verifications:${repos.current}:${props.group}:${props.snap}`, (s) => api.snapshots.verifications(props.group, props.snap, s), { enabled: () => !!repos.current, refreshOn: ['run'] })
const corrupt = useResource(() => `corrupt:${repos.current}`, (s) => api.maintenance.corrupt(false, s), { enabled: () => !!repos.current, refreshOn: ['run'] })
const myCorrupt = computed(() => list(corrupt.data.value).filter((c) => (c.affected ?? []).includes(`${props.group}/${props.snap}`)))
const objKey = computed(() => (row.value ? `${row.value.project}/${row.value.object}` : ''))
const tests = useResource(() => `restore-tests:${repos.current}:${objKey.value}`, (s) => protectApi.restoreTests.list({ object: objKey.value, limit: 5 }, s).catch(() => null), { enabled: () => !!repos.current && !!objKey.value, refreshOn: ['job'] })

const change = computed(() => (row.value ? changeSummary(row.value, prev.value) : null))
const kind = computed(() => row.value?.kind ?? '')
const isVolume = computed(() => (row.value?.object ?? '').startsWith('volume/'))
const instanceName = computed(() => { const o = row.value?.object ?? ''; return o.startsWith('instance/') ? o.split('/')[1] : '' })

const items = computed(() => {
  const r = row.value
  if (!r) return []
  return [
    { key: 'object', label: 'Object', value: `${r.project}/${r.object}` },
    { key: 'kind', label: 'Kind', value: kindLabel(r.kind) },
    { key: 'host', label: 'Host', value: r.host },
    { key: 'policy', label: 'Policy', value: r.policy },
    { key: 'created', label: 'Created' },
    { key: 'committed', label: 'Committed' },
    { key: 'snapshot', label: 'Snapshot id', value: r.snapshot_id, mono: true },
    { key: 'group', label: 'Group id', value: r.group_id, mono: true },
    { key: 'base', label: 'Built on', value: r.stats?.previous_snapshot_id || (r.stats ? 'nothing (full)' : null), mono: !!r.stats?.previous_snapshot_id },
    { key: 'chunks', label: 'Chunks', value: formatNumber(r.chunk_count) },
    { key: 'digest', label: 'Index digest', value: r.index_digest, mono: true },
    { key: 'source', label: 'Recorded by', value: r.stats ? ({ client: 'a backup run', sync: 'synchronisation from another server', reconcile: 'reconcile (rebuilt from the datastore)' } as Record<string, string>)[r.stats.source] ?? r.stats.source : null },
  ]
})

// ---- protection and notes
const canEdit = computed(() => hasRole('operator'))
const savingProt = ref(false)
async function setProtected(v: boolean) {
  if (!row.value) return
  savingProt.value = true
  try {
    await api.snapshots.setProtected(props.group, props.snap, { protected: v })
    toast.success(v ? 'Snapshot protected' : 'Protection removed', v ? 'Retention and forget skip it.' : 'Retention may remove it at its next run.')
    invalidate(`snapshots:${repos.current}`)
    await refresh()
  } catch (e) {
    toast.error(e, 'Could not change the protection')
  } finally {
    savingProt.value = false
  }
}
const notes = ref('')
const editingNotes = ref(false)
watch(row, (r) => { if (!editingNotes.value) notes.value = r?.notes ?? '' }, { immediate: true })
const savingNotes = ref(false)
async function saveNotes() {
  savingNotes.value = true
  try {
    await api.snapshots.setNotes(props.group, props.snap, { notes: notes.value.trim() })
    toast.success(notes.value.trim() ? 'Notes saved' : 'Notes cleared')
    editingNotes.value = false
    invalidate(`snapshots:${repos.current}`)
    await refresh()
  } catch (e) {
    toast.error(e, 'Could not save the notes')
  } finally {
    savingNotes.value = false
  }
}

// ---- forget
async function forget() {
  const r = row.value
  if (!r) return
  if (r.protected) { toast.warn('This snapshot is protected', 'Unprotect it first; protected snapshots are never forgotten.'); return }
  if (!(await confirm({ title: 'Forget this snapshot?', message: `${r.project}/${r.object} @ ${r.snapshot_id} is removed from the repository for good. Its chunks are freed by the next garbage collection unless other snapshots use them.${next.value ? '' : ' It is the newest snapshot of this object: the next backup reads more.'}`, confirmLabel: 'Forget snapshot', danger: true, typeToConfirm: r.snapshot_id }))) return
  try {
    await api.snapshots.forget(props.group, props.snap)
    toast.success('Snapshot forgotten', 'Its space is reclaimed by the next garbage collection.')
    invalidate(`snapshots:${repos.current}`)
    void router.push(prev.value ? { name: 'snapshot', params: { group: props.group, snap: prev.value.snapshot_id } } : { name: 'snapshots' })
  } catch (e) {
    toast.error(e, 'Could not forget the snapshot')
  }
}

const restoreTo = (action: string) => ({ name: 'restore', query: { action, group: props.group, snap: props.snap } })
const verState = (s: string) => (s === 'ok' ? 'ok' : s === 'failed' ? 'failed' : s)
</script>

<template>
  <div data-testid="snapshot-detail">
    <Skeleton v-if="rows === undefined" height="220px" />
    <Callout v-else-if="snaps.error.value && !row" status="critical" title="Could not load the snapshot" :retry="refresh">{{ snaps.error.value.message }}</Callout>
    <EmptyState v-else-if="!row" title="This snapshot does not exist in this repository" icon="history">
      It may have been pruned or forgotten, or it is in another repository (top bar).
      <template #actions><RouterLink class="btn" :to="{ name: 'snapshots' }">All snapshots</RouterLink></template>
    </EmptyState>
    <template v-else>
      <header class="page-head">
        <div>
          <p class="eyebrow"><PIcon :name="kind === 'block' ? 'hard-drive' : kind === 'instance' ? 'box' : 'folder'" :size="14" />{{ kindLabel(kind) }} snapshot <span class="muted">· {{ position }} of {{ siblings }} of this object</span></p>
          <h1><RouterLink :to="{ name: 'object', params: { group } }" class="objlink">{{ row.project }}/{{ row.object }}</RouterLink></h1>
          <p class="lede"><AbsoluteTime :time="row.created" seconds /> (<RelativeTime :time="row.created" />) on {{ row.host }} · <span class="mono">{{ row.snapshot_id }}</span></p>
        </div>
        <div class="actions">
          <div class="nav">
            <RouterLink v-if="prev" class="btn btn-sm" :to="{ name: 'snapshot', params: { group, snap: prev.snapshot_id } }" data-testid="prev"><PIcon name="chevron-left" :size="14" />Previous</RouterLink>
            <button v-else type="button" class="btn btn-sm" disabled><PIcon name="chevron-left" :size="14" />Previous</button>
            <RouterLink v-if="next" class="btn btn-sm" :to="{ name: 'snapshot', params: { group, snap: next.snapshot_id } }" data-testid="next">Next<PIcon name="chevron-right" :size="14" /></RouterLink>
            <button v-else type="button" class="btn btn-sm" disabled>Next<PIcon name="chevron-right" :size="14" /></button>
          </div>
        </div>
      </header>

      <section class="panel recover">
        <div class="panel-body rbody">
          <div class="rtext">
            <h2>Get data back</h2>
            <p class="muted">Restores never overwrite: they create new objects next to the original.</p>
          </div>
          <div class="racts">
            <template v-if="hasRole('restore')">
              <RouterLink v-if="kind === 'filesystem'" class="btn btn-primary" :to="{ name: 'snapshot-browse', params: { group, snap } }" data-testid="browse"><PIcon name="folder-open" :size="15" />Browse files</RouterLink>
              <RouterLink v-if="kind === 'block'" class="btn btn-primary" :to="restoreTo('guest_files')"><PIcon name="file-search" :size="15" />Recover files…</RouterLink>
              <RouterLink v-if="kind === 'filesystem'" class="btn" :to="restoreTo('fs_extract')"><PIcon name="file-lock" :size="15" />Extract on the host…</RouterLink>
              <RouterLink v-if="isVolume && kind !== 'instance'" class="btn" :to="restoreTo('restore_volume')"><PIcon name="hard-drive-download" :size="15" />Restore as a new volume…</RouterLink>
              <RouterLink v-if="instanceName" class="btn" :to="{ name: 'restore', query: { action: 'restore_instance', instance: instanceName, project: row.project, group: kind === 'instance' ? group : undefined, snap: kind === 'instance' ? snap : undefined } }"><PIcon name="archive-restore" :size="15" />Restore the instance…</RouterLink>
            </template>
            <p v-else class="muted">Browsing and restoring need the restore role.</p>
          </div>
        </div>
      </section>

      <div class="tiles">
        <StatTile label="Size" :value="formatBytes(row.logical_bytes ?? 0)" :hint="kind === 'block' ? 'disk size' : 'logical bytes'" />
        <StatTile label="Changed" :value="change?.dirty !== null && change?.dirty !== undefined ? formatBytes(change.dirty) : '–'" :hint="change?.sizeDelta !== null && change?.sizeDelta !== undefined ? (change.sizeDelta === 0 ? 'same size' : `size ${formatBytes(change.sizeDelta, { signed: true })}`) : prev ? '' : 'first snapshot'" />
        <StatTile label="Uploaded" :value="change?.uploaded !== null && change?.uploaded !== undefined ? formatBytes(change.uploaded) : '–'" :hint="row.stats && row.stats.dirty_bytes ? `${Math.round((1 - row.stats.uploaded_bytes / Math.max(1, row.stats.dirty_bytes)) * 100)} % deduplicated` : ''" />
        <StatTile label="Run took" :value="row.stats?.duration_ms ? formatDuration(row.stats.duration_ms) : '–'" />
        <StatTile label="Read" :value="row.stats ? (row.stats.full_read ? 'Full' : 'Incremental') : '–'" :status="row.stats?.full_read ? 'warn' : undefined" :hint="row.stats ? `${formatBytes(row.stats.read_bytes)} read` : ''" />
        <StatTile label="Integrity" :value="row.verifyClass === 'ok' ? 'Verified' : row.verifyClass === 'failed' ? 'Failed' : row.verifyClass === 'stale' ? 'Stale' : 'Never verified'" :status="row.verifyClass === 'ok' ? 'ok' : row.verifyClass === 'failed' ? 'critical' : 'warn'" :hint="row.verifiedAt ? `verified ${new Date(row.verifiedAt).toISOString().slice(0, 10)}` : ''" />
      </div>
      <Callout v-if="statsMissing" status="info" class="gap">This server does not report run statistics; changed, uploaded and duration stay empty.</Callout>

      <Callout v-if="row.verifyClass === 'failed'" status="critical" title="The last verification found damage" class="gap" data-testid="integrity-failed">
        This snapshot is never used as a base for new backups and is skipped by verified-only sync. {{ myCorrupt.length ? `${myCorrupt.length} quarantined ${noun(myCorrupt.length, 'chunk affects', 'chunks affect')} it (below).` : '' }} If an older or newer snapshot is intact, restore from that one; run a verification after the datastore is repaired.
        <template #actions><RouterLink class="btn btn-sm" :to="{ name: 'verify' }">Verification</RouterLink></template>
      </Callout>

      <div class="grid">
        <section class="panel">
          <header class="panel-head"><h2>Details</h2></header>
          <div class="panel-body">
            <KeyValue :items="items" :columns="1">
              <template #value-created><AbsoluteTime :time="row.created" seconds /></template>
              <template #value-committed><AbsoluteTime :time="row.committed" seconds /></template>
              <template #value-base>
                <RouterLink v-if="row.stats?.previous_snapshot_id && prev && prev.snapshot_id === row.stats.previous_snapshot_id" :to="{ name: 'snapshot', params: { group, snap: prev.snapshot_id } }" class="mono">{{ row.stats.previous_snapshot_id }}</RouterLink>
                <span v-else-if="row.stats?.previous_snapshot_id" class="mono">{{ row.stats.previous_snapshot_id }} <span class="muted">(no longer kept)</span></span>
                <span v-else>{{ row.stats ? 'nothing: a full read' : '–' }}</span>
              </template>
            </KeyValue>
          </div>
        </section>

        <div class="col">
          <section class="panel">
            <header class="panel-head"><h2>Protection and notes</h2></header>
            <div class="panel-body stack">
              <div class="prot">
                <div>
                  <p class="t"><PIcon :name="row.protected ? 'lock' : 'lock-open'" :size="15" /> {{ row.protected ? 'Protected' : 'Not protected' }}</p>
                  <p class="muted small">A protected snapshot is never removed by retention or forget. Use it for the state before an upgrade, or for legal holds.</p>
                </div>
                <Tooltip v-if="!canEdit" text="Changing protection needs the operator role."><SwitchInput :model-value="!!row.protected" disabled label="Protected" /></Tooltip>
                <SwitchInput v-else :model-value="!!row.protected" :disabled="savingProt" label="Protected" data-testid="protect-switch" @update:model-value="setProtected" />
              </div>
              <div>
                <label for="snap-notes" class="lbl">Notes</label>
                <textarea id="snap-notes" v-model="notes" class="textarea" rows="3" :disabled="!canEdit" placeholder="Why this snapshot matters (visible to everyone who can see it)" data-testid="notes" @input="editingNotes = true" />
                <div v-if="canEdit && notes.trim() !== (row.notes ?? '')" class="row nacts">
                  <button type="button" class="btn btn-sm btn-primary" :disabled="savingNotes" data-testid="notes-save" @click="saveNotes">Save notes</button>
                  <button type="button" class="btn btn-sm btn-ghost" @click="notes = row.notes ?? ''; editingNotes = false">Discard</button>
                </div>
              </div>
            </div>
          </section>

          <section class="panel">
            <header class="panel-head"><h2>Integrity</h2><VerifyBadge :state="row.verifyClass" :at="row.verifiedAt" /></header>
            <div class="panel-body">
              <div v-if="myCorrupt.length" class="corrupt">
                <p class="t">{{ myCorrupt.length }} quarantined {{ noun(myCorrupt.length, 'chunk') }}</p>
                <ul><li v-for="c in myCorrupt" :key="c.chunk_id"><span class="mono">{{ c.chunk_id.slice(0, 16) }}…</span> {{ c.detail }} <span class="muted">· <RelativeTime :time="c.detected_at" /></span></li></ul>
              </div>
              <Skeleton v-if="ver.loading.value" height="60px" />
              <Callout v-else-if="ver.error.value" status="critical" :retry="ver.refresh">{{ ver.error.value.message }}</Callout>
              <p v-else-if="!list(ver.data.value).length" class="muted">No verification has read this snapshot yet. The repository's verification schedule picks up new snapshots; an operator can start one on the Verification page.</p>
              <table v-else class="vt" data-testid="verifications">
                <thead><tr><th>When</th><th>Result</th><th class="r">Chunks</th><th>Problems</th></tr></thead>
                <tbody>
                  <tr v-for="v in list(ver.data.value)" :key="v.at"><td><RelativeTime :time="v.at" /></td><td><StatusPill :state="verState(v.state)" size="sm" /></td><td class="r num">{{ formatNumber(v.chunks) }}</td><td class="probs">{{ (v.problems ?? []).join('; ') || '–' }}</td></tr>
                </tbody>
              </table>
            </div>
          </section>

          <section v-if="list(tests.data.value).length" class="panel">
            <header class="panel-head"><h2>Restore tests of this object</h2><RouterLink class="btn btn-sm btn-ghost" :to="{ name: 'restore-tests' }">All<PIcon name="chevron-right" :size="13" /></RouterLink></header>
            <div class="panel-body">
              <ul class="rt">
                <li v-for="t in list(tests.data.value)" :key="t.id"><StatusPill :state="t.state" size="sm" /> {{ t.method }} of <span class="mono">{{ t.snapshot_id }}</span> <span class="muted">· <RelativeTime :time="t.finished ?? t.started" /></span></li>
              </ul>
            </div>
          </section>

          <section v-if="hasRole('admin')" class="panel danger">
            <header class="panel-head"><h2>Forget</h2></header>
            <div class="panel-body dz">
              <p class="muted small">Removes this restore point from the repository for good.{{ row.protected ? ' It is protected: unprotect it first.' : '' }}</p>
              <button type="button" class="btn btn-danger" :disabled="!!row.protected" data-testid="forget" @click="forget"><PIcon name="trash-2" :size="15" />Forget snapshot…</button>
            </div>
          </section>
        </div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.eyebrow { display: flex; align-items: center; gap: 6px; font-size: var(--fs-sm); color: var(--text-2); margin-bottom: 2px; }
.objlink { color: var(--text); text-decoration: none; }
.objlink:hover { text-decoration: underline; }
.mono { font-family: var(--font-mono); font-size: 0.95em; overflow-wrap: anywhere; }
.nav { display: flex; gap: var(--s-1); }
.recover { margin-bottom: var(--s-4); }
.rbody { display: flex; align-items: center; gap: var(--s-4); flex-wrap: wrap; }
.rtext h2 { font-size: var(--fs-md); }
.rtext p { font-size: var(--fs-sm); }
.racts { display: flex; gap: var(--s-2); flex-wrap: wrap; margin-left: auto; }
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: var(--s-3); margin-bottom: var(--s-4); }
.gap { margin-bottom: var(--s-4); }
.grid { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: var(--s-4); align-items: start; }
@media (max-width: 1000px) { .grid { grid-template-columns: minmax(0, 1fr); } }
.col { display: flex; flex-direction: column; gap: var(--s-4); min-width: 0; }
.prot { display: flex; gap: var(--s-4); align-items: flex-start; justify-content: space-between; }
.t { font-weight: var(--fw-medium); display: flex; align-items: center; gap: 6px; }
.small { font-size: var(--fs-sm); }
.lbl { display: block; font-weight: var(--fw-medium); margin-bottom: 4px; }
.textarea { width: 100%; }
.nacts { margin-top: var(--s-2); }
.vt { width: 100%; border-collapse: collapse; font-size: var(--fs-sm); }
.vt th { text-align: left; color: var(--text-muted); font-weight: var(--fw-medium); padding: 4px 6px; border-bottom: 1px solid var(--border); }
.vt td { padding: 6px; border-bottom: 1px solid var(--border); vertical-align: top; }
.vt .r { text-align: right; }
.probs { overflow-wrap: anywhere; color: var(--critical-text); }
.corrupt { background: var(--critical-bg); border: 1px solid var(--critical-border); border-radius: var(--r); padding: var(--s-3); margin-bottom: var(--s-3); font-size: var(--fs-sm); }
.corrupt ul { margin: 4px 0 0; padding-left: 18px; }
.rt { list-style: none; margin: 0; padding: 0; display: grid; gap: 6px; font-size: var(--fs-sm); }
.dz { display: flex; gap: var(--s-3); align-items: center; justify-content: space-between; flex-wrap: wrap; }
.danger { border-color: var(--critical-border); }
</style>
