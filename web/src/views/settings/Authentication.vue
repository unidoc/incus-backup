<script setup lang="ts">
// Settings → Authentication (administrators): how people sign in.
//  * Sign-in policy: require a second factor (none | administrators | everyone); turn password and
//    passkey sign-in off when single sign-on is the only way in (guarded: an administrator must be able
//    to sign in through an identity provider; the server CLI stays the break-glass path).
//  * Passkeys: the relying party (domain) and origins in effect, overrides, and a self-check of this page.
//  * Identity providers (OpenID Connect): add, edit, test discovery, delete; group → role and
//    group → repository mappings. The client secret is write-only.
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import Modal from '@/components/Modal.vue'
import Spinner from '@/components/Spinner.vue'
import Callout from '@/components/Callout.vue'
import StatusPill from '@/components/StatusPill.vue'
import EmptyState from '@/components/EmptyState.vue'
import CopyField from '@/components/CopyField.vue'
import KeyValue from '@/components/KeyValue.vue'
import FormField from '@/components/form/FormField.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import TagInput from '@/components/form/TagInput.vue'
import PasswordInput from '@/components/form/PasswordInput.vue'
import { useResource } from '@/api/cache'
import { authn, type AuthSettings, type DiscoveryReport, type OIDCProvider, type OIDCProviderInput, type Require2FA } from '@/api/sections/authn'
import { repos } from '@/stores/repos'
import { toast, errorText } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import { unavailableReason } from '@/lib/webauthn'
import { session, refreshUser } from '@/stores/session'
import '@/components/config/section.css'

const router = useRouter()
const settings = useResource(() => 'authn:settings', (s) => authn.settings(s))
const providers = useResource(() => 'authn:providers', (s) => authn.providers(s))
const st = computed<AuthSettings | undefined>(() => settings.data.value)
const plist = computed(() => providers.data.value ?? [])

// ---- policy
const policyBusy = ref(false)
const policyError = ref('')
const r2fa = [
  { value: 'none', label: 'Not required', help: 'Each person decides. Recommended at least for administrators.' },
  { value: 'admin', label: 'Administrators', help: 'Administrators must use a security key or an authenticator app.' },
  { value: 'all', label: 'Everyone', help: 'Every person who signs in with a password must use a second factor.' },
] as const
async function savePolicy(u: { require_2fa?: Require2FA; local_login?: boolean }) {
  policyBusy.value = true
  policyError.value = ''
  try {
    settings.data.value = await authn.saveSettings(u)
    toast.success('Sign-in policy saved')
  } catch (e) { policyError.value = errorText(e) } finally { policyBusy.value = false }
}
async function setRequire(v: Require2FA) {
  if (st.value?.require_2fa === v) return
  if (v !== 'none' && !(await confirm({ title: v === 'all' ? 'Require a second factor of everyone?' : 'Require a second factor of administrators?', message: 'People it covers who have none are asked to set one up at their next request, and can do nothing else until they have. This includes you if you have none.', confirmLabel: 'Require it' }))) return
  await savePolicy({ require_2fa: v })
  // the policy may cover this session too: then only enrolment answers until a factor is set up
  if (!policyError.value) {
    await refreshUser()
    if (session.enrol2fa) await router.replace({ name: 'enrol-2fa', query: { next: '/settings/authentication' } })
  }
}
async function setLocal(v: boolean) {
  if (!v && !(await confirm({ title: 'Turn off password and passkey sign-in?', message: 'Only single sign-on works on the web console then. Sessions already open continue. If the identity provider fails, run `incus-backup-server user local-login on` on the server to turn it back on.', confirmLabel: 'Turn off', danger: true }))) return
  await savePolicy({ local_login: v })
}

