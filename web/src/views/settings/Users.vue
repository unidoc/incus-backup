<script setup lang="ts">
// Users: who can sign in and with which role. Create, edit role/name/e-mail, disable, delete, reset a
// password (the user must change it at the next sign-in), remove two-factor authentication. The server
// protects the last enabled administrator.
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import CIcon from '@/components/config/CIcon.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import EmptyState from '@/components/EmptyState.vue'
import Modal from '@/components/Modal.vue'
import Spinner from '@/components/Spinner.vue'
import CopyField from '@/components/CopyField.vue'
import Callout from '@/components/Callout.vue'
import FormField from '@/components/form/FormField.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import PasswordInput from '@/components/form/PasswordInput.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import Tabs from '@/components/Tabs.vue'
import { useResource } from '@/api/cache'
import { cfg, list } from '@/api/sections/config'
import { ROLES } from '@/api/perms'
import { ROLE_INFO, generatePassword } from '@/components/config/roles'
import { passwordStrength } from '@/lib/password'
import { session } from '@/stores/session'
import { toast, errorText } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import type { ColumnDef } from '@/lib/table'
import type { Role, User } from '@/api/types'
import '@/components/config/section.css'
import UserAccessDialog from '@/components/access/UserAccessDialog.vue'
import UserIdentitiesDialog from '@/components/authn/UserIdentitiesDialog.vue'
import { authn } from '@/api/sections/authn'

const accessFor = ref<User | null>(null)
const ssoFor = ref<User | null>(null)

const users = useResource(() => 'cfg:users', (s) => cfg.users.list(s))
const rows = computed(() => list(users.data.value))
const admins = computed(() => rows.value.filter((u) => u.role === 'admin' && !u.disabled))
const isLastAdmin = (u: User) => u.role === 'admin' && !u.disabled && admins.value.length === 1
const roleOpts = ROLES.map((r) => ({ value: r, label: ROLE_INFO.find((x) => x.role === r)?.label ?? r }))
// a global role of "none" means per-repository roles only (Repository access)
const editRoleOpts = [...roleOpts, { value: 'none', label: 'No access (repository roles only)' }]
const locked = (u: User) => !!u.locked_until && Date.parse(u.locked_until) > Date.now()
const stateOf = (u: User) => (u.disabled ? 'disabled' : locked(u) ? 'locked' : u.must_change_password ? 'must change password' : 'active')

const cols: ColumnDef<User>[] = [
  { key: 'name', label: 'User', required: true, value: (u) => `${u.name} ${u.display_name} ${u.email}` },
  { key: 'role', label: 'Role', width: '120px' },
  { key: 'totp_enabled', label: 'Two-factor', width: '110px', value: (u) => (u.totp_enabled || u.webauthn_keys ? 1 : 0) },
  { key: 'signin', label: 'Sign-in', width: '190px', value: (u) => signIn(u).join(' ') },
  { key: 'state', label: 'State', width: '180px', value: stateOf },
  { key: 'last_login_at', label: 'Last sign-in', width: '130px' },
  { key: 'created_at', label: 'Created', hidden: true },
  { key: 'actions', label: '', sortable: false, noExport: true, align: 'right', width: '262px', required: true },
]
const lastAdminText = (name: string) => `${name} is the last enabled administrator. Make another user an administrator first, so nobody is locked out of users, settings and the audit log.`
function explain(e: unknown, u: User) {
  const t = errorText(e)
  return /last enabled administrator/.test(t) ? lastAdminText(u.name) : t
}

// how a user signs in (password, passkeys / security keys, identity providers)
function signIn(u: User): string[] {
  const out: string[] = []
  if (u.has_password !== false) out.push('password')
  if (u.webauthn_keys) out.push(`${u.webauthn_keys} key${u.webauthn_keys > 1 ? 's' : ''}`)
  for (const p of u.identities ?? []) out.push(`SSO ${p}`)
  return out
}
async function resetKeys(u: User) {
  if (!(await confirm({ title: `Remove every security key and passkey of ${u.name}?`, message: 'Use this when they lost a key or a key is blocked as possibly copied. They sign in with the password (and code, if set up) until they register a key again in their profile.', confirmLabel: 'Remove keys', danger: true }))) return
  try { const r = await authn.resetUserKeys(u.name); toast.success('Security keys removed', `${r.removed} removed. Ask ${u.name} to register a key again.`); void users.refresh() } catch (e) { toast.error(e, 'Could not remove the keys') }
}

