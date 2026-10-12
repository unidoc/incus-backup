<script setup lang="ts">
// The signed-in user's own account: password, two-factor authentication, sessions, API tokens.
import { ref, computed, nextTick, watch } from 'vue'
import QRCode from 'qrcode'
import Icon from '@/components/icons/Icon.vue'
import Tabs from '@/components/Tabs.vue'
import KeyValue from '@/components/KeyValue.vue'
import FormField from '@/components/form/FormField.vue'
import PasswordInput from '@/components/form/PasswordInput.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import CopyField from '@/components/CopyField.vue'
import Modal from '@/components/Modal.vue'
import DataTable from '@/components/DataTable.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import Stepper from '@/components/Stepper.vue'
import { api } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import { session, refreshUser } from '@/stores/session'
import { passwordStrength } from '@/lib/password'
import { ROLES, atLeast } from '@/api/perms'
import { toast, errorText } from '@/stores/toast'
import { ApiError } from '@/api/client'
import { passwordCheckError, needsPassword } from '@/lib/authErrors'
import { confirm } from '@/composables/useConfirm'
import { useClipboard, downloadText } from '@/composables/useClipboard'
import type { ColumnDef } from '@/lib/table'
import type { SessionInfo, TokenInfo, Role, TOTPEnrolment } from '@/api/types'
import { gap } from '@/api/sections/gapcore'
import { repos } from '@/stores/repos'
import SecurityKeys from '@/components/authn/SecurityKeys.vue'

const tab = ref('security')
const tabs = [{ id: 'security', label: 'Password and 2FA' }, { id: 'sessions', label: 'Sessions' }, { id: 'tokens', label: 'API tokens' }]
const isUser = computed(() => session.kind === 'user' && !!session.user)

// ---- password
const pw = ref({ current: '', next: '', confirm: '' })
const pwBusy = ref(false)
const pwError = ref('')
const pwOk = computed(() => pw.value.current && passwordStrength(pw.value.next, [session.name]).acceptable && pw.value.next === pw.value.confirm)
async function changePassword() {
  if (!pwOk.value) return
  pwBusy.value = true
  pwError.value = ''
  try {
    await api.me.changePassword({ current: pw.value.current, new: pw.value.next })
    pw.value = { current: '', next: '', confirm: '' }
    toast.success('Password changed', 'All your other sessions were signed out.')
    invalidate('me:sessions')
  } catch (e) {
    pwError.value = passwordCheckError(e)
  } finally {
    pwBusy.value = false
  }
}