// ---- passkeys
const rp = ref({ id: '', origins: [] as string[], busy: false, error: '' })
const here = typeof location !== 'undefined' ? location.origin : ''
const blocked = unavailableReason()
const platform = ref<boolean | null>(null)
onMounted(async () => {
  try { platform.value = await PublicKeyCredential.isUserVerifyingPlatformAuthenticatorAvailable() } catch { platform.value = null }
})
let loadedRP = false
watch(st, (v) => {
  if (v && !loadedRP) { rp.value.id = v.webauthn_rp_id; rp.value.origins = [...(v.webauthn_origins ?? [])]; loadedRP = true }
}, { immediate: true })
const originOK = computed(() => !!st.value && st.value.webauthn_origins_effective.includes(here))
async function saveRP() {
  rp.value.busy = true
  rp.value.error = ''
  try {
    settings.data.value = await authn.saveSettings({ webauthn_rp_id: rp.value.id.trim(), webauthn_origins: rp.value.origins })
    toast.success('Passkey settings saved', settings.data.value.webauthn_error ? 'Passkeys are still not usable: see the check below.' : undefined)
  } catch (e) { rp.value.error = errorText(e) } finally { rp.value.busy = false }
}
// (URLs in static component props trip vue-tsc: bound as expressions)
const originExample = 'https://backup.example.com'
const issuerHint = 'Keycloak: https://id.example.com/realms/REALM · Authentik: https://auth.example.com/application/o/SLUG/ · Google: https://accounts.google.com'
const originRule = (v: string) => (/^https:\/\/[^/\s]+$|^http:\/\/localhost(:\d+)?$/.test(v.replace(/\/$/, '')) ? '' : 'An origin looks like https://backup.example.com[:port]')

// ---- providers
type Editor = Omit<OIDCProviderInput, 'client_secret'> & { client_secret: string; isNew: boolean; has_secret: boolean; busy: boolean; error: string }
const roleOpts = [{ value: 'none', label: 'No access' }, { value: 'readonly', label: 'Read-only' }, { value: 'restore', label: 'Restore' }, { value: 'operator', label: 'Operator' }]
const mapRoleOpts = [{ value: 'readonly', label: 'Read-only' }, { value: 'restore', label: 'Restore' }, { value: 'operator', label: 'Operator' }, { value: 'admin', label: 'Administrator' }]
const ed = ref<Editor | null>(null)
function openNew() {
  ed.value = { isNew: true, has_secret: false, busy: false, error: '', name: '', display_name: '', issuer: '', client_id: '', client_secret: '', scopes: ['openid', 'email', 'profile'], allowed_domains: [], groups_claim: 'groups', default_role: 'none', role_mappings: [], scope_mappings: [], auto_create: false, enabled: true }
}
function openEdit(p: OIDCProvider) {
  ed.value = { isNew: false, has_secret: p.has_secret, busy: false, error: '', name: p.name, display_name: p.display_name, issuer: p.issuer, client_id: p.client_id, client_secret: '', clear_secret: false, scopes: [...p.scopes], allowed_domains: [...(p.allowed_domains ?? [])], groups_claim: p.groups_claim, default_role: p.default_role, role_mappings: p.role_mappings.map((m) => ({ ...m })), scope_mappings: p.scope_mappings.map((m) => ({ ...m })), auto_create: p.auto_create, enabled: p.enabled }
}
const redirectFor = (name: string) => (st.value?.oidc_redirect_base ? `${st.value.oidc_redirect_base}${name || '<name>'}/callback` : '')
const edOK = computed(() => !!ed.value && /^[a-z0-9][a-z0-9-]{0,31}$/.test(ed.value.name) && /^https?:\/\/\S+$/.test(ed.value.issuer) && !!ed.value.client_id.trim()
  && ed.value.role_mappings.every((m) => m.group.trim()) && ed.value.scope_mappings.every((m) => m.group.trim() && m.repository))
