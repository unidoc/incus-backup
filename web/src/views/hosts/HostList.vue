<script setup lang="ts">
// Hosts: every enrolled identity (backup hosts and sync peers) of every repository, with what an
// operator checks first: online, protection, last run, certificate. Filter chips for the states that
// need action, bulk enable/disable for operators, quick actions per row.
import { computed, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import StatTile from '@/components/StatTile.vue'
import EmptyState from '@/components/EmptyState.vue'
import Callout from '@/components/Callout.vue'
import Tooltip from '@/components/Tooltip.vue'
import FilterChips from '@/components/infra/FilterChips.vue'
import RowMenu, { type MenuItem } from '@/components/infra/RowMenu.vue'
import BackupNowDialog from '@/components/infra/BackupNowDialog.vue'
import { useHostRows, type HostListRow } from './hostRows'
import { invalidate } from '@/api/cache'
import { infraApi } from '@/api/sections/infra'
import { confirm } from '@/composables/useConfirm'
import { toast, errorText } from '@/stores/toast'
import { hasRole } from '@/stores/session'
import { repos, repoMany } from '@/stores/repos'
import { certStatus } from '@/lib/infra'
import { plural } from '@/lib/format'
import type { ColumnDef } from '@/lib/table'
import type { ObjectRow } from '@/api/types'

const router = useRouter()
const { ov, ids, certs, rows, refresh } = useHostRows()
const canOperate = computed(() => hasRole('operator'))
const isAdmin = computed(() => hasRole('admin'))

// ---- filters
const view = ref('all')
const scope = ref('all')
const repo = ref('all')
const base = computed(() => (rows.value ?? []).filter((r) => (scope.value === 'all' || r.scope === scope.value) && (repo.value === 'all' || r.repository === repo.value)))
const tests: Record<string, (r: HostListRow) => boolean> = {
  all: () => true,
  attention: (r) => r.attention,
  online: (r) => r.state === 'online',
  offline: (r) => r.state === 'offline',
  disabled: (r) => r.disabled,
  cert: (r) => !!r.cert && r.cert.state !== 'ok',
}
const count = (k: string) => base.value.filter(tests[k]).length
const chips = computed(() => [
  { value: 'all', label: 'All', count: base.value.length },
  { value: 'attention', label: 'Needs attention', count: count('attention'), status: 'warn' as const },
  { value: 'online', label: 'Online', count: count('online') },
  { value: 'offline', label: 'Offline', count: count('offline'), status: 'critical' as const },
  { value: 'disabled', label: 'Disabled', count: count('disabled') },
  { value: 'cert', label: 'Certificate', count: count('cert'), status: 'warn' as const },
])
const shown = computed(() => (rows.value ? base.value.filter(tests[view.value] ?? tests.all) : undefined))
const scopes = computed(() => [...new Set((rows.value ?? []).map((r) => r.scope))].sort())

// ---- tiles
const all = computed(() => rows.value ?? [])
const backupHosts = computed(() => all.value.filter((r) => r.scope === 'backup'))
const peers = computed(() => all.value.length - backupHosts.value.length)
const certSoon = computed(() => all.value.filter((r) => r.cert && r.cert.state !== 'ok').length)

const cols = computed<ColumnDef<HostListRow>[]>(() => [
  { key: 'name', label: 'Host', required: true },
  ...(repoMany.value && repo.value === 'all' ? [{ key: 'repository', label: 'Repository' } as ColumnDef<HostListRow>] : []),
  { key: 'scope', label: 'Scope', hidden: true },
  { key: 'state', label: 'State', value: (r) => r.state },
  { key: 'last_seen', label: 'Last seen', value: (r) => r.last_seen ?? '', hidden: true },
  { key: 'objects', label: 'Objects', align: 'right', value: (r) => (r.scope === 'backup' ? r.objects : null) },
  { key: 'kinds', label: 'Instances / disks / filesystems / volumes', hidden: true, value: (r) => (r.scope === 'backup' ? `${r.instances}/${r.disks}/${r.filesystems}/${r.volumes}` : '') },
  { key: 'protection', label: 'Protection', value: (r) => r.protectionLabel },
  { key: 'last_backup', label: 'Last backup', value: (r) => r.last_backup ?? '' },
  { key: 'last_run_status', label: 'Last run', value: (r) => r.last_run_status ?? '', hidden: true },
  { key: 'cert', label: 'Certificate', value: (r) => (r.cert ? `${r.cert.state} ${r.cert.days_left}d` : '') },
  { key: 'last_addr', label: 'Address', hidden: true, mono: true },
  { key: 'fingerprint', label: 'Fingerprint', hidden: true, mono: true, value: (r) => r.fingerprint.slice(0, 16) },
  { key: 'enrolled', label: 'Enrolled', hidden: true },
  { key: 'actions', label: 'Actions', sortable: false, noExport: true, align: 'right', width: '92px', required: true, value: () => '' },
])

// ---- actions
const selected = ref<string[]>([])
const bulkBusy = ref('')
async function setEnabled(targets: HostListRow[], enabled: boolean): Promise<void> {
  if (!targets.length) return
  const verb = enabled ? 'enable' : 'disable'
  if (!enabled) {
    const one = targets.length === 1
    const ok = await confirm({
      title: one ? `Disable ${targets[0].name}?` : `Disable ${targets.length} hosts?`,
      message: `A disabled host can no longer back up, restore or renew its certificate: every request it makes is refused. Nothing is deleted; enabling it again restores access.${one ? '' : ` Hosts: ${targets.map((t) => t.name).join(', ')}.`}`,
      confirmLabel: one ? 'Disable host' : `Disable ${targets.length} hosts`, typeToConfirm: one ? targets[0].name : `disable ${targets.length}`,
    })
    if (!ok) return
  }
  const fails: string[] = []
  let n = 0
  for (const t of targets) {
    bulkBusy.value = `${enabled ? 'Enabling' : 'Disabling'} ${++n} of ${targets.length}…`
    try { await infraApi.setHostEnabled(t.repository, t.name, t.scope, enabled) } catch (e) { fails.push(`${t.name}: ${errorText(e)}`) }
  }
  bulkBusy.value = ''
  invalidate('infra-hosts'); invalidate('overview'); invalidate('certificates')
  await refresh()
  const ok = targets.length - fails.length
  if (ok) toast.success(targets.length === 1 ? `${targets[0].name} ${enabled ? 'enabled' : 'disabled'}` : `${plural(ok, 'host')} ${enabled ? 'enabled' : 'disabled'}`)
  if (fails.length) toast.error(fails.join('\n'), `Could not ${verb} ${plural(fails.length, 'host')}`)
  selected.value = []
}
const byKey = computed(() => new Map((rows.value ?? []).map((r) => [r.key, r])))
const selRows = (keys: string[]) => keys.map((k) => byKey.value.get(k)).filter((x): x is HostListRow => !!x)

const bn = ref<{ open: boolean; row: HostListRow | null; objects: ObjectRow[] }>({ open: false, row: null, objects: [] })
function backupNow(r: HostListRow) {
  bn.value = { open: true, row: r, objects: ids.data.value?.[r.repository]?.objects.filter((o) => o.host === r.name) ?? [] }
}
const hostLink = (r: HostListRow) => ({ name: 'host', params: { name: r.name }, query: { ...(r.repository !== repos.current ? { repository: r.repository } : {}), ...(r.scope !== 'backup' ? { scope: r.scope } : {}) } })
function menu(r: HostListRow): MenuItem[] {
  const opNeed = canOperate.value ? false : 'Needs the operator role'
  const items: MenuItem[] = [{ label: 'Open', icon: 'chevron-right', run: () => router.push(hostLink(r)) }]
  if (r.scope === 'backup') items.push({ label: 'Back up now', icon: 'play', run: () => backupNow(r), disabled: opNeed || (r.disabled ? 'The host is disabled' : false), testid: 'menu-backup-now' })
  if (isAdmin.value) items.push({ label: 'Re-enrol or replace', icon: 'key-round', run: () => router.push({ name: 'host-onboard', query: { host: r.name, scope: r.scope, replace: '1' } }) })
  items.push(r.disabled
    ? { label: 'Enable', icon: 'lock-open', run: () => setEnabled([r], true), disabled: opNeed, testid: 'menu-enable' }
    : { label: 'Disable', icon: 'lock', run: () => setEnabled([r], false), danger: true, disabled: opNeed, testid: 'menu-disable' })
  return items
}
const loadErr = computed(() => ids.error.value ?? ov.error.value)
</script>

<template>
  <div class="hosts-page">
    <header class="page-head">
      <div>
        <h1>Hosts</h1>
        <p class="lede">Incus hosts that back up here, and sync peers. Each connects with its own certificate; disabling one refuses every request it makes.</p>
      </div>
      <div class="actions">
        <button type="button" class="btn btn-sm" :disabled="ids.refreshing.value" @click="refresh()"><Icon name="refresh-cw" :size="13" />Refresh</button>
        <RouterLink v-if="isAdmin" :to="{ name: 'host-onboard' }" class="btn btn-primary" data-testid="add-host"><Icon name="plus" :size="14" />Add a host</RouterLink>
      </div>
    </header>

    <div class="tiles" data-testid="host-tiles">
      <StatTile label="Backup hosts" :value="backupHosts.length" :loading="!rows" :hint="peers ? plural(peers, 'sync peer') : 'no sync peers'" />
      <StatTile label="Online" :value="backupHosts.filter((r) => r.state === 'online').length" :loading="!rows" :hint="`of ${backupHosts.filter((r) => !r.disabled).length} enabled`" />
      <StatTile label="Offline" :value="backupHosts.filter((r) => r.state === 'offline').length" :loading="!rows" :status="backupHosts.some((r) => r.state === 'offline') ? 'critical' : undefined" hint="backup hosts not seen for 6 hours" />
      <StatTile label="Needs attention" :value="all.filter((r) => r.attention).length" :loading="!rows" :status="all.some((r) => r.attention) ? 'warn' : undefined" hint="offline, overdue, failed or certificate" />
      <StatTile label="Certificates" :value="certSoon" :loading="!rows && certs.loading.value" :status="certSoon ? 'warn' : undefined" :hint="certSoon ? 'expiring, expired or untrusted' : 'all valid'" />
    </div>

    <Callout v-if="certs.error.value" status="warn" title="Certificate states are not shown">{{ certs.error.value.message }}</Callout>

    <div class="filters">
      <FilterChips v-model="view" :options="chips" label="Show hosts" data-testid="host-chips" />
      <span class="spacer" />
      <label class="sel"><span class="sr-only">Scope</span>
        <select v-model="scope" class="select" data-testid="scope-filter"><option value="all">All scopes</option><option v-for="s in scopes" :key="s" :value="s">{{ s }}</option></select>
      </label>
      <label v-if="repoMany" class="sel"><span class="sr-only">Repository</span>
        <select v-model="repo" class="select" data-testid="repo-filter"><option value="all">All repositories</option><option v-for="r in repos.list" :key="r.name" :value="r.name">{{ r.name }}</option></select>
      </label>
    </div>

    <Callout v-if="bulkBusy" status="info" :title="bulkBusy" />

    <DataTable :rows="shown" :columns="cols" row-key="key" :loading="ids.loading.value" :error="loadErr" :retry="refresh" storage-key="hosts" csv-name="hosts"
      :selectable="canOperate" v-model:selected="selected" filter-placeholder="Filter hosts (name, address, scope)" :initial-sort="{ key: 'name', dir: 'asc' }" data-testid="host-table">
      <template #bulk="{ selected: keys }">
        <button type="button" class="btn btn-sm" :disabled="!!bulkBusy" data-testid="bulk-enable" @click="setEnabled(selRows(keys), true)"><Icon name="lock-open" :size="13" />Enable</button>
        <button type="button" class="btn btn-sm btn-danger" :disabled="!!bulkBusy" data-testid="bulk-disable" @click="setEnabled(selRows(keys), false)"><Icon name="lock" :size="13" />Disable</button>
      </template>
      <template #cell-name="{ row }">
        <span class="hn">
          <Icon :name="row.scope === 'backup' ? 'server' : 'refresh-cw'" :size="14" class="hi" />
          <RouterLink :to="hostLink(row)" class="strong" :data-host="row.name">{{ row.name }}</RouterLink>
          <span v-if="row.scope !== 'backup'" class="scope" :title="row.scope === 'sync-read' ? 'Sync peer that pulls copies from this repository' : 'Sync peer that pushes copies into this repository'">{{ row.scope }}</span>
        </span>
      </template>
      <template #cell-scope="{ row }"><span class="scope">{{ row.scope }}</span></template>
      <template #cell-state="{ row }"><span class="stc"><StatusPill :state="row.state" size="sm" /><RelativeTime :time="row.last_seen" class="muted small" /></span></template>
      <template #cell-last_seen="{ row }"><RelativeTime :time="row.last_seen" /></template>
      <template #cell-objects="{ row }">
        <Tooltip v-if="row.scope === 'backup'" :text="`${row.instances} instances, ${row.disks} VM disks, ${row.filesystems} container filesystems, ${row.volumes} custom volumes`">
          <span class="counts num"><b>{{ row.objects }}</b><span class="kc"><Icon name="box" :size="12" />{{ row.instances }}</span><span class="kc"><Icon name="hard-drive" :size="12" />{{ row.disks }}</span><span class="kc"><Icon name="folder" :size="12" />{{ row.filesystems }}</span><span class="kc"><Icon name="database" :size="12" />{{ row.volumes }}</span></span>
        </Tooltip>
        <span v-else class="muted">–</span>
      </template>
      <template #cell-protection="{ row }">
        <StatusPill v-if="row.protection" :status="row.protectionStatus" :label="row.protectionLabel" size="sm" variant="dot" />
        <span v-else class="muted">–</span>
      </template>
      <template #cell-last_backup="{ row }">
        <span v-if="row.scope === 'backup'" class="stc"><RelativeTime :time="row.last_backup" /><StatusPill v-if="row.last_run_status" :state="row.last_run_status" variant="dot" :label="`last run ${row.last_run_status}`" size="sm" class="small" /></span>
        <span v-else class="muted">–</span>
      </template>
      <template #cell-last_run_status="{ row }"><StatusPill v-if="row.last_run_status" :state="row.last_run_status" variant="dot" /><span v-else class="muted">–</span></template>
      <template #cell-cert="{ row }">
        <Tooltip v-if="row.cert" :text="`Expires ${row.cert.not_after.slice(0, 10)}, in ${row.cert.days_left} days. Hosts renew automatically 45 days before.`">
          <StatusPill :status="certStatus(row.cert.state)" :label="row.cert.state === 'ok' ? `${row.cert.days_left} d left` : `${row.cert.state}, ${row.cert.days_left} d`" size="sm" variant="dot" />
        </Tooltip>
        <span v-else class="muted">–</span>
      </template>
      <template #cell-enrolled="{ row }"><RelativeTime :time="row.enrolled" /></template>
      <template #cell-actions="{ row }">
        <span class="acts">
          <button v-if="row.scope === 'backup' && canOperate" type="button" class="btn btn-sm btn-ghost btn-icon" :disabled="row.disabled" :aria-label="`Back up ${row.name} now`" :title="row.disabled ? 'The host is disabled' : 'Back up now'" data-testid="row-backup-now" @click="backupNow(row)"><Icon name="play" :size="14" /></button>
          <RowMenu :items="menu(row)" :label="`Actions for ${row.name}`" />
        </span>
      </template>
      <template #empty>
        <EmptyState title="No host is enrolled yet" icon="server">A host enrols once with a one-time token, then backs up on its own schedule.
          <template v-if="isAdmin" #actions><RouterLink :to="{ name: 'host-onboard' }" class="btn btn-primary btn-sm"><Icon name="plus" :size="14" />Add a host</RouterLink></template>
        </EmptyState>
      </template>
    </DataTable>

    <BackupNowDialog v-if="bn.row" v-model:open="bn.open" :repository="bn.row.repository" :host="bn.row.name" :objects="bn.objects" :offline="bn.row.state === 'offline'" />
  </div>
</template>

<style scoped>
.hosts-page { display: flex; flex-direction: column; gap: 14px; }
.page-head { margin-bottom: 0; }
.tiles { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: 10px; }
.filters { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.sel .select { height: var(--control-h-sm); font-size: var(--fs-sm); width: auto; min-width: 140px; }
.hn { display: inline-flex; align-items: center; gap: 7px; min-width: 0; white-space: nowrap; }
.stc { display: inline-flex; flex-direction: column; align-items: flex-start; gap: 1px; line-height: 1.2; padding: 3px 0; }
.hi { color: var(--text-muted); }
.strong { font-weight: var(--fw-medium); color: var(--text); }
.scope { font-size: var(--fs-xs); padding: 1px 6px; border-radius: var(--r-sm); background: var(--surface-2); border: 1px solid var(--border); color: var(--text-2); white-space: nowrap; }
.counts { display: inline-flex; gap: 8px; align-items: center; justify-content: flex-end; white-space: nowrap; }
.kc { display: inline-flex; align-items: center; gap: 2px; color: var(--text-muted); font-size: var(--fs-sm); }
.acts { display: inline-flex; gap: 2px; justify-content: flex-end; }
@media (max-width: 1100px) { .tiles { grid-template-columns: repeat(3, minmax(0, 1fr)); } }
@media (max-width: 600px) { .tiles { grid-template-columns: repeat(2, minmax(0, 1fr)); } .sel, .sel .select { width: 100%; } }
</style>