// ---- create
const cr = ref({ open: false, name: '', display: '', email: '', role: 'operator' as Role, password: '', must_change: true, busy: false, error: '', done: '' })
function openCreate() { cr.value = { open: true, name: '', display: '', email: '', role: 'operator', password: generatePassword(), must_change: true, busy: false, error: '', done: '' } }
const crOk = computed(() => /^[^\s/:]{1,64}$/.test(cr.value.name) && passwordStrength(cr.value.password).acceptable)
async function create() {
  cr.value.busy = true
  cr.value.error = ''
  try {
    await cfg.users.create({ name: cr.value.name.trim(), password: cr.value.password, role: cr.value.role, display: cr.value.display.trim(), email: cr.value.email.trim(), must_change: cr.value.must_change })
    cr.value.done = cr.value.password
    toast.success('User created', `${cr.value.name} can sign in now.`)
    void users.refresh()
  } catch (e) { cr.value.error = errorText(e) } finally { cr.value.busy = false }
}

// ---- edit
const ed = ref<{ user: User; display: string; email: string; role: Role; busy: boolean; error: string } | null>(null)
function openEdit(u: User) { ed.value = { user: u, display: u.display_name, email: u.email, role: u.role, busy: false, error: '' } }
async function saveEdit() {
  const e = ed.value
  if (!e) return
  e.busy = true
  e.error = ''
  try {
    await cfg.users.update(e.user.name, { display_name: e.display.trim(), email: e.email.trim(), role: e.role })
    toast.success('User saved', e.role !== e.user.role ? `${e.user.name} is now ${e.role}. Their API tokens are limited to it.` : undefined)
    ed.value = null
    void users.refresh()
  } catch (x) { e.error = explain(x, e.user) } finally { if (ed.value) ed.value.busy = false }
}

// ---- reset password
const rp = ref<{ user: User; password: string; must_change: boolean; busy: boolean; error: string; done: boolean } | null>(null)
function openReset(u: User) { rp.value = { user: u, password: generatePassword(), must_change: true, busy: false, error: '', done: false } }
async function reset() {
  const r = rp.value
  if (!r) return
  r.busy = true
  r.error = ''
  try { await cfg.users.resetPassword(r.user.name, r.password, r.must_change); r.done = true; toast.success('Password reset', `${r.user.name} was signed out everywhere.`); void users.refresh() } catch (e) { r.error = errorText(e) } finally { r.busy = false }
}

async function setDisabled(u: User, disabled: boolean) {
  if (disabled && !(await confirm({ title: `Disable ${u.name}?`, message: 'They cannot sign in, their sessions end and their API tokens stop working. Nothing is deleted; enable them again any time.', confirmLabel: 'Disable user', danger: true }))) return
  try { await cfg.users.update(u.name, { disabled }); toast.success(disabled ? 'User disabled' : 'User enabled'); void users.refresh() } catch (e) { toast.error(explain(e, u), disabled ? 'Could not disable the user' : 'Could not enable the user') }
}
async function removeTOTP(u: User) {
  if (!(await confirm({ title: `Remove two-factor authentication of ${u.name}?`, message: 'Use this when they lost their phone and their recovery codes. They sign in with the password only until they set it up again in their profile.', confirmLabel: 'Remove two-factor', danger: true }))) return
  try { await cfg.users.removeTOTP(u.name); toast.success('Two-factor authentication removed', `Ask ${u.name} to set it up again.`); void users.refresh() } catch (e) { toast.error(e, 'Could not remove two-factor authentication') }
}
async function remove(u: User) {
  if (isLastAdmin(u)) { toast.error(lastAdminText(u.name), 'Could not delete the user'); return }
  const self = u.name.toLowerCase() === session.name.toLowerCase()
  if (!(await confirm({ title: `Delete ${u.name}?`, message: `${self ? 'This is your own account: you are signed out at once. ' : ''}The user, their sessions and their API tokens are deleted. The audit log keeps their name. Consider disabling instead.`, confirmLabel: 'Delete user', danger: true, typeToConfirm: u.name }))) return
  try { await cfg.users.remove(u.name); toast.success('User deleted'); void users.refresh() } catch (e) { toast.error(explain(e, u), 'Could not delete the user') }
}
function closeMenu(e: MouseEvent) { const d = (e.currentTarget as HTMLElement).closest('details'); if (d) d.open = false }
const tabs = [{ id: 'users', label: 'Users', to: { name: 'users' } }, { id: 'tokens', label: 'API tokens', to: { name: 'tokens' } }]
</script>