async function saveProvider() {
  const e = ed.value
  if (!e) return
  e.busy = true
  e.error = ''
  const { isNew, has_secret: _h, busy: _b, error: _e, ...body } = e
  void _h; void _b; void _e
  try {
    const p = isNew ? await authn.createProvider(body) : await authn.updateProvider(e.name, body)
    ed.value = null
    toast.success(isNew ? 'Identity provider added' : 'Identity provider saved', isNew ? 'Checking that the server can reach it.' : undefined)
    void providers.refresh()
    void settings.refresh()
    if (isNew) void test(p)
  } catch (x) { e.error = errorText(x) } finally { if (ed.value) ed.value.busy = false }
}
async function removeProvider(p: OIDCProvider) {
  if (!(await confirm({ title: `Remove ${p.display_name || p.name}?`, message: 'Its links to users and its client secret are deleted. Users created through it stay, but cannot sign in through it any more.', confirmLabel: 'Remove provider', danger: true, typeToConfirm: p.name }))) return
  try { await authn.deleteProvider(p.name); toast.success('Identity provider removed'); void providers.refresh(); void settings.refresh() } catch (e) { toast.error(e, 'Could not remove the provider') }
}
const tests = ref<Record<string, { busy: boolean; r?: DiscoveryReport; error?: string }>>({})
async function test(p: OIDCProvider) {
  tests.value[p.name] = { busy: true }
  try { tests.value[p.name] = { busy: false, r: await authn.testProvider(p.name) } } catch (e) { tests.value[p.name] = { busy: false, error: errorText(e) } }
}
const repoOpts = computed(() => repos.list.map((r) => ({ value: r.name, label: r.name })))
</script>