// ---- 2FA enrolment: start -> scan + confirm -> recovery codes (shown once)
// ask: the sign-in is older than 10 minutes, so the server wants the password again (as for keys and tokens)
type Enrol = { step: number; data: TOTPEnrolment | null; qr: string; code: string; codes: string[]; busy: boolean; error: string; ask: boolean; password: string }
const blankEnrol = (step = -1): Enrol => ({ step, data: null, qr: '', code: '', codes: [], busy: false, error: '', ask: false, password: '' })
const enrol = ref<Enrol>(blankEnrol())
const askText = 'Your sign-in is older than 10 minutes: confirm your password to set up two-factor authentication.'
const pwFailed = (e: unknown) => e instanceof ApiError && (e.status === 401 || e.status === 429)
const codeEl = ref<HTMLInputElement | null>(null)
async function startEnrol(password?: string) {
  enrol.value = { ...blankEnrol(0), busy: true, ask: enrol.value.ask, password: password ?? '' }
  try {
    const d = await api.me.beginTOTP(password || undefined)
    const qr = await QRCode.toString(d.uri, { type: 'svg', margin: 1, errorCorrectionLevel: 'M', color: { dark: '#0f1720', light: '#ffffff' } })
    enrol.value = { ...enrol.value, data: d, qr, busy: false }
    await nextTick()
    codeEl.value?.focus()
  } catch (e) {
    if (needsPassword(e)) enrol.value = { ...enrol.value, busy: false, ask: true, error: password ? passwordCheckError(e) : askText }
    else enrol.value = { ...enrol.value, busy: false, error: pwFailed(e) ? passwordCheckError(e) : errorText(e) }
  }
}
async function confirmEnrol() {
  if (enrol.value.code.length !== 6) return
  enrol.value.busy = true
  enrol.value.error = ''
  try {
    const r = await api.me.confirmTOTP({ code: enrol.value.code, password: enrol.value.ask && enrol.value.password ? enrol.value.password : undefined })
    enrol.value = { ...enrol.value, step: 1, codes: r.recovery_codes, busy: false, data: null, qr: '', password: '' }
    await refreshUser()
  } catch (e) {
    if (needsPassword(e)) enrol.value = { ...enrol.value, busy: false, ask: true, error: enrol.value.password ? passwordCheckError(e) : askText }
    else if (pwFailed(e)) enrol.value = { ...enrol.value, busy: false, error: passwordCheckError(e) }
    else enrol.value = { ...enrol.value, busy: false, error: `${errorText(e)}. Check the time on your phone and enter the current code.`, code: '' }
  }
}
const savedCodes = ref(false)
const { copy: copyCodes, copied: codesCopied } = useClipboard()
const codesText = computed(() => `incus-backup recovery codes for ${session.name} (${location.host})\nEach code works once.\n\n${enrol.value.codes.join('\n')}\n`)
function finishEnrol() {
  enrol.value = blankEnrol()
  savedCodes.value = false
  toast.success('Two-factor authentication is on', 'You will be asked for a code at every sign-in.')
}
function cancelEnrol() { enrol.value = blankEnrol() }

// ---- disable 2FA (password prompt)
const disable = ref({ open: false, password: '', busy: false, error: '' })
async function disableTOTP() {
  disable.value.busy = true
  disable.value.error = ''
  try {
    await api.me.disableTOTP({ password: disable.value.password })
    disable.value = { open: false, password: '', busy: false, error: '' }
    await refreshUser()
    toast.success('Two-factor authentication is off')
  } catch (e) {
    disable.value.busy = false
    disable.value.error = passwordCheckError(e)
  }
}

// ---- sessions
const sessions = useResource(() => 'me:sessions', (s) => api.me.sessions(s), { enabled: () => isUser.value })
const sessionCols: ColumnDef<SessionInfo>[] = [
  { key: 'user_agent', label: 'Browser', required: true, format: (r) => browserName(r.user_agent) },
  { key: 'ip', label: 'Address', mono: true },
  { key: 'created_at', label: 'Signed in' },
  { key: 'last_seen', label: 'Last active' },
  { key: 'expires_at', label: 'Ends at the latest', hidden: true },
  { key: 'actions', label: '', sortable: false, noExport: true, align: 'right', width: '110px', required: true },
]
function browserName(ua: string) {
  const m = /(Firefox|Edg|Chrome|Safari|curl|Go-http-client)\/([\d.]+)/.exec(ua)
  const os = /Windows|Mac OS X|Linux|Android|iPhone/.exec(ua)?.[0]?.replace('Mac OS X', 'macOS') ?? ''
  if (!m) return ua || 'Unknown'
  const n = m[1] === 'Edg' ? 'Edge' : m[1]
  return `${n} ${m[2].split('.')[0]}${os ? ' on ' + os : ''}`
}
async function revokeSession(s: SessionInfo) {
  if (!(await confirm({ title: 'Sign out this session?', message: `${browserName(s.user_agent)} from ${s.ip} will need to sign in again.`, confirmLabel: 'Sign out session' }))) return
  try { await api.me.revokeSession(s.id); toast.success('Session signed out'); void sessions.refresh() } catch (e) { toast.error(e, 'Could not sign out the session') }
}