<template>
  <div>
    <header class="page-head">
      <div>
        <h1>Users</h1>
        <p class="lede">People who sign in to this console, and what their role lets them do. Every change is in the audit log.</p>
      </div>
      <div class="actions"><button type="button" class="btn btn-primary" data-testid="user-new" @click="openCreate"><CIcon name="user-plus" :size="14" />Add a user</button></div>
    </header>
    <Tabs :tabs="tabs" label="Users and tokens" class="subnav" />

    <div class="layout">
      <DataTable :rows="rows" :columns="cols" row-key="id" :loading="users.loading.value" :error="users.error.value" :retry="users.refresh" storage-key="cfg-users" csv-name="users" data-testid="user-table">
        <template #cell-name="{ row }">
          <div class="cx-title"><span class="t">{{ row.name }}<span v-if="row.name.toLowerCase() === session.name.toLowerCase()" class="cx-tag info you">you</span></span><span v-if="row.display_name || row.email" class="d">{{ [row.display_name, row.email].filter(Boolean).join(' · ') }}</span></div>
        </template>
        <template #cell-role="{ row }"><span class="cx-tag" :class="{ crit: row.role === 'admin' }">{{ row.role }}</span></template>
        <template #cell-totp_enabled="{ row }"><StatusPill :status="row.totp_enabled || row.webauthn_keys ? 'ok' : row.has_password === false ? 'neutral' : row.role === 'admin' ? 'warn' : 'neutral'" :label="row.totp_enabled || row.webauthn_keys ? 'on' : row.has_password === false ? 'at IdP' : 'off'" size="sm" /></template>
        <template #cell-signin="{ row }"><span class="signin"><span v-for="m in signIn(row)" :key="m" class="cx-tag" :class="{ info: m.startsWith('SSO'), ok: m.includes('key') }">{{ m }}</span></span></template>
        <template #cell-state="{ row }">
          <StatusPill :status="row.disabled ? 'neutral' : locked(row) ? 'critical' : row.must_change_password ? 'warn' : 'ok'" :label="stateOf(row)" size="sm" />
          <div v-if="locked(row)" class="muted small">until <AbsoluteTime :time="row.locked_until" /></div>
        </template>
        <template #cell-last_login_at="{ row }"><RelativeTime :time="row.last_login_at" /></template>
        <template #cell-created_at="{ row }"><AbsoluteTime :time="row.created_at" /></template>
        <template #cell-actions="{ row }">
          <span class="cx-actions">
            <button type="button" class="btn btn-sm btn-ghost" :data-testid="`user-edit-${row.name}`" @click="openEdit(row)"><Icon name="pencil" :size="14" />Edit</button>
            <button type="button" class="btn btn-sm btn-ghost btn-icon" :data-testid="`user-access-${row.name}`" title="Repository access: global role and per-repository roles" :aria-label="`Repository access of ${row.name}`" @click="accessFor = row"><Icon name="database" :size="14" /></button>
            <button type="button" class="btn btn-sm btn-ghost" :data-testid="`user-reset-${row.name}`" title="Set a new password" @click="openReset(row)"><CIcon name="key" :size="14" />Password</button>
            <details class="more">
              <summary class="btn btn-sm btn-ghost btn-icon" :aria-label="`More for ${row.name}`" :data-testid="`user-more-${row.name}`"><Icon name="ellipsis" :size="14" /></summary>
              <div class="menu" role="menu" @click="closeMenu">
                <button v-if="row.totp_enabled" type="button" role="menuitem" @click="removeTOTP(row)">Remove two-factor</button>
                <button v-if="row.webauthn_keys" type="button" role="menuitem" :data-testid="`user-reset-keys-${row.name}`" @click="resetKeys(row)">Remove security keys</button>
                <button type="button" role="menuitem" :data-testid="`user-sso-${row.name}`" @click="ssoFor = row">Single sign-on…</button>
                <button type="button" role="menuitem" :disabled="!row.disabled && isLastAdmin(row)" :title="!row.disabled && isLastAdmin(row) ? lastAdminText(row.name) : ''" :data-testid="`user-disable-${row.name}`" @click="setDisabled(row, !row.disabled)">{{ row.disabled ? 'Enable' : 'Disable' }}</button>
                <button type="button" role="menuitem" class="danger" :disabled="isLastAdmin(row)" :title="isLastAdmin(row) ? lastAdminText(row.name) : ''" :data-testid="`user-delete-${row.name}`" @click="remove(row)">Delete…</button>
              </div>
            </details>
          </span>
        </template>
        <template #empty><EmptyState title="No users" icon="users">Add the people who run backups. Give each person their own account.</EmptyState></template>
      </DataTable>

      <aside class="side">
        <section class="panel" data-testid="role-panel">
          <div class="panel-head"><h2>Roles</h2></div>
          <div class="panel-body roles">
            <div v-for="r in ROLE_INFO" :key="r.role" class="ri"><div class="rn"><span class="cx-tag" :class="{ crit: r.role === 'admin' }">{{ r.role }}</span>{{ r.label }}</div><ul><li v-for="c in r.can" :key="c">{{ c }}</li></ul></div>
            <p class="cx-help">Every request is checked by the server, whatever the console shows.</p>
          </div>
        </section>
        <section class="panel">
          <div class="panel-head"><h2>Sessions and sign-in</h2></div>
          <div class="panel-body cx-help">
            <p>Sessions end after 30 minutes without activity and after 12 hours at the latest. Each person sees and signs out their own sessions in their <RouterLink :to="{ name: 'profile' }">profile</RouterLink>.</p>
            <p>Disabling a user or resetting their password ends all their sessions at once. Five wrong passwords lock an account for 1 to 15 minutes.</p>
            <p v-if="admins.some((a) => !a.totp_enabled && !a.webauthn_keys && a.has_password !== false)" class="warnp"><strong>{{ admins.filter((a) => !a.totp_enabled && !a.webauthn_keys && a.has_password !== false).length }} of {{ admins.length }} administrators without two-factor authentication.</strong> Administrators should always use it; <RouterLink :to="{ name: 'authentication' }">Settings → Authentication</RouterLink> can require it.</p>
          </div>
        </section>
      </aside>
    </div>

    <!-- create -->
    <Modal :open="cr.open" title="Add a user" width="560px" @update:open="(v: boolean) => { cr.open = v }">
      <form v-if="!cr.done" id="user-form" class="cx-form" data-testid="user-form" @submit.prevent="create">
        <div class="cx-cols">
          <FormField v-slot="f" label="User name" hint="For signing in; cannot be changed later"><input :id="f.id" v-model="cr.name" class="input" autocomplete="off" spellcheck="false" data-testid="user-name" /></FormField>
          <FormField v-slot="f" label="Role"><SelectInput :id="f.id" v-model="cr.role" :options="roleOpts" data-testid="user-new-role" /></FormField>
        </div>
        <div class="cx-cols">
          <FormField v-slot="f" label="Display name" optional><input :id="f.id" v-model="cr.display" class="input" /></FormField>
          <FormField v-slot="f" label="E-mail" optional><input :id="f.id" v-model="cr.email" type="email" class="input" /></FormField>
        </div>
        <FormField v-slot="f" label="First password" hint="Give it to the user through a safe channel">
          <div class="pwrow"><PasswordInput :id="f.id" v-model="cr.password" strength :context="[cr.name]" autocomplete="new-password" /><button type="button" class="btn btn-sm" @click="cr.password = generatePassword()">Generate</button></div>
        </FormField>
        <SwitchInput v-model="cr.must_change" label="They must choose their own password at the first sign-in" />
        <Callout v-if="cr.error" status="critical" title="Could not create the user">{{ cr.error }}</Callout>
      </form>
      <div v-else class="cx-form">
        <Callout status="ok" :title="`${cr.name} was created`">Give them the user name and this password. {{ cr.must_change ? 'They choose their own at the first sign-in.' : '' }}</Callout>
        <CopyField :value="cr.done" label="Password" secret once />
      </div>
      <template #footer>
        <template v-if="!cr.done"><button type="button" class="btn btn-ghost" @click="cr.open = false">Cancel</button><button type="submit" form="user-form" class="btn btn-primary" :disabled="!crOk || cr.busy" data-testid="user-create"><Spinner v-if="cr.busy" :size="13" />Add user</button></template>
        <button v-else type="button" class="btn btn-primary" data-testid="user-done" @click="cr.open = false">Done</button>
      </template>
    </Modal>

    <!-- edit -->
    <Modal :open="!!ed" :title="`Edit ${ed?.user.name}`" width="520px" @update:open="(v: boolean) => { if (!v) ed = null }">
      <form v-if="ed" id="user-edit" class="cx-form" data-testid="user-edit-form" @submit.prevent="saveEdit">
        <FormField v-slot="f" label="Role"><SelectInput :id="f.id" v-model="ed.role" :options="editRoleOpts" data-testid="user-role" /></FormField>
        <Callout v-if="ed.user.role === 'admin' && ed.role !== 'admin' && isLastAdmin(ed.user)" status="warn" title="Last administrator">{{ lastAdminText(ed.user.name) }}</Callout>
        <div class="cx-cols">
          <FormField v-slot="f" label="Display name" optional><input :id="f.id" v-model="ed.display" class="input" /></FormField>
          <FormField v-slot="f" label="E-mail" optional><input :id="f.id" v-model="ed.email" type="email" class="input" /></FormField>
        </div>
        <p class="cx-help">A lower role limits the user's API tokens at once; a higher role does not raise them.</p>
        <Callout v-if="ed.error" status="critical" title="Could not save the user">{{ ed.error }}</Callout>
      </form>
      <template #footer><button type="button" class="btn btn-ghost" @click="ed = null">Cancel</button><button type="submit" form="user-edit" class="btn btn-primary" :disabled="ed?.busy" data-testid="user-save"><Spinner v-if="ed?.busy" :size="13" />Save</button></template>
    </Modal>

    <UserAccessDialog :user="accessFor" @close="accessFor = null" @saved="users.refresh()" />
    <UserIdentitiesDialog :user="ssoFor" @close="ssoFor = null" @changed="users.refresh()" />

    <!-- reset password -->
    <Modal :open="!!rp" :title="`New password for ${rp?.user.name}`" width="520px" @update:open="(v: boolean) => { if (!v) rp = null }">
      <form v-if="rp && !rp.done" id="user-reset" class="cx-form" @submit.prevent="reset">
        <p class="cx-help">The user is signed out everywhere. Their API tokens keep working; revoke them on the API tokens page if the account was compromised.</p>
        <FormField v-slot="f" label="New password"><div class="pwrow"><PasswordInput :id="f.id" v-model="rp.password" strength :context="[rp.user.name]" autocomplete="new-password" /><button type="button" class="btn btn-sm" @click="rp.password = generatePassword()">Generate</button></div></FormField>
        <SwitchInput v-model="rp.must_change" label="They must choose a new password at the next sign-in" />
        <Callout v-if="rp.error" status="critical" title="Could not reset the password">{{ rp.error }}</Callout>
      </form>
      <div v-else-if="rp" class="cx-form"><Callout status="ok" title="Password reset">Give {{ rp.user.name }} this password through a safe channel.</Callout><CopyField :value="rp.password" label="Password" secret once /></div>
      <template #footer>
        <template v-if="rp && !rp.done"><button type="button" class="btn btn-ghost" @click="rp = null">Cancel</button><button type="submit" form="user-reset" class="btn btn-primary" :disabled="rp.busy || !passwordStrength(rp.password).acceptable" data-testid="user-reset-submit"><Spinner v-if="rp.busy" :size="13" />Set password</button></template>
        <button v-else type="button" class="btn btn-primary" @click="rp = null">Done</button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.subnav { margin-bottom: 16px; }
