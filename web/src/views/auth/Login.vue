<script setup lang="ts">
// Sign in. Methods (docs/authentication.md):
//  * name + password, then a second factor when the user has one: the security key (WebAuthn, started at
//    once) or the 6-digit code / a recovery code. The password stays in memory only for the code step.
//  * "Sign in with a passkey": a discoverable credential, no name typed.
//  * single sign-on buttons: a plain link (a top-level navigation) to the identity provider.
// The server says which methods are on (GET /auth/oidc/providers); a failed single sign-on comes back
// as ?sso_error=<word>.
import { ref, computed, nextTick, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AuthLayout from '@/components/app/AuthLayout.vue'
import FormField from '@/components/form/FormField.vue'
import PasswordInput from '@/components/form/PasswordInput.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import Icon from '@/components/icons/Icon.vue'
import { login, adoptSession, session } from '@/stores/session'
import { signInError } from '@/lib/authErrors'
import { authn, ssoErrorText, type SignInOptions } from '@/api/sections/authn'
import { getAssertion, unavailableReason, webauthnError } from '@/lib/webauthn'
import { ApiError } from '@/api/client'

const route = useRoute()
const router = useRouter()
const name = ref('')
const password = ref('')
const code = ref('')
const step = ref<'password' | 'code' | 'key'>('password')
const methods = ref({ totp: false, webauthn: false })
const ticket = ref('')
const recovery = ref(false)
const busy = ref(false)
const keyBusy = ref(false)
const error = ref('')
const codeInput = ref<HTMLInputElement | null>(null)
const options = ref<SignInOptions | null>(null)
const optionsError = ref(false)
const expired = computed(() => route.query.expired === '1' || session.expired)
const next = computed(() => (typeof route.query.next === 'string' && route.query.next.startsWith('/') && !route.query.next.startsWith('//') ? route.query.next : '/'))
const ssoError = computed(() => (typeof route.query.sso_error === 'string' ? ssoErrorText(route.query.sso_error) : ''))
const signedOut = computed(() => route.query.reason === 'signed_out')
const localLogin = computed(() => options.value?.local_login ?? true)
const providers = computed(() => options.value?.providers ?? [])
const passkeyBlocked = unavailableReason()
const showPasskey = computed(() => localLogin.value && !!options.value?.passkeys)
const explain = (e: unknown) => signInError(e, step.value === 'code' ? 'code' : 'password')

onMounted(async () => {
  try { options.value = await authn.signInOptions() } catch { optionsError.value = true }
})

async function done() {
  password.value = ''
  await router.replace(next.value)
}

async function submit() {
  if (busy.value) return
  error.value = ''
  busy.value = true
  try {
    const res = await login({ name: name.value.trim(), password: password.value, code: step.value === 'code' ? code.value.trim() : undefined })
    if (!res.ok) {
      methods.value = res.methods
      ticket.value = res.ticket
      if (step.value === 'password' && res.methods.webauthn && res.ticket) {
        step.value = 'key'
        void useKey()
        return
      }
      step.value = 'code'
      await nextTick()
      codeInput.value?.focus()
      return
    }
    await done()
  } catch (e) {
    error.value = localOffText(e) ?? explain(e)
    if (step.value === 'code') code.value = ''
  } finally {
    busy.value = false
  }
}

function localOffText(e: unknown): string | null {
  if (e instanceof ApiError && e.status === 403 && (e.body as { local_login_disabled?: boolean } | null)?.local_login_disabled) {
    if (options.value) options.value = { ...options.value, local_login: false }
    return 'Password and passkey sign-in are turned off on this server. Use single sign-on.'
  }
  return null
}

// the security key as the second factor (ticket from the password step)
async function useKey() {
  if (keyBusy.value) return
  error.value = ''
  keyBusy.value = true
  try {
    const start = await authn.beginLogin(ticket.value)
    const cred = await getAssertion(start.options)
    adoptSession(await authn.finishLogin(start.ceremony, cred))
    await done()
  } catch (e) {
    error.value = keyError(e)
    if (e instanceof ApiError && e.status === 401 && /password again/i.test(e.message)) back()
  } finally {
    keyBusy.value = false
  }
}

// sign-in with a passkey (no name)
async function usePasskey() {
  if (keyBusy.value) return
  error.value = ''
  keyBusy.value = true
  try {
    const start = await authn.beginLogin()
    const cred = await getAssertion(start.options)
    adoptSession(await authn.finishLogin(start.ceremony, cred))
    await done()
  } catch (e) {
    error.value = keyError(e)
  } finally {
    keyBusy.value = false
  }
}

function keyError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 429) return 'Too many failed attempts. Sign-in for this account is locked for a few minutes (or this address tried too often). Wait, then try again.'
    return localOffText(e) ?? e.message
  }
  return webauthnError(e, 'sign-in')
}