// ---- tokens
const tokens = useResource(() => 'me:tokens', (s) => api.me.tokens(s), { enabled: () => isUser.value })
watch(tab, (t) => { if (t === 'sessions') void sessions.refresh(); if (t === 'tokens') void tokens.refresh() }) // lists are fresh when shown
const tokenCols: ColumnDef<TokenInfo>[] = [
  { key: 'name', label: 'Name', required: true },
  { key: 'role', label: 'Role' },
  { key: 'repositories', label: 'Repositories', value: (t) => ((t as TokenInfo & { repositories?: string[] | null }).repositories ?? ['all']).join(', ') },
  { key: 'created_at', label: 'Created' },
  { key: 'last_used_at', label: 'Last used' },
  { key: 'expires_at', label: 'Expires' },
  { key: 'id', label: 'ID', mono: true, hidden: true },
  { key: 'actions', label: '', sortable: false, noExport: true, align: 'right', width: '90px', required: true },
]
const roleOptions = computed(() => ROLES.filter((r) => atLeast(session.role, r)).map((r) => ({ value: r, label: r })))
const nt = ref({ open: false, name: '', role: '' as Role | '', ttl: '720', busy: false, error: '', secret: '', restrict: false, repos: [] as string[], ask: false, password: '' })
function openToken() { nt.value = { open: true, name: '', role: session.role, ttl: '720', busy: false, error: '', secret: '', restrict: false, repos: [], ask: false, password: '' } }
async function createToken() {
  nt.value.busy = true
  nt.value.error = ''
  try {
    const r = await gap.createToken({ name: nt.value.name.trim(), role: (nt.value.role || undefined) as Role | undefined, ttl_hours: Number(nt.value.ttl) || 0, repositories: nt.value.restrict ? nt.value.repos : undefined, password: nt.value.ask && nt.value.password ? nt.value.password : undefined })
    nt.value.secret = r.token
    nt.value.password = ''
    void tokens.refresh()
  } catch (e) {
    if (needsPassword(e)) {
      nt.value.ask = true
      nt.value.error = nt.value.password ? passwordCheckError(e) : 'Your sign-in is older than 10 minutes: confirm your password to create a token.'
    } else nt.value.error = e instanceof ApiError && (e.status === 401 || e.status === 429) ? passwordCheckError(e) : errorText(e)
  } finally {
    nt.value.busy = false
  }
}
async function revokeToken(t: TokenInfo) {
  if (!(await confirm({ title: `Revoke the token “${t.name}”?`, message: 'Scripts using it stop working immediately.', confirmLabel: 'Revoke token', danger: true, typeToConfirm: t.name }))) return
  try { await api.me.revokeToken(t.id); toast.success('Token revoked'); void tokens.refresh() } catch (e) { toast.error(e, 'Could not revoke the token') }
}
</script>

