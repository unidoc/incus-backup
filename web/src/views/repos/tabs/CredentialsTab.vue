<script setup lang="ts">
// S3 credentials (admin): stored sealed on the server, listed by name and access key id only. Rotate
// replaces the key pair of a credential in place (every repository using it switches immediately);
// delete only works for a credential no repository uses. Secrets live in this component's memory
// while a dialog is open and are cleared after submit or cancel.
import { computed, reactive, ref } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import EmptyState from '@/components/EmptyState.vue'
import Modal from '@/components/Modal.vue'
import FormField from '@/components/form/FormField.vue'
import PasswordInput from '@/components/form/PasswordInput.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import { api, list } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import { toast, errorText } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import type { ColumnDef } from '@/lib/table'
import type { Credential, RepoView } from '@/api/types'

const props = defineProps<{ repo: RepoView }>()
const creds = useResource(() => 'credentials', (s) => api.credentials.list(s))
const rows = computed(() => list(creds.data.value))
const cols: ColumnDef<Credential>[] = [
  { key: 'name', label: 'Name', required: true },
  { key: 'kind', label: 'Kind' },
  { key: 'access_key', label: 'Access key id', mono: true },
  { key: 'used_by', label: 'Used by', value: (c) => c.used_by.join(', ') },
  { key: 'created_at', label: 'Created' },
  { key: 'rotated_at', label: 'Rotated', value: (c) => c.rotated_at ?? '' },
  { key: 'actions', label: '', sortable: false, noExport: true },
]

const dlg = reactive({ open: false, mode: 'rotate' as 'rotate' | 'add', name: '', access: '', busy: false, error: '' })
const secret = ref('')
function openRotate(c: Credential) { Object.assign(dlg, { open: true, mode: 'rotate', name: c.name, access: c.access_key, busy: false, error: '' }); secret.value = '' }
function openAdd() { Object.assign(dlg, { open: true, mode: 'add', name: '', access: '', busy: false, error: '' }); secret.value = '' }
function close() { dlg.open = false; secret.value = ''; dlg.access = '' }
const nameOk = computed(() => dlg.mode === 'rotate' || /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(dlg.name))
async function submit() {
  if (!dlg.access.trim() || !secret.value || !nameOk.value) return
  dlg.busy = true
  dlg.error = ''
  try {
    if (dlg.mode === 'rotate') await api.credentials.rotate(dlg.name, { access_key: dlg.access.trim(), secret_key: secret.value })
    else await api.credentials.create({ name: dlg.name.trim(), access_key: dlg.access.trim(), secret_key: secret.value })
    toast.success(dlg.mode === 'rotate' ? `Credential ${dlg.name} rotated` : `Credential ${dlg.name} stored`, dlg.mode === 'rotate' ? 'Repositories using it switched to the new key. Revoke the old key at your provider once backups run.' : 'Use it when you add an S3 repository.')
    close()
    invalidate('credentials')
    await creds.refresh()
  } catch (e) {
    dlg.error = errorText(e)
  } finally { dlg.busy = false }
}
async function remove(c: Credential) {
  if (c.used_by.length) return
  if (!(await confirm({ title: `Delete credential ${c.name}?`, danger: true, typeToConfirm: c.name, confirmLabel: 'Delete', message: `The sealed key pair (access key ${c.access_key}) is removed from this server. It does not revoke the key at your provider.` }))) return
  try {
    await api.credentials.remove(c.name)
    toast.success(`Credential ${c.name} deleted`)
    invalidate('credentials')
    await creds.refresh()
  } catch (e) { toast.error(e, 'Could not delete the credential') }
}
</script>

<template>
  <section class="panel">
    <div class="panel-head"><h2>S3 credentials</h2><span class="sub">server-wide; secrets are sealed and never shown again</span><span class="spacer" />
      <button type="button" class="btn btn-sm" data-testid="cred-add" @click="openAdd"><Icon name="plus" :size="13" />Store a credential</button></div>
    <DataTable :rows="creds.data.value === undefined ? undefined : rows" :columns="cols" row-key="name" storage-key="credentials" csv-name="credentials" :loading="creds.loading.value" :error="creds.error.value" :retry="creds.refresh" dense>
      <template #cell-name="{ row }"><span class="strong">{{ row.name }}</span> <StatusPill v-if="row.used_by.includes(repo.name)" status="info" :label="`used by ${repo.name}`" size="sm" /></template>
      <template #cell-used_by="{ row }"><span v-if="row.used_by.length">{{ row.used_by.join(', ') }}</span><span v-else class="muted">unused</span></template>
      <template #cell-created_at="{ row }"><RelativeTime :time="row.created_at" /></template>
      <template #cell-rotated_at="{ row }"><RelativeTime :time="row.rotated_at" never="never" /></template>
      <template #cell-actions="{ row }">
        <span class="acts">
          <button type="button" class="btn btn-sm" :data-testid="`cred-rotate-${row.name}`" @click="openRotate(row)"><Icon name="refresh-cw" :size="12" />Rotate</button>
          <button type="button" class="btn btn-sm btn-ghost" :disabled="row.used_by.length > 0" :title="row.used_by.length ? `Used by ${row.used_by.join(', ')}: a repository in use cannot lose its credential` : 'Delete'" @click="remove(row)"><Icon name="trash-2" :size="12" />Delete</button>
        </span>
      </template>
      <template #empty><EmptyState title="No S3 credential stored" icon="key-round" compact>Credentials are created with an S3 repository, or stored here first and picked when adding one.</EmptyState></template>
    </DataTable>
  </section>

  <Modal :open="dlg.open" :title="dlg.mode === 'rotate' ? `Rotate ${dlg.name}` : 'Store an S3 credential'" :description="dlg.mode === 'rotate' ? 'Enter the NEW key pair. Repositories using this credential switch to it immediately.' : 'Sealed on the server; only the access key id is ever shown again.'" @update:open="(v) => !v && close()">
    <form class="form-grid" data-testid="cred-form" @submit.prevent="submit">
      <FormField v-if="dlg.mode === 'add'" v-slot="f" label="Name" :error="dlg.name && !nameOk ? 'Letters, digits, dot, dash and underscore.' : ''" required>
        <input :id="f.id" v-model="dlg.name" class="input" autocomplete="off" />
      </FormField>
      <FormField v-slot="f" label="Access key" required><input :id="f.id" v-model="dlg.access" class="input mono" autocomplete="off" spellcheck="false" /></FormField>
      <FormField v-slot="f" label="Secret key" required><PasswordInput :id="f.id" v-model="secret" autocomplete="new-password" /></FormField>
      <Callout v-if="dlg.mode === 'rotate'" status="info">Create the new key at your provider first and keep the old one until the next backup and GC succeed; then revoke it there.</Callout>
      <Callout v-if="dlg.error" status="critical" :title="dlg.mode === 'rotate' ? 'Could not rotate the credential' : 'Could not store the credential'">{{ dlg.error }}</Callout>
    </form>
    <template #footer>
      <button type="button" class="btn" @click="close">Cancel</button>
      <button type="button" class="btn btn-primary" :disabled="dlg.busy || !dlg.access.trim() || !secret || !nameOk" @click="submit"><Spinner v-if="dlg.busy" :size="13" />{{ dlg.mode === 'rotate' ? 'Rotate' : 'Store' }}</button>
    </template>
  </Modal>
</template>

<style scoped>
.panel > :deep(.dt) { border: 0; border-radius: 0 0 var(--r-lg) var(--r-lg); }
.strong { font-weight: var(--fw-semibold); }
.acts { display: inline-flex; gap: 4px; }
</style>