function back() { step.value = 'password'; code.value = ''; error.value = ''; recovery.value = false; ticket.value = '' }
function toCode() { step.value = 'code'; error.value = ''; void nextTick(() => codeInput.value?.focus()) }
function toKey() { step.value = 'key'; error.value = ''; void useKey() }
function onCodeInput(e: Event) {
  const v = (e.target as HTMLInputElement).value
  code.value = recovery.value ? v.toLowerCase().replace(/[^a-z0-9-]/g, '').slice(0, 11) : v.replace(/\D/g, '').slice(0, 6)
  if (!recovery.value && code.value.length === 6) void submit()
}
const title = computed(() => (step.value === 'password' ? 'Sign in' : 'Two-factor authentication'))
const lede = computed(() => {
  if (step.value === 'key') return 'Confirm with your security key or passkey.'
  if (step.value === 'code') return recovery.value ? 'Enter one of your recovery codes. Each code works once.' : 'Enter the 6-digit code from your authenticator app.'
  return undefined
})
</script>

<template>
  <AuthLayout :title="title" :lede="lede">
    <Callout v-if="ssoError && step === 'password'" status="critical" title="Single sign-on did not work" data-testid="sso-error">{{ ssoError }}</Callout>
    <Callout v-else-if="signedOut && step === 'password'" status="warn" title="You were signed out" data-testid="signed-out">This session was ended on the server: you signed out in another tab, an administrator revoked the session or disabled the account, or the password changed. Sign in again to continue where you were.</Callout>
    <Callout v-else-if="expired && step === 'password'" status="warn" title="Your session ended" data-testid="session-expired">Sign in again to continue where you were.</Callout>

    <template v-if="step === 'password'">
      <form v-if="localLogin" class="form-grid" data-testid="login-form" @submit.prevent="submit">
        <FormField v-slot="f" label="User name">
          <input :id="f.id" v-model="name" class="input" name="username" autocomplete="username webauthn" autocapitalize="none" spellcheck="false" required autofocus />
        </FormField>
        <FormField v-slot="f" label="Password">
          <PasswordInput :id="f.id" v-model="password" name="password" autocomplete="current-password" />
        </FormField>
        <p v-if="error" class="err" role="alert" data-testid="login-error">{{ error }}</p>
        <button type="submit" class="btn btn-primary btn-block" :disabled="busy || !name || !password"><Spinner v-if="busy" :size="13" />Sign in</button>
      </form>
      <Callout v-else status="info" title="Single sign-on only" data-testid="local-off">Password and passkey sign-in are turned off on this server. Sign in with your organisation's account below.</Callout>

      <div v-if="localLogin && (showPasskey || providers.length)" class="or" aria-hidden="true"><span>or</span></div>
      <div v-if="showPasskey || providers.length" class="alts" :class="{ solo: !localLogin }">
        <template v-if="showPasskey">
          <button type="button" class="btn btn-block" :disabled="keyBusy || !!passkeyBlocked" :title="passkeyBlocked" data-testid="passkey-signin" @click="usePasskey">
            <Spinner v-if="keyBusy" :size="13" /><Icon v-else name="fingerprint" />Sign in with a passkey
          </button>
          <p v-if="passkeyBlocked" class="note" data-testid="passkey-blocked">{{ passkeyBlocked }}</p>
        </template>
        <a v-for="p in providers" :key="p.name" class="btn btn-block" :class="{ 'btn-primary': !localLogin }" :href="authn.ssoURL(p.name, next)" :data-testid="`sso-${p.name}`"><Icon name="key-round" />Sign in with {{ p.label }}</a>
      </div>
      <p v-if="!localLogin && error" class="err" role="alert" data-testid="login-error">{{ error }}</p>
      <p v-if="!localLogin && !providers.length && options" class="note">No identity provider is enabled. An administrator can turn password sign-in back on with <code>incus-backup-server user local-login on</code> on the server.</p>
      <p v-if="optionsError" class="note">Could not load the other sign-in options; password sign-in still works.</p>
    </template>

    <div v-else-if="step === 'key'" class="form-grid" data-testid="key-step">
      <div class="keybox" aria-live="polite">
        <Icon name="fingerprint" :size="28" />
        <p v-if="keyBusy">Waiting for your security key. Touch it or confirm on your device.</p>
        <p v-else>Use the security key or passkey you registered for this account.</p>
      </div>
      <p v-if="error" class="err" role="alert" data-testid="login-error">{{ error }}</p>
      <button type="button" class="btn btn-primary btn-block" :disabled="keyBusy" data-testid="key-retry" @click="useKey"><Spinner v-if="keyBusy" :size="13" />Use security key</button>
      <div class="alt">
        <button v-if="methods.totp" type="button" class="linkbtn" data-testid="use-code" @click="toCode">Use an authenticator code instead</button>
        <span v-else />
        <button type="button" class="linkbtn" @click="back">Back</button>
      </div>
    </div>

    <form v-else class="form-grid" data-testid="totp-form" @submit.prevent="submit">
      <FormField v-slot="f" :label="recovery ? 'Recovery code' : 'Authentication code'">
        <input :id="f.id" ref="codeInput" :value="code" class="input mono code" :class="{ rc: recovery }" :inputmode="recovery ? 'text' : 'numeric'" autocomplete="one-time-code" :placeholder="recovery ? 'xxxxx-xxxxx' : '000000'" :aria-describedby="f.describedby" data-testid="totp-code" @input="onCodeInput" />
      </FormField>
      <p v-if="error" class="err" role="alert" data-testid="login-error">{{ error }}</p>
      <button type="submit" class="btn btn-primary btn-block" :disabled="busy || (recovery ? code.length !== 11 : code.length !== 6)"><Spinner v-if="busy" :size="13" />Verify</button>
      <div class="alt">
        <button type="button" class="linkbtn" data-testid="use-recovery" @click="recovery = !recovery; code = ''; error = ''">{{ recovery ? 'Use the authenticator code' : 'Use a recovery code' }}</button>
        <button v-if="methods.webauthn && ticket" type="button" class="linkbtn" data-testid="use-key" @click="toKey">Use security key</button>
        <button type="button" class="linkbtn" @click="back">Back</button>
      </div>
    </form>
    <template #foot>Forgot your password or lost your security key or 2FA device? An administrator can reset it, or run <code>incus-backup-server user set-password</code> on the server.</template>
  </AuthLayout>