.layout { display: grid; grid-template-columns: minmax(0, 1fr) 320px; gap: 16px; align-items: start; }
.side { display: grid; grid-template-columns: minmax(0, 1fr); gap: 16px; }
.you { margin-left: 6px; }
.signin { display: inline-flex; flex-wrap: wrap; gap: 4px; }
.roles { display: grid; grid-template-columns: minmax(0, 1fr); gap: 12px; }
.ri ul { margin: 4px 0 0; padding-left: 18px; font-size: var(--fs-sm); color: var(--text-2); line-height: 1.5; }
.rn { display: flex; align-items: center; gap: 8px; font-weight: var(--fw-medium); }
.pwrow { display: grid; grid-template-columns: 1fr auto; gap: 8px; align-items: start; }
.warnp { color: var(--warn-text); }
.more { position: relative; }
.more > summary { list-style: none; }
.more > summary::-webkit-details-marker { display: none; }
.menu { position: absolute; right: 0; top: calc(100% + 4px); z-index: 20; min-width: 180px; background: var(--surface); border: 1px solid var(--border-strong); border-radius: var(--r); box-shadow: var(--shadow-pop); padding: 4px; display: grid; }
.menu button { appearance: none; border: 0; background: none; font: inherit; text-align: left; padding: 7px 10px; border-radius: var(--r-sm); color: var(--text); cursor: pointer; font-size: var(--fs-sm); }
.menu button:hover:not(:disabled) { background: var(--surface-3); }
.menu button:disabled { color: var(--text-faint); cursor: not-allowed; }
.menu .danger { color: var(--critical-text); }
@media (max-width: 1100px) { .layout { grid-template-columns: minmax(0, 1fr); } }
</style>
