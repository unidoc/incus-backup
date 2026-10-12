<script setup lang="ts">
// Hosts and sync peers enrolled in this repository: who may write (backup), who may sync from/into it,
// when they were last seen, what they protect, and disabling an identity (certificate revoked until enabled).
import { computed } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import EmptyState from '@/components/EmptyState.vue'
import { list } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import type { RepoApi } from '@/api/sections/storage'
import { selectRepo } from '@/stores/repos'
import { hasRole } from '@/stores/session'
import { toast } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import { shortId } from '@/lib/format'
import type { ColumnDef } from '@/lib/table'
import type { HostRow, HostStatus, RepoView } from '@/api/types'

const props = defineProps<{ repo: RepoView; api: RepoApi }>()
const router = useRouter()
const hosts = useResource(() => `hosts:${props.repo.name}`, (s) => props.api.hosts(s), { refreshOn: ['host'] })
const status = useResource(() => `status:${props.repo.name}`, (s) => props.api.status(s), { refreshOn: ['run', 'host'] })
interface Row extends HostRow { st?: HostStatus; key: string }
const rows = computed<Row[]>(() => list(hosts.data.value).map((h) => ({ ...h, key: `${h.name}/${h.scope}`, st: h.scope === 'backup' ? (status.data.value?.hosts ?? []).find((x) => x.name === h.name) : undefined })))
const online = (h: Row) => !!h.last_seen && Date.now() - Date.parse(h.last_seen) < 26 * 3_600_000
const scopeLabel: Record<string, string> = { backup: 'Backup host', 'sync-read': 'Sync peer (reads)', 'sync-write': 'Sync peer (writes)' }
const cols: ColumnDef<Row>[] = [
  { key: 'name', label: 'Name', required: true },
  { key: 'scope', label: 'Role', format: (h) => scopeLabel[h.scope] ?? h.scope },
  { key: 'state', label: 'State', value: (h) => (h.disabled ? 'disabled' : online(h) ? 'online' : 'offline') },
  { key: 'last_seen', label: 'Last seen', value: (h) => h.last_seen ?? '' },
  { key: 'objects', label: 'Objects', align: 'right', value: (h) => h.st?.objects ?? null },
  { key: 'protected', label: 'Protected', align: 'right', value: (h) => (h.st ? `${h.st.protected} / ${h.st.objects}` : '') },
  { key: 'last_run', label: 'Last backup run', value: (h) => h.st?.last_run_ok ?? '' },
  { key: 'last_addr', label: 'Address', mono: true, hidden: true },
  { key: 'fingerprint', label: 'Certificate', mono: true, hidden: true, format: (h) => shortId(h.fingerprint, 16) },
  { key: 'enrolled', label: 'Enrolled', hidden: true },
  { key: 'actions', label: '', sortable: false, noExport: true },
]
async function toggle(h: Row) {
  const enable = h.disabled
  if (!(await confirm({ title: `${enable ? 'Enable' : 'Disable'} ${h.name} (${h.scope})?`, danger: !enable, confirmLabel: enable ? 'Enable' : 'Disable',
    message: enable ? 'Its certificate is accepted again from the next request.' : h.scope === 'backup' ? 'Its certificate is refused immediately: backups from this host stop until you enable it again. Its snapshots stay.' : 'The sync job using this identity fails until you enable it again.' }))) return
  try {
    await props.api.setHostEnabled(h.name, h.scope, enable)
    toast.success(`${h.name} ${enable ? 'enabled' : 'disabled'}`)
    invalidate(`hosts:${props.repo.name}`)
    invalidate('hosts:')
    await hosts.refresh()
  } catch (e) { toast.error(e, `Could not ${enable ? 'enable' : 'disable'} ${h.name}`) }
}
function openHost(name: string) { selectRepo(props.repo.name); void router.push({ name: 'host', params: { name } }) }
</script>

<template>
  <section class="panel">
    <div class="panel-head"><h2>Hosts and peers</h2><span class="sub">identities enrolled in {{ repo.name }}</span><span class="spacer" />
      <RouterLink v-if="hasRole('admin')" :to="{ name: 'host-onboard' }" class="btn btn-sm" @click="selectRepo(repo.name)"><Icon name="plus" :size="13" />Add a host</RouterLink></div>
    <DataTable :rows="hosts.data.value === undefined ? undefined : rows" :columns="cols" row-key="key" storage-key="repo-hosts" csv-name="hosts" :loading="hosts.loading.value" :error="hosts.error.value" :retry="hosts.refresh" dense>
      <template #cell-name="{ row }"><button v-if="row.scope === 'backup'" type="button" class="link strong" @click="openHost(row.name)">{{ row.name }}</button><span v-else class="strong">{{ row.name }}</span></template>
      <template #cell-state="{ row }"><StatusPill :state="row.disabled ? 'disabled' : online(row) ? 'online' : 'offline'" size="sm" /></template>
      <template #cell-last_seen="{ row }"><RelativeTime :time="row.last_seen" /></template>
      <template #cell-protected="{ row }">
        <span v-if="row.st" :class="{ warnv: row.st.protected < row.st.objects }" :title="row.st.unprotected?.join(', ') ?? ''">{{ row.st.protected }} / {{ row.st.objects }}</span>
      </template>
      <template #cell-last_run="{ row }"><StatusPill v-if="row.st?.last_run_ok" :state="row.st.last_run_ok" variant="dot" /><span v-if="row.st?.last_run_at" class="muted small"> <RelativeTime :time="row.st.last_run_at" /></span></template>
      <template #cell-enrolled="{ row }"><RelativeTime :time="row.enrolled" /></template>
      <template #cell-actions="{ row }">
        <button v-if="hasRole('operator')" type="button" class="btn btn-sm" :class="{ 'btn-ghost': !row.disabled }" @click="toggle(row)">{{ row.disabled ? 'Enable' : 'Disable' }}</button>
      </template>
      <template #empty>
        <EmptyState title="No host enrolled yet" icon="server">Enrol an Incus host to start backing up into {{ repo.name }}.
          <template v-if="hasRole('admin')" #actions><RouterLink :to="{ name: 'host-onboard' }" class="btn btn-primary btn-sm" @click="selectRepo(repo.name)"><Icon name="plus" :size="14" />Add a host</RouterLink></template>
        </EmptyState>
      </template>
    </DataTable>
  </section>
</template>

<style scoped>
.panel > :deep(.dt) { border: 0; border-radius: 0 0 var(--r-lg) var(--r-lg); }
.link { border: 0; background: none; padding: 0; color: var(--accent-text); cursor: pointer; font: inherit; }
.link:hover { text-decoration: underline; }
.strong { font-weight: var(--fw-semibold); }
.warnv { color: var(--warn-text); font-weight: var(--fw-medium); }
</style>
