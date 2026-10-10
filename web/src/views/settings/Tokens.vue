<script setup lang="ts">
// The administrator's overview of automation access: every API token of every user, what it can do
// now (the lower of its role and its owner's), when it was last used; revoke any of them. Tokens are
// created by their owner in the profile (the secret is shown once to that person only).
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import EmptyState from '@/components/EmptyState.vue'
import CodeBlock from '@/components/CodeBlock.vue'
import Tabs from '@/components/Tabs.vue'
import { useResource } from '@/api/cache'
import { cfg, list, type AdminToken } from '@/api/sections/config'
import { toast } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import type { ColumnDef } from '@/lib/table'
import '@/components/config/section.css'

const tokens = useResource(() => 'cfg:api-tokens', (s) => cfg.tokens.list(s))
const rows = computed(() => list(tokens.data.value))
const stateOf = (t: AdminToken) => (t.expired ? 'expired' : t.owner_disabled ? 'owner disabled' : t.effective_role !== t.role ? 'limited' : 'active')
const unused = computed(() => rows.value.filter((t) => !t.expired && !t.owner_disabled && (!t.last_used_at || Date.now() - Date.parse(t.last_used_at) > 90 * 86400e3)).length)
const adminTokens = computed(() => rows.value.filter((t) => t.effective_role === 'admin').length)

const cols: ColumnDef<AdminToken>[] = [
  { key: 'name', label: 'Token', required: true },
  { key: 'owner', label: 'Owner', width: '130px' },
  { key: 'effective_role', label: 'Can act as', width: '150px', value: (t) => t.effective_role || 'nothing' },
  { key: 'state', label: 'State', width: '140px', value: stateOf },
  { key: 'last_used_at', label: 'Last used', width: '120px' },
  { key: 'created_at', label: 'Created', width: '120px' },
  { key: 'expires_at', label: 'Expires', width: '120px' },
  { key: 'id', label: 'ID', mono: true, hidden: true },
  { key: 'actions', label: '', sortable: false, noExport: true, align: 'right', width: '90px', required: true },
]
async function revoke(t: AdminToken) {
  if (!(await confirm({ title: `Revoke “${t.name}” of ${t.owner}?`, message: 'Scripts and integrations using it get 401 at once. The owner can create a new one in their profile.', danger: true, typeToConfirm: t.name, confirmLabel: 'Revoke token' }))) return
  try { await cfg.tokens.revoke(t.id); toast.success('Token revoked', `${t.owner}/${t.name} no longer works.`); void tokens.refresh() } catch (e) { toast.error(e, 'Could not revoke the token') }
}
const base = `${location.origin}/api/v1`
const curl = [
  `# read: the dashboard data (any role)`,
  `curl -fsS -H "Authorization: Bearer $IB_TOKEN" ${base}/overview`,
  ``,
  `# open alerts, critical only`,
  `curl -fsS -H "Authorization: Bearer $IB_TOKEN" "${base}/alerts?active=1&severity=critical"`,
  ``,
  `# operator: verify the newest snapshots of a repository`,
  `curl -fsS -X POST -H "Authorization: Bearer $IB_TOKEN" -H "Content-Type: application/json" \\`,
  `  -d '{"mode":"new"}' "${base}/verify?repository=custody"`,
].join('\n')
const tabs = [{ id: 'users', label: 'Users', to: { name: 'users' } }, { id: 'tokens', label: 'API tokens', to: { name: 'tokens' } }]
</script>

<template>
  <div>
    <header class="page-head">
      <div>
        <h1>API tokens</h1>
        <p class="lede">Automation access: every token of every user, what it can do right now, and when it was last used.</p>
      </div>
      <div class="actions"><RouterLink :to="{ name: 'profile' }" class="btn"><Icon name="plus" :size="14" />Create your own token</RouterLink></div>
    </header>
    <Tabs :tabs="tabs" label="Users and tokens" class="subnav" />

    <div class="cx-grid2 top">
      <section class="panel">
        <div class="panel-head"><h2>How tokens work</h2></div>
        <div class="panel-body cx-help">
          <p>A token acts as its owner, with the token's role or the owner's current role, whichever is lower. Demoting or disabling a user limits or stops their tokens at once.</p>
          <p>Tokens need no CSRF header and no session; send them as <code>Authorization: Bearer ibt_…</code>. The secret is shown once, to its owner, when it is created in the profile. The server keeps only a hash.</p>
          <p>Give each script its own token with the smallest role it needs, and an expiry. Readonly reads everything but credentials; operator can run jobs; admin can delete data.</p>
          <p v-if="adminTokens" class="warnp"><strong>{{ adminTokens }} token{{ adminTokens === 1 ? '' : 's' }} can act as administrator.</strong> Check that each one needs it.</p>
          <p v-if="unused">{{ unused }} usable token{{ unused === 1 ? ' was' : 's were' }} not used for 90 days or never: candidates for revoking.</p>
        </div>
      </section>
      <section class="panel">
        <div class="panel-head"><h2>Use one</h2><span class="sub">curl</span></div>
        <div class="panel-body"><CodeBlock :code="curl" label="Examples" /></div>
      </section>
    </div>

    <DataTable :rows="rows" :columns="cols" row-key="id" :loading="tokens.loading.value" :error="tokens.error.value" :retry="tokens.refresh" storage-key="cfg-api-tokens" csv-name="api-tokens" data-testid="token-table">
      <template #cell-name="{ row }"><strong>{{ row.name }}</strong></template>
      <template #cell-effective_role="{ row }">
        <span v-if="row.effective_role" class="cx-tag" :class="{ crit: row.effective_role === 'admin' }">{{ row.effective_role }}</span><span v-else class="muted">nothing</span>
        <span v-if="row.effective_role && row.effective_role !== row.role" class="muted small" :title="`The token has ${row.role}; its owner only ${row.owner_role}`"> (token: {{ row.role }})</span>
      </template>
      <template #cell-state="{ row }"><StatusPill :status="row.expired || row.owner_disabled ? 'neutral' : row.effective_role !== row.role ? 'warn' : 'ok'" :label="stateOf(row)" size="sm" /></template>
      <template #cell-last_used_at="{ row }"><RelativeTime :time="row.last_used_at" /></template>
      <template #cell-created_at="{ row }"><AbsoluteTime :time="row.created_at" /></template>
      <template #cell-expires_at="{ row }"><AbsoluteTime v-if="row.expires_at" :time="row.expires_at" /><span v-else class="muted">never</span></template>
      <template #cell-actions="{ row }"><button type="button" class="btn btn-sm btn-ghost" :data-testid="`token-revoke-${row.name}`" @click="revoke(row)">Revoke</button></template>
      <template #empty><EmptyState title="No API tokens" icon="key-round">Nothing uses the API with a token. Create one in your profile for a script or a monitoring system.</EmptyState></template>
    </DataTable>
  </div>
</template>

<style scoped>
.subnav { margin-bottom: 16px; }
.top { margin-bottom: 16px; }
.warnp { color: var(--warn-text); }
</style>
