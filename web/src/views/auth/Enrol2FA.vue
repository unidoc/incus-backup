<script setup lang="ts">
// Forced enrolment: the sign-in policy (Settings → Authentication, require_2fa) requires a second factor
// of this user and none is set up. The server answers only the enrolment routes until one is: a
// security key / passkey, or an authenticator app (TOTP, with recovery codes).
import { ref, computed, nextTick } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import QRCode from 'qrcode'
import AuthLayout from '@/components/app/AuthLayout.vue'
import FormField from '@/components/form/FormField.vue'
import Callout from '@/components/Callout.vue'
import CopyField from '@/components/CopyField.vue'
import Spinner from '@/components/Spinner.vue'
import Icon from '@/components/icons/Icon.vue'
import SecurityKeys from '@/components/authn/SecurityKeys.vue'
import { api } from '@/api/endpoints'
import { session, refreshUser, logout } from '@/stores/session'
import { errorText } from '@/stores/toast'
import { useClipboard, downloadText } from '@/composables/useClipboard'
import type { TOTPEnrolment } from '@/api/types'

const route = useRoute()
const router = useRouter()
const mode = ref<'choose' | 'totp' | 'codes'>('choose')
const next = computed(() => (typeof route.query.next === 'string' && route.query.next.startsWith('/') && !route.query.next.startsWith('//') ? route.query.next : '/'))

async function finished() {
  await refreshUser()
  if (!session.enrol2fa) await router.replace(next.value)
}

// ---- authenticator app
const t = ref<{ data: TOTPEnrolment | null; qr: string; code: string; busy: boolean; error: string; codes: string[] }>({ data: null, qr: '', code: '', busy: false, error: '', codes: [] })
const codeEl = ref<HTMLInputElement | null>(null)
async function startTOTP() {
  mode.value = 'totp'
  t.value = { data: null, qr: '', code: '', busy: true, error: '', codes: [] }
  try {
    const d = await api.me.beginTOTP()
    const qr = await QRCode.toString(d.uri, { type: 'svg', margin: 1, errorCorrectionLevel: 'M', color: { dark: '#0f1720', light: '#ffffff' } })
    t.value = { ...t.value, data: d, qr, busy: false }
    await nextTick()
    codeEl.value?.focus()
  } catch (e) { t.value = { ...t.value, busy: false, error: errorText(e) } }
}
async function confirmTOTP() {
  if (t.value.code.length !== 6) return
  t.value.busy = true
  t.value.error = ''
  try {
    const r = await api.me.confirmTOTP({ code: t.value.code })
    t.value = { ...t.value, codes: r.recovery_codes, busy: false }
    mode.value = 'codes'
  } catch (e) { t.value = { ...t.value, busy: false, code: '', error: `${errorText(e)}. Check the time on your phone and enter the current code.` } }
}
const saved = ref(false)
const { copy, copied } = useClipboard()
const codesText = computed(() => `incus-backup recovery codes for ${session.name} (${location.host})\nEach code works once.\n\n${t.value.codes.join('\n')}\n`)
async function signOut() { await logout(); await router.replace({ name: 'login' }) }
</script>