<template>
  <div class="profile">
    <header class="page-head">
      <div>
        <h1>Profile</h1>
        <p class="lede">Your account, how you sign in, and the API tokens you own.</p>
      </div>
    </header>

    <Callout v-if="!isUser" status="info" title="Signed in with an API token">Profile settings belong to people. This request uses {{ session.kind }} credentials ({{ session.name }}).</Callout>

    <template v-else>
      <section class="panel acct">
        <div class="avatar" aria-hidden="true">{{ (session.user?.display_name || session.name).slice(0, 1).toUpperCase() }}</div>
        <KeyValue :columns="2" :items="[
          { key: 'name', label: 'User name', value: session.user?.name, mono: true },
          { key: 'role', label: 'Role', value: session.user?.role },
          { key: 'display', label: 'Display name', value: session.user?.display_name },
          { key: 'email', label: 'E-mail', value: session.user?.email },
          { key: 'totp', label: 'Two-factor', value: session.user?.totp_enabled ? 'on' : 'off' },
          { key: 'last', label: 'Last sign-in', value: session.user?.last_login_at },
        ]">
          <template #value-totp><StatusPill :status="session.user?.totp_enabled || session.user?.webauthn_keys ? 'ok' : session.user?.has_password === false ? 'neutral' : 'warn'" :label="session.user?.totp_enabled || session.user?.webauthn_keys ? 'On' : session.user?.has_password === false ? 'At your identity provider' : 'Off'" data-testid="totp-status" /></template>
          <template #value-last><RelativeTime :time="session.user?.last_login_at" /></template>
        </KeyValue>
      </section>

      <Tabs v-model="tab" :tabs="tabs" label="Profile sections" />

      <div v-if="tab === 'security'" class="grid2">
        <section class="panel">
          <div class="panel-head"><h2>Password</h2><span class="sub">Changing it signs out your other sessions</span></div>
          <form class="panel-body form-grid" data-testid="profile-password" @submit.prevent="changePassword">
            <FormField v-slot="f" label="Current password"><PasswordInput :id="f.id" v-model="pw.current" autocomplete="current-password" name="current" /></FormField>
            <FormField v-slot="f" label="New password"><PasswordInput :id="f.id" v-model="pw.next" strength :context="[session.name]" autocomplete="new-password" name="new" /></FormField>
            <FormField v-slot="f" label="Repeat the new password" :error="pw.confirm && pw.confirm !== pw.next ? 'The passwords do not match.' : ''"><PasswordInput :id="f.id" v-model="pw.confirm" autocomplete="new-password" name="confirm" /></FormField>
            <p v-if="pwError" class="err" role="alert">{{ pwError }}</p>
            <div class="form-actions"><button type="submit" class="btn btn-primary" :disabled="!pwOk || pwBusy"><Spinner v-if="pwBusy" :size="13" />Change password</button></div>
          </form>
        </section>

        <section class="panel" data-testid="totp-panel">
          <div class="panel-head"><h2>Two-factor authentication</h2><StatusPill :status="session.user?.totp_enabled ? 'ok' : 'warn'" :label="session.user?.totp_enabled ? 'On' : 'Off'" size="sm" /></div>
          <div class="panel-body">
            <template v-if="enrol.step === -1">
              <template v-if="session.user?.totp_enabled">
                <p class="txt">A code from your authenticator app is required at every sign-in. If you lose the device, use a recovery code.</p>
                <div class="form-actions"><button type="button" class="btn" data-testid="totp-disable" @click="disable = { open: true, password: '', busy: false, error: '' }">Turn off two-factor</button></div>
              </template>
              <template v-else>
                <p class="txt">Protect your account with a time-based code from an authenticator app (any TOTP app works). Administrators should always use it.</p>
                <div class="form-actions"><button type="button" class="btn btn-primary" data-testid="totp-enable" @click="startEnrol()"><Icon name="smartphone" />Set up two-factor</button></div>
              </template>
            </template>

            <template v-else>
              <Stepper :steps="['Scan and confirm', 'Save recovery codes']" :current="enrol.step" />
              <div v-if="enrol.step === 0">
                <div v-if="enrol.busy && !enrol.data" class="row"><Spinner /> Preparing…</div>
                <form v-else-if="enrol.ask && !enrol.data" class="form-grid" data-testid="totp-password-form" @submit.prevent="startEnrol(enrol.password)">
                  <FormField v-slot="f" label="Your password" :error="enrol.error"><PasswordInput :id="f.id" v-model="enrol.password" autocomplete="current-password" :invalid="f.invalid" data-testid="totp-password" /></FormField>
                  <div class="form-actions">
                    <button type="submit" class="btn btn-primary" :disabled="!enrol.password" data-testid="totp-password-continue">Continue</button>
                    <button type="button" class="btn btn-ghost" @click="cancelEnrol">Cancel</button>
                  </div>
                </form>
                <div v-else-if="enrol.data" class="enrol">
                  <div class="qr" role="img" aria-label="QR code for your authenticator app" v-html="enrol.qr" />
                  <div class="enrol-side">
                    <p class="txt">Scan the code with your authenticator app, or enter the key by hand.</p>
                    <CopyField :value="enrol.data.secret" label="Key" data-testid="totp-secret" />
                    <form class="form-grid" @submit.prevent="confirmEnrol">
                      <FormField v-if="enrol.ask" v-slot="f" label="Your password"><PasswordInput :id="f.id" v-model="enrol.password" autocomplete="current-password" data-testid="totp-confirm-password" /></FormField>
                      <FormField v-slot="f" label="Code from the app" :error="enrol.error">
                        <input :id="f.id" ref="codeEl" v-model="enrol.code" class="input mono code" inputmode="numeric" maxlength="6" autocomplete="one-time-code" placeholder="000000" :aria-invalid="f.invalid" :aria-describedby="f.describedby" data-testid="totp-confirm-code" @input="enrol.code = enrol.code.replace(/\D/g, '')" />
                      </FormField>
                      <div class="form-actions">
                        <button type="submit" class="btn btn-primary" :disabled="enrol.code.length !== 6 || enrol.busy || (enrol.ask && !enrol.password)" data-testid="totp-confirm"><Spinner v-if="enrol.busy" :size="13" />Confirm</button>
                        <button type="button" class="btn btn-ghost" @click="cancelEnrol">Cancel</button>
                      </div>
                    </form>
                  </div>
                </div>
                <p v-else-if="enrol.error" class="err" role="alert">{{ enrol.error }}</p>
              </div>
              <div v-else class="codes-step" data-testid="recovery-codes">
                <Callout status="warn" title="Save these recovery codes now">Each code signs you in once if you lose your phone. They are shown only now and cannot be displayed again.</Callout>
                <ol class="codes num"><li v-for="c in enrol.codes" :key="c"><code>{{ c }}</code></li></ol>
                <div class="form-actions">
                  <button type="button" class="btn" @click="copyCodes(enrol.codes.join('\n')); savedCodes = true"><Icon :name="codesCopied ? 'check' : 'copy'" />{{ codesCopied ? 'Copied' : 'Copy' }}</button>
                  <button type="button" class="btn" @click="downloadText(`incus-backup-recovery-${session.name}.txt`, codesText); savedCodes = true"><Icon name="download" />Download</button>
                </div>
                <label class="ack"><input v-model="savedCodes" type="checkbox" data-testid="codes-saved" /> I saved the recovery codes in a safe place</label>
                <div class="form-actions"><button type="button" class="btn btn-primary" :disabled="!savedCodes" data-testid="totp-done" @click="finishEnrol">Done</button></div>
              </div>
            </template>
          </div>
        </section>
        <section class="panel keys-panel" data-testid="webauthn-panel">
          <div class="panel-head"><h2>Security keys &amp; passkeys</h2><StatusPill :status="session.user?.webauthn_keys ? 'ok' : 'neutral'" :label="session.user?.webauthn_keys ? `${session.user.webauthn_keys} registered` : 'None'" size="sm" /></div>
          <div class="panel-body">
            <p class="txt">Sign in with a passkey (no password), or use a key as the second step after your password. Register two keys, or a key and an authenticator app, so losing one does not lock you out.</p>
            <SecurityKeys />
          </div>
        </section>
      </div>

      <section v-else-if="tab === 'sessions'">
        <DataTable :rows="sessions.data.value" :columns="sessionCols" row-key="id" :loading="sessions.loading.value" :error="sessions.error.value" :retry="sessions.refresh" storage-key="me-sessions" csv-name="sessions" :initial-sort="{ key: 'last_seen', dir: 'desc' }" :filterable="false" empty-title="No sessions">
          <template #cell-user_agent="{ row }"><span class="truncate" :title="row.user_agent">{{ browserName(row.user_agent) }}</span> <StatusPill v-if="row.current" status="info" label="This session" size="sm" /></template>
          <template #cell-created_at="{ row }"><AbsoluteTime :time="row.created_at" /></template>
          <template #cell-last_seen="{ row }"><RelativeTime :time="row.last_seen" /></template>
          <template #cell-expires_at="{ row }"><AbsoluteTime :time="row.expires_at" /></template>
          <template #cell-actions="{ row }"><button v-if="!row.current" type="button" class="btn btn-sm" @click="revokeSession(row)">Sign out</button></template>
        </DataTable>
      </section>

      <section v-else>
        <DataTable :rows="tokens.data.value" :columns="tokenCols" row-key="id" :loading="tokens.loading.value" :error="tokens.error.value" :retry="tokens.refresh" storage-key="me-tokens" csv-name="api-tokens" empty-title="No API tokens">
          <template #toolbar><button type="button" class="btn btn-sm btn-primary" data-testid="token-new" @click="openToken"><Icon name="plus" :size="14" />New token</button></template>
          <template #cell-created_at="{ row }"><AbsoluteTime :time="row.created_at" /></template>
          <template #cell-last_used_at="{ row }"><RelativeTime :time="row.last_used_at" never="never used" /></template>
          <template #cell-expires_at="{ row }"><AbsoluteTime :time="row.expires_at" never="never" /></template>
          <template #cell-actions="{ row }"><button type="button" class="btn btn-sm" @click="revokeToken(row)">Revoke</button></template>
          <template #empty><div class="tok-empty">Tokens let scripts call the API as you (<code>Authorization: Bearer ibt_…</code>) with a role up to yours.</div></template>
        </DataTable>
      </section>
    </template>

    <Modal :open="disable.open" title="Turn off two-factor authentication?" description="Signing in will then need only your password." @update:open="(v) => (disable.open = v)">
      <form id="disable-form" class="form-grid" @submit.prevent="disableTOTP">
        <FormField v-slot="f" label="Your password" :error="disable.error"><PasswordInput :id="f.id" v-model="disable.password" autocomplete="current-password" :invalid="f.invalid" /></FormField>
      </form>
      <template #footer>
        <button type="button" class="btn" @click="disable.open = false">Cancel</button>
        <button type="submit" form="disable-form" class="btn btn-danger" :disabled="!disable.password || disable.busy" data-testid="totp-disable-confirm"><Spinner v-if="disable.busy" :size="13" />Turn off</button>
      </template>
    </Modal>

    <Modal :open="nt.open" :title="nt.secret ? 'Token created' : 'New API token'" @update:open="(v) => (nt.open = v)">
      <div v-if="nt.secret" class="form-grid" data-testid="token-secret">
        <CopyField :value="nt.secret" label="Token" once />
        <p class="txt small">Use it as <code>Authorization: Bearer &lt;token&gt;</code>. Bearer requests need no CSRF header.</p>
      </div>
      <form v-else id="token-form" class="form-grid" @submit.prevent="createToken">
        <FormField v-slot="f" label="Name" hint="What uses it, e.g. “monitoring” or “ci-restore-test”."><input :id="f.id" v-model="nt.name" class="input" required data-testid="token-name" /></FormField>
        <FormField v-slot="f" label="Role" hint="At most your own role."><SelectInput :id="f.id" v-model="nt.role" :options="roleOptions" /></FormField>
        <FormField v-slot="f" label="Expires after"><SelectInput :id="f.id" v-model="nt.ttl" :options="[{ value: '24', label: '1 day' }, { value: '168', label: '7 days' }, { value: '720', label: '30 days' }, { value: '2160', label: '90 days' }, { value: '8760', label: '1 year (the longest)' }]" /></FormField>
        <fieldset class="tok-repos" data-testid="token-repos">
          <legend>Repositories</legend>
          <label><input v-model="nt.restrict" type="checkbox" data-testid="token-restrict" /> Only these repositories (the token then has no rights outside them, not even read-only settings)</label>
          <div v-if="nt.restrict" class="tok-repo-list">
            <label v-for="r in repos.list" :key="r.name"><input v-model="nt.repos" type="checkbox" :value="r.name" :data-testid="`token-repo-${r.name}`" /> <span class="mono">{{ r.name }}</span></label>
            <p v-if="!nt.repos.length" class="txt small">Pick at least one repository.</p>
          </div>
        </fieldset>
        <FormField v-if="nt.ask" v-slot="f" label="Your password" hint="Creating a token needs your password when you signed in more than 10 minutes ago."><PasswordInput :id="f.id" v-model="nt.password" autocomplete="current-password" data-testid="token-password" /></FormField>
        <p v-if="nt.error" class="err" role="alert">{{ nt.error }}</p>
      </form>
      <template #footer>
        <template v-if="nt.secret"><button type="button" class="btn btn-primary" @click="nt.open = false">Done</button></template>
        <template v-else>
          <button type="button" class="btn" @click="nt.open = false">Cancel</button>
          <button type="submit" form="token-form" class="btn btn-primary" :disabled="!nt.name.trim() || nt.busy || (nt.ask && !nt.password) || (nt.restrict && !nt.repos.length)" data-testid="token-create"><Spinner v-if="nt.busy" :size="13" />Create token</button>
        </template>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.profile { display: flex; flex-direction: column; gap: 16px; max-width: 1120px; }