<template>
  <div class="authp">
    <header class="page-head">
      <div>
        <h1>Authentication</h1>
        <p class="lede">How people sign in: passwords with a second factor, passkeys and security keys, and single sign-on through your identity provider. Every method ends in the same session, roles and repository access.</p>
      </div>
      <div class="actions"><RouterLink class="btn" :to="{ name: 'users' }"><Icon name="users" :size="14" />Users</RouterLink></div>
    </header>

    <Callout v-if="settings.error.value" status="critical" title="Could not load the sign-in settings" :retry="settings.refresh">{{ settings.error.value.message }}</Callout>

    <div class="grid">
      <section class="panel" data-testid="policy-panel">
        <div class="panel-head"><h2>Sign-in policy</h2></div>
        <div class="panel-body stack">
          <div>
            <h3>Second factor required for</h3>
            <div class="cx-seg" role="group" aria-label="Second factor required for">
              <button v-for="o in r2fa" :key="o.value" type="button" :aria-pressed="st?.require_2fa === o.value" :disabled="policyBusy || !st" :data-testid="`require-${o.value}`" @click="setRequire(o.value)">{{ o.label }}</button>
            </div>
            <p class="cx-help">{{ r2fa.find((o) => o.value === st?.require_2fa)?.help }} Single sign-on users get their second factor from the identity provider. A person without one is asked to set it up right after signing in.</p>
          </div>
          <div>
            <h3>Password and passkey sign-in</h3>
            <SwitchInput :model-value="st?.local_login ?? true" :disabled="policyBusy || !st || (!!st.local_login && !st.sso_admins.length)" label="Allow sign-in with a password or a passkey" data-testid="local-login" @update:model-value="setLocal" />
            <p v-if="st && st.local_login && !st.sso_admins.length" class="cx-help">To allow single sign-on only, an administrator must first sign in through an identity provider once (an administrator role mapping), so nobody is locked out.</p>
            <p v-else-if="st" class="cx-help">Administrators who sign in through an identity provider: <strong>{{ st.sso_admins.join(', ') || 'none' }}</strong>. Break-glass on the server: <code>incus-backup-server user local-login on</code> and <code>user set-password</code>.</p>
          </div>
          <Callout v-if="policyError" status="critical" title="Could not save the policy">{{ policyError }}</Callout>
        </div>
      </section>

      <section class="panel" data-testid="passkey-panel">
        <div class="panel-head"><h2>Passkeys and security keys</h2><StatusPill :status="st?.webauthn_error ? 'warn' : 'ok'" :label="st?.webauthn_error ? 'not usable' : 'ready'" size="sm" /></div>
        <div class="panel-body stack">
          <KeyValue v-if="st" :columns="1" :items="[
            { key: 'rp', label: 'Passkey domain (RP ID)', value: st.webauthn_rp_id_effective || '—', mono: true },
            { key: 'o', label: 'Allowed origins', value: st.webauthn_origins_effective.join(', ') || '—', mono: true },
          ]" />
          <ul class="checks" data-testid="passkey-checks">
            <li><StatusPill :status="st?.webauthn_error ? 'critical' : 'ok'" variant="dot" :label="st?.webauthn_error || 'The server can run passkey ceremonies.'" /></li>
            <li><StatusPill :status="originOK ? 'ok' : 'warn'" variant="dot" :label="originOK ? `This page (${here}) is an allowed origin.` : `This page (${here}) is not an allowed origin: passkeys work only at ${st?.webauthn_origins_effective.join(', ') || 'the console address'}.`" /></li>
            <li><StatusPill :status="blocked ? 'warn' : 'ok'" variant="dot" :label="blocked || 'This browser supports passkeys on this page.'" /></li>
            <li v-if="platform !== null"><StatusPill :status="platform ? 'ok' : 'neutral'" variant="dot" :label="platform ? 'This device has a built-in authenticator (Touch ID, Windows Hello, Android).' : 'No built-in authenticator on this device: use a security key or a phone.'" /></li>
          </ul>
          <details class="adv">
            <summary>Override the domain or origins</summary>
            <p class="cx-help">By default the console address (base_url, <RouterLink :to="{ name: 'settings' }">Server settings</RouterLink>) is the only origin and its host the domain. Changing the domain later makes registered keys stop working.</p>
            <form class="form-grid" @submit.prevent="saveRP">
              <FormField v-slot="f" label="Passkey domain (RP ID)" optional hint="The host of the console or a parent domain, e.g. example.com"><input :id="f.id" v-model="rp.id" class="input mono" placeholder="derived from base_url" data-testid="rp-id" /></FormField>
              <FormField v-slot="g" label="Allowed origins" optional><TagInput :id="g.id" v-model="rp.origins" :placeholder="originExample" :validate="originRule" /></FormField>
              <Callout v-if="rp.error" status="critical" title="Could not save">{{ rp.error }}</Callout>
              <div class="form-actions"><button type="submit" class="btn" :disabled="rp.busy"><Spinner v-if="rp.busy" :size="13" />Save passkey settings</button></div>
            </form>
          </details>
        </div>
      </section>
    </div>

    <section class="panel" data-testid="providers-panel">
      <div class="panel-head"><h2>Identity providers (single sign-on)</h2><button type="button" class="btn btn-sm btn-primary" data-testid="provider-new" @click="openNew"><Icon name="plus" :size="14" />Add a provider</button></div>
      <div class="panel-body">
        <div v-if="providers.loading.value && !providers.data.value" class="row"><Spinner /> Loading</div>
        <Callout v-else-if="providers.error.value" status="critical" title="Could not load the providers" :retry="providers.refresh">{{ providers.error.value.message }}</Callout>
        <EmptyState v-else-if="!plist.length" title="No identity provider" icon="key-round">Add Keycloak, Authentik, Entra ID, Google or any OpenID Connect provider so people sign in with their organisation account.</EmptyState>
        <ul v-else class="provs">
          <li v-for="p in plist" :key="p.id" :data-testid="`provider-${p.name}`">
            <div class="ph">
              <div class="cx-title"><span class="t">{{ p.display_name || p.name }} <StatusPill :status="p.enabled ? 'ok' : 'neutral'" :label="p.enabled ? 'enabled' : 'disabled'" size="sm" /></span><span class="d mono">{{ p.issuer }}</span></div>
              <span class="cx-actions">
                <button type="button" class="btn btn-sm" :disabled="tests[p.name]?.busy" :data-testid="`provider-test-${p.name}`" @click="test(p)"><Spinner v-if="tests[p.name]?.busy" :size="12" />Test</button>
                <button type="button" class="btn btn-sm btn-ghost" @click="openEdit(p)"><Icon name="pencil" :size="14" />Edit</button>
                <button type="button" class="btn btn-sm btn-ghost btn-icon" :aria-label="`Remove ${p.name}`" @click="removeProvider(p)"><Icon name="trash-2" :size="14" /></button>
              </span>
            </div>
            <div class="facts">
              <span class="cx-tag mono">{{ p.name }}</span>
              <span class="cx-tag">default role: {{ p.default_role }}</span>
              <span class="cx-tag" :class="{ warn: p.auto_create }">{{ p.auto_create ? 'creates users' : 'linked users only' }}</span>
              <span v-if="p.role_mappings.length" class="cx-tag info">{{ p.role_mappings.length }} role mapping{{ p.role_mappings.length > 1 ? 's' : '' }}</span>
              <span v-if="p.scope_mappings.length" class="cx-tag info">restricted to mapped repositories</span>
              <span v-if="p.allowed_domains.length" class="cx-tag">{{ p.allowed_domains.join(', ') }}</span>
              <span class="cx-tag" :class="{ warn: !p.has_secret }">{{ p.has_secret ? 'client secret stored' : 'public client (PKCE only)' }}</span>
            </div>
            <div v-if="tests[p.name]?.r || tests[p.name]?.error" class="test" :data-testid="`provider-result-${p.name}`">
              <Callout v-if="tests[p.name]?.error" status="critical" title="Test failed">{{ tests[p.name]?.error }}</Callout>
              <template v-else-if="tests[p.name]?.r">
                <Callout :status="tests[p.name]!.r!.ok ? (tests[p.name]!.r!.warnings.length ? 'warn' : 'ok') : 'critical'" :title="tests[p.name]!.r!.ok ? 'Discovery and key set reachable' : 'The provider is not usable yet'">
                  <span v-if="tests[p.name]!.r!.error">{{ tests[p.name]!.r!.error }}</span>
                  <span v-else-if="tests[p.name]!.r!.jwks_error">Key set: {{ tests[p.name]!.r!.jwks_error }}</span>
                  <span v-else>Issuer {{ tests[p.name]!.r!.issuer }} · {{ tests[p.name]!.r!.jwks_keys }} signing key{{ tests[p.name]!.r!.jwks_keys > 1 ? 's' : '' }} · PKCE {{ tests[p.name]!.r!.code_challenge_methods_supported?.join(', ') || 'not listed' }}</span>
                  <ul v-if="tests[p.name]!.r!.warnings.length" class="warns"><li v-for="w in tests[p.name]!.r!.warnings" :key="w">{{ w }}</li></ul>
                </Callout>
              </template>
            </div>
          </li>
        </ul>
        <p class="cx-help hint">The redirect URI to register at the provider is <code>{{ st?.oidc_redirect_base ? `${st.oidc_redirect_base}<name>/callback` : 'unknown until the console address (base_url) is set' }}</code>. Users are matched by the provider's subject id, never by e-mail alone; roles and repository access are recomputed from the token at every sign-in.</p>
      </div>
    </section>

    <Modal :open="!!ed" :title="ed?.isNew ? 'Add an identity provider' : `Edit ${ed?.name}`" width="720px" drawer @update:open="(v: boolean) => { if (!v) ed = null }">
      <form v-if="ed" id="prov-form" class="cx-form" data-testid="provider-form" @submit.prevent="saveProvider">
        <div class="cx-cols">
          <FormField v-slot="f" label="Name" hint="Lower case; part of the redirect URI; cannot change"><input :id="f.id" v-model="ed.name" class="input mono" :disabled="!ed.isNew" placeholder="corp" data-testid="prov-name" /></FormField>
          <FormField v-slot="f" label="Button label" optional><input :id="f.id" v-model="ed.display_name" class="input" placeholder="Corp SSO" data-testid="prov-label" /></FormField>
        </div>
        <FormField v-slot="f" label="Issuer URL" :hint="issuerHint"><input :id="f.id" v-model="ed.issuer" class="input mono" placeholder="https://id.example.com/realms/main" data-testid="prov-issuer" /></FormField>
        <div class="cx-cols">
          <FormField v-slot="f" label="Client ID"><input :id="f.id" v-model="ed.client_id" class="input mono" data-testid="prov-client" /></FormField>
          <FormField v-slot="f" label="Client secret" :hint="ed.has_secret ? 'Stored. Leave empty to keep it.' : 'Optional for a public client (PKCE only)'" optional><PasswordInput :id="f.id" v-model="ed.client_secret" autocomplete="new-password" :placeholder="ed.has_secret ? '•••••••• (stored)' : ''" data-testid="prov-secret" /></FormField>
        </div>
        <CopyField v-if="redirectFor(ed.name)" :value="redirectFor(ed.name)" label="Redirect URI to register at the provider" />
        <Callout v-else status="warn" title="Console address not set">Set base_url in <RouterLink :to="{ name: 'settings' }">Server settings</RouterLink> first: the redirect URI is built from it.</Callout>
        <div class="cx-cols">
          <FormField v-slot="f" label="Scopes"><TagInput :id="f.id" v-model="ed.scopes" placeholder="openid" /></FormField>
          <FormField v-slot="f" label="Groups claim" hint="Claim that lists the user's groups"><input :id="f.id" v-model="ed.groups_claim" class="input mono" /></FormField>
        </div>
        <FormField v-slot="f" label="Allowed e-mail domains" optional hint="Empty: any verified address of this provider"><TagInput :id="f.id" v-model="ed.allowed_domains" placeholder="example.com" /></FormField>
        <div class="cx-cols">
          <FormField v-slot="f" label="Default role" hint="Never administrator: map a group for that"><SelectInput :id="f.id" v-model="ed.default_role" :options="roleOpts" data-testid="prov-default-role" /></FormField>
          <div class="sw-stack">
            <SwitchInput v-model="ed.auto_create" label="Create users at their first sign-in" data-testid="prov-autocreate" />
            <SwitchInput v-model="ed.enabled" label="Enabled (shown on the sign-in page)" />
          </div>
        </div>

        <fieldset class="maps">
          <legend>Group → role</legend>
          <p class="cx-help">The highest role of every matching group, or the default role. Recomputed at every sign-in.</p>
          <div v-for="(m, i) in ed.role_mappings" :key="`r${i}`" class="maprow">
            <input v-model="m.group" class="input" placeholder="group, e.g. backup-admins" :aria-label="`Group ${i + 1}`" />
            <SelectInput v-model="m.role" :options="mapRoleOpts" :aria-label="`Role for group ${i + 1}`" />
            <button type="button" class="btn btn-ghost btn-icon btn-sm" :aria-label="`Remove mapping ${i + 1}`" @click="ed.role_mappings.splice(i, 1)"><Icon name="x" :size="14" /></button>
          </div>
          <button type="button" class="btn btn-sm" data-testid="prov-add-role" @click="ed.role_mappings.push({ group: '', role: 'readonly' })"><Icon name="plus" :size="13" />Add a role mapping</button>
        </fieldset>
        <fieldset class="maps">
          <legend>Group → repository</legend>
          <p class="cx-help">With at least one row, users of this provider see only the repositories of their groups (none if no group matches); administrators always see all.</p>
          <div v-for="(m, i) in ed.scope_mappings" :key="`s${i}`" class="maprow three">
            <input v-model="m.group" class="input" placeholder="group" :aria-label="`Group ${i + 1}`" />
            <SelectInput v-model="m.repository" :options="repoOpts" placeholder="repository" :aria-label="`Repository for group ${i + 1}`" />
            <SelectInput v-model="m.role" :options="mapRoleOpts.filter((o) => o.value !== 'admin')" :aria-label="`Role on the repository ${i + 1}`" />
            <button type="button" class="btn btn-ghost btn-icon btn-sm" :aria-label="`Remove repository mapping ${i + 1}`" @click="ed.scope_mappings.splice(i, 1)"><Icon name="x" :size="14" /></button>
          </div>
          <button type="button" class="btn btn-sm" @click="ed.scope_mappings.push({ group: '', repository: repoOpts[0]?.value ?? '', role: 'readonly' })"><Icon name="plus" :size="13" />Add a repository mapping</button>
        </fieldset>
        <Callout v-if="ed.error" status="critical" title="Could not save the provider">{{ ed.error }}</Callout>
      </form>
      <template #footer>
        <button type="button" class="btn" @click="ed = null">Cancel</button>
        <button type="submit" form="prov-form" class="btn btn-primary" :disabled="!edOK || ed?.busy" data-testid="prov-save"><Spinner v-if="ed?.busy" :size="13" />{{ ed?.isNew ? 'Add provider' : 'Save' }}</button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.authp { display: flex; flex-direction: column; gap: 16px; max-width: 1200px; }