<template>
  <AuthLayout title="Set up two-factor authentication" lede="This server requires a second step at sign-in for your account. Set one up to continue." wide>
    <div v-if="mode === 'choose'" class="choices" data-testid="enrol-choices">
      <section class="choice">
        <h2><Icon name="fingerprint" />Security key or passkey</h2>
        <p>A hardware key (YubiKey and similar), or a passkey on your phone or computer. It also lets you sign in without a password.</p>
        <SecurityKeys enrol @added="finished" />
      </section>
      <section class="choice">
        <h2><Icon name="smartphone" />Authenticator app</h2>
        <p>A 6-digit code from an app on your phone (any TOTP app). You also get recovery codes.</p>
        <div class="form-actions"><button type="button" class="btn" data-testid="enrol-totp" @click="startTOTP">Set up an authenticator app</button></div>
      </section>
    </div>

    <div v-else-if="mode === 'totp'" class="totp">
      <div v-if="t.busy && !t.data" class="row"><Spinner /> Preparing</div>
      <div v-else-if="t.data" class="enrol">
        <div class="qr" role="img" aria-label="QR code for your authenticator app" v-html="t.qr" />
        <div class="side">
          <p>Scan the code with your authenticator app, or enter the key by hand.</p>
          <CopyField :value="t.data.secret" label="Key" data-testid="totp-secret" />
          <form class="form-grid" @submit.prevent="confirmTOTP">
            <FormField v-slot="f" label="Code from the app" :error="t.error">
              <input :id="f.id" ref="codeEl" v-model="t.code" class="input mono code" inputmode="numeric" maxlength="6" autocomplete="one-time-code" placeholder="000000" data-testid="totp-confirm-code" @input="t.code = t.code.replace(/\D/g, '')" />
            </FormField>
            <div class="form-actions">
              <button type="submit" class="btn btn-primary" :disabled="t.code.length !== 6 || t.busy" data-testid="totp-confirm"><Spinner v-if="t.busy" :size="13" />Confirm</button>
              <button type="button" class="btn btn-ghost" @click="mode = 'choose'">Back</button>
            </div>
          </form>
        </div>
      </div>
      <Callout v-else-if="t.error" status="critical" title="Could not start the set-up">{{ t.error }}</Callout>
    </div>

    <div v-else class="codes-step" data-testid="recovery-codes">
      <Callout status="warn" title="Save these recovery codes now">Each code signs you in once if you lose your phone. They are shown only now.</Callout>
      <ol class="codes num"><li v-for="c in t.codes" :key="c"><code>{{ c }}</code></li></ol>
      <div class="form-actions">
        <button type="button" class="btn" @click="copy(t.codes.join('\n')); saved = true"><Icon :name="copied ? 'check' : 'copy'" />{{ copied ? 'Copied' : 'Copy' }}</button>
        <button type="button" class="btn" @click="downloadText(`incus-backup-recovery-${session.name}.txt`, codesText); saved = true"><Icon name="download" />Download</button>
      </div>
      <label class="ack"><input v-model="saved" type="checkbox" data-testid="codes-saved" /> I saved the recovery codes in a safe place</label>
      <div class="form-actions"><button type="button" class="btn btn-primary" :disabled="!saved" data-testid="totp-done" @click="finished">Continue</button></div>
    </div>

    <template #foot>Signed in as <strong>{{ session.name }}</strong>. <button type="button" class="linkbtn" data-testid="enrol-signout" @click="signOut">Sign out</button></template>
  </AuthLayout>
</template>

<style scoped>
.choices { display: grid; gap: 14px; }
.choice { border: 1px solid var(--border); border-radius: var(--r-lg); padding: 14px 16px; display: grid; gap: 10px; }
.choice h2 { display: flex; gap: 8px; align-items: center; font-size: var(--fs-md, 15px); margin: 0; }
.choice h2 :deep(svg) { color: var(--accent-text); }
.choice p, .side p { color: var(--text-2); font-size: var(--fs-sm); margin: 0; }
.row { display: flex; gap: 8px; align-items: center; color: var(--text-muted); }
.enrol { display: grid; grid-template-columns: 170px 1fr; gap: 18px; align-items: start; }
.qr { width: 170px; height: 170px; padding: 8px; background: #fff; border: 1px solid var(--border); border-radius: var(--r); }
.qr :deep(svg) { width: 100%; height: 100%; display: block; }
.side { display: flex; flex-direction: column; gap: 12px; min-width: 0; }
.code { font-size: 18px; letter-spacing: .25em; width: 160px; }
.codes-step { display: flex; flex-direction: column; gap: 14px; }
.codes { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 6px 24px; margin: 0; padding: 12px 12px 12px 36px; background: var(--code-bg); border: 1px solid var(--border); border-radius: var(--r); }
.ack { display: flex; gap: 8px; align-items: center; font-size: var(--fs-sm); }
.form-actions { display: flex; gap: 8px; flex-wrap: wrap; }
.linkbtn { border: 0; background: none; padding: 0; color: var(--accent-text); cursor: pointer; font: inherit; }
.linkbtn:hover { text-decoration: underline; }
@media (max-width: 560px) { .enrol { grid-template-columns: 1fr; } .codes { grid-template-columns: 1fr; } }
</style>