.page-head { margin-bottom: 4px; }
.acct { display: flex; gap: 20px; padding: 16px 20px; align-items: flex-start; }
.acct :deep(.kv) { flex: 1; }
.avatar { display: grid; place-items: center; width: 48px; height: 48px; border-radius: 50%; background: var(--accent-soft); color: var(--accent-text); font-size: 20px; font-weight: var(--fw-semibold); flex: none; }
.grid2 { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1.2fr); gap: 16px; align-items: start; }
.txt { color: var(--text-2); margin-bottom: 14px; }
.err { color: var(--critical-text); font-size: var(--fs-sm); }
.enrol { display: grid; grid-template-columns: 180px 1fr; gap: 20px; align-items: start; }
.qr { width: 180px; height: 180px; padding: 8px; background: #fff; border: 1px solid var(--border); border-radius: var(--r); }
.qr :deep(svg) { width: 100%; height: 100%; display: block; }
.enrol-side { display: flex; flex-direction: column; gap: 14px; min-width: 0; }
.enrol-side .txt { margin: 0; }
.code { font-size: 18px; letter-spacing: .25em; width: 160px; }
.codes-step { display: flex; flex-direction: column; gap: 14px; }
.codes { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 6px 24px; margin: 0; padding: 12px 12px 12px 36px; background: var(--code-bg); border: 1px solid var(--border); border-radius: var(--r); }
.codes code { font-size: 14px; letter-spacing: .04em; }
.ack { display: flex; gap: 8px; align-items: center; font-size: var(--fs-sm); }
.ack input { accent-color: var(--accent); }
.tok-empty { padding: 20px; text-align: center; color: var(--text-muted); font-size: var(--fs-sm); }
.keys-panel { grid-column: 1 / -1; }
@media (max-width: 1000px) { .grid2 { grid-template-columns: 1fr; } }
@media (max-width: 560px) { .enrol { grid-template-columns: 1fr; } .acct { flex-direction: column; } .codes { grid-template-columns: 1fr; } }
.tok-repos { border: 1px solid var(--border); border-radius: var(--r); padding: 8px 12px 10px; margin: 0; display: grid; gap: 6px; font-size: var(--fs-sm); }
.tok-repos legend { font-weight: var(--fw-medium); padding: 0 4px; }
.tok-repo-list { display: flex; flex-wrap: wrap; gap: 6px 16px; }
</style>
