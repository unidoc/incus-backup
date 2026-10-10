<script setup lang="ts">
// "Repository access" for one user (docs/permissions.md): the global role (routes not bound to a
// repository; may be "none") and, when restricted, one role per repository. Repositories without a row
// are invisible to the user. Administrators always reach every repository.
import { computed, ref, watch } from 'vue'
import Modal from '@/components/Modal.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import FormField from '@/components/form/FormField.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import { gap, type GlobalRole, type RepoRole } from '@/api/sections/gapcore'
import { api } from '@/api/endpoints'
import { toast, errorText } from '@/stores/toast'
import type { Role, User } from '@/api/types'

const props = defineProps<{ user: User | null }>()
const emit = defineEmits<{ close: []; saved: [] }>()

const ROLE_LABEL: Record<string, string> = { none: 'No access', readonly: 'Read-only', restore: 'Restore', operator: 'Operator', admin: 'Administrator' }
const globalOpts = (['none', 'readonly', 'restore', 'operator', 'admin'] as GlobalRole[]).map((r) => ({ value: r, label: ROLE_LABEL[r] }))
const repoOpts = [{ value: '', label: 'No access' }, ...(['readonly', 'restore', 'operator', 'admin'] as Role[]).map((r) => ({ value: r, label: ROLE_LABEL[r] }))]

const st = ref({ loading: false, busy: false, error: '', global: 'readonly' as GlobalRole, origGlobal: 'readonly' as GlobalRole, scoped: false, roles: {} as Record<string, string>, repos: [] as string[] })

watch(() => props.user, async (u) => {
  if (!u) return
  st.value = { loading: true, busy: false, error: '', global: u.role as GlobalRole, origGlobal: u.role as GlobalRole, scoped: false, roles: {}, repos: [] }
  try {
    const [a, rs] = await Promise.all([gap.access.get(u.name), api.repositories.list()])
    st.value.global = a.global_role
    st.value.origGlobal = a.global_role
    st.value.scoped = a.scoped
    st.value.repos = (rs ?? []).map((r) => r.name).sort()
    for (const r of st.value.repos) st.value.roles[r] = ''
    for (const s of a.scopes ?? []) st.value.roles[s.repository] = s.role
  } catch (e) {
    st.value.error = errorText(e)
  } finally {
    st.value.loading = false
  }
}, { immediate: true })

const isAdmin = computed(() => st.value.global === 'admin')
const granted = computed(() => st.value.repos.filter((r) => st.value.roles[r]).length)
const summary = computed(() => {
  if (isAdmin.value) return 'Administrators reach every repository.'
  if (!st.value.scoped) return st.value.global === 'none' ? 'No access to any repository.' : `${ROLE_LABEL[st.value.global]} on every repository, including repositories added later.`
  return granted.value ? `Only ${granted.value} of ${st.value.repos.length} repositories; the others are invisible to this user.` : 'Restricted to no repository: the user sees no repository at all.'
})

async function save() {
  const u = props.user
  if (!u) return
  st.value.busy = true
  st.value.error = ''
  try {
    if (st.value.global !== st.value.origGlobal) {
      if (isAdmin.value) {
        await gap.access.set(u.name, false, []) // an administrator cannot keep scopes
      }
      await gap.access.setGlobalRole(u.name, st.value.global)
    }
    if (!isAdmin.value) {
      const scopes: RepoRole[] = st.value.scoped ? st.value.repos.filter((r) => st.value.roles[r]).map((r) => ({ repository: r, role: st.value.roles[r] as Role })) : []
      await gap.access.set(u.name, st.value.scoped, scopes)
    }
    toast.success('Access saved', `${u.name}: ${summary.value}`)
    emit('saved')
    emit('close')
  } catch (e) {
    st.value.error = errorText(e)
  } finally {
    st.value.busy = false
  }
}
</script>

<template>
  <Modal :open="!!user" :title="`Repository access of ${user?.name}`" width="600px" @update:open="(v: boolean) => { if (!v) emit('close') }">
    <div class="acc" data-testid="access-dialog">
      <p v-if="st.loading" class="muted"><Spinner :size="13" /> Loading…</p>
      <template v-else>
        <FormField v-slot="f" label="Global role" hint="Applies to everything that is not one repository: users, settings, notifications, certificates.">
          <SelectInput :id="f.id" v-model="st.global" :options="globalOpts" data-testid="access-global" />
        </FormField>
        <fieldset class="mode" :disabled="isAdmin">
          <legend>Repositories</legend>
          <label class="opt"><input v-model="st.scoped" type="radio" :value="false" name="acc-mode" data-testid="access-all" /> <span><strong>Every repository</strong> with the global role</span></label>
          <label class="opt"><input v-model="st.scoped" type="radio" :value="true" name="acc-mode" data-testid="access-scoped" /> <span><strong>Only the repositories below</strong>, each with its own role</span></label>
        </fieldset>
        <table v-if="st.scoped && !isAdmin" class="grid" data-testid="access-table">
          <thead><tr><th>Repository</th><th>Role</th></tr></thead>
          <tbody>
            <tr v-for="r in st.repos" :key="r">
              <td class="mono">{{ r }}</td>
              <td><SelectInput v-model="st.roles[r]" :options="repoOpts" size="sm" :data-testid="`access-role-${r}`" /></td>
            </tr>
            <tr v-if="!st.repos.length"><td colspan="2" class="muted">No repositories exist yet.</td></tr>
          </tbody>
        </table>
        <Callout :status="isAdmin ? 'info' : st.scoped && !granted ? 'warn' : 'info'" data-testid="access-summary">{{ summary }}</Callout>
        <p class="cx-help">Lists, the dashboard and live events show only the repositories the user may read. API tokens of this user never get more than this.</p>
        <Callout v-if="st.error" status="critical" title="Could not save the access">{{ st.error }}</Callout>
      </template>
    </div>
    <template #footer>
      <button type="button" class="btn btn-ghost" @click="emit('close')">Cancel</button>
      <button type="button" class="btn btn-primary" :disabled="st.busy || st.loading" data-testid="access-save" @click="save"><Spinner v-if="st.busy" :size="13" />Save access</button>
    </template>
  </Modal>
</template>

<style scoped>
.acc { display: grid; gap: 14px; }
.mode { border: 1px solid var(--border); border-radius: var(--r); padding: 8px 12px 10px; display: grid; gap: 6px; margin: 0; }
.mode legend { font-weight: var(--fw-medium); padding: 0 4px; font-size: var(--fs-sm); }
.mode:disabled { opacity: 0.55; }
.opt { display: flex; gap: 8px; align-items: baseline; font-size: var(--fs-sm); cursor: pointer; }
.grid { width: 100%; border-collapse: collapse; font-size: var(--fs-sm); }
.grid th { text-align: left; font-weight: var(--fw-medium); color: var(--text-2); background: var(--surface-2); padding: 6px 10px; border-bottom: 1px solid var(--border); }
.grid td { padding: 6px 10px; border-bottom: 1px solid var(--border); }
.grid td:last-child { width: 200px; }
.mono { font-family: var(--font-mono); }
.muted { color: var(--text-muted); }
.cx-help { font-size: var(--fs-sm); color: var(--text-2); margin: 0; }
</style>