.grid { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: 16px; align-items: start; }
.stack { display: grid; gap: 18px; }
h3 { font-size: var(--fs-sm); font-weight: var(--fw-semibold); margin: 0 0 8px; color: var(--text); }
.stack .cx-help { margin-top: 8px; }
.checks { list-style: none; margin: 0; padding: 0; display: grid; gap: 6px; font-size: var(--fs-sm); }
.checks :deep(.pill) { white-space: normal; line-height: 1.4; align-items: flex-start; }
.checks :deep(.dot) { flex: none; margin-top: 5px; }
.stack :deep(dd) { overflow-wrap: anywhere; min-width: 0; }
.adv summary { cursor: pointer; color: var(--accent-text); font-size: var(--fs-sm); }
.adv[open] summary { margin-bottom: 10px; }
.adv .form-grid { margin-top: 10px; }
.row { display: flex; gap: 8px; align-items: center; color: var(--text-muted); }
.provs { list-style: none; margin: 0; padding: 0; display: grid; gap: 0; border: 1px solid var(--border); border-radius: var(--r); }
.provs li { padding: 12px 14px; display: grid; gap: 8px; }
.provs li + li { border-top: 1px solid var(--border); }
.ph { display: flex; justify-content: space-between; gap: 12px; align-items: flex-start; flex-wrap: wrap; }
.facts { display: flex; flex-wrap: wrap; gap: 6px; }
.test .warns { margin: 6px 0 0; padding-left: 18px; }
.hint { margin-top: 12px; overflow-wrap: anywhere; }
.panel-head > .btn { margin-left: auto; }
.maps { border: 1px solid var(--border); border-radius: var(--r); padding: 10px 12px 12px; margin: 0; display: grid; gap: 8px; }
.maps legend { font-weight: var(--fw-medium); padding: 0 4px; font-size: var(--fs-sm); }
.maprow { display: grid; grid-template-columns: minmax(0, 1.4fr) minmax(0, 1fr) auto; gap: 8px; align-items: center; }
.maprow.three { grid-template-columns: minmax(0, 1.2fr) minmax(0, 1fr) minmax(0, 0.9fr) auto; }
.maps > .btn { justify-self: start; }
.sw-stack { display: grid; gap: 10px; align-content: center; }
.mono { font-family: var(--font-mono); }
@media (max-width: 1000px) { .grid { grid-template-columns: minmax(0, 1fr); } }
@media (max-width: 560px) { .maprow, .maprow.three { grid-template-columns: minmax(0, 1fr) auto; } .maprow > :nth-child(2), .maprow.three > :nth-child(3) { grid-column: 1; } }
</style>