</template>

<style scoped>
.err { color: var(--critical-text); font-size: var(--fs-sm); }
.note { color: var(--text-muted); font-size: var(--fs-sm); line-height: 1.45; margin-top: 8px; }
.code { height: 44px; font-size: 22px; letter-spacing: .3em; text-align: center; }
.code.rc { font-size: 17px; letter-spacing: .12em; }
.alt { display: flex; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.alts { display: grid; gap: 8px; }
.alts.solo { margin-top: 14px; }
.alts a.btn { text-decoration: none; }
.or { display: flex; align-items: center; gap: 10px; color: var(--text-faint); font-size: var(--fs-xs); text-transform: uppercase; letter-spacing: .06em; margin: 16px 0 12px; }
.or::before, .or::after { content: ''; flex: 1; height: 1px; background: var(--border); }
.keybox { display: flex; flex-direction: column; align-items: center; gap: 10px; padding: 18px 12px; border: 1px dashed var(--border-strong); border-radius: var(--r-lg); color: var(--text-2); text-align: center; }
.keybox :deep(svg) { color: var(--accent-text); }
.linkbtn { border: 0; background: none; padding: 0; color: var(--accent-text); cursor: pointer; font-size: var(--fs-sm); }
.linkbtn:hover { text-decoration: underline; }
</style>
