<script setup lang="ts">
// "Security keys & passkeys" of the signed-in user: list, add (navigator.credentials.create), rename,
// remove. Adding or removing needs a sign-in within the last 10 minutes or the password (the server
// says so with password_required; the field appears then). enrol: the forced-enrolment page shows only
// the add action and emits `added`.
import { computed, ref, nextTick } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import Modal from '@/components/Modal.vue'
import Spinner from '@/components/Spinner.vue'
import Callout from '@/components/Callout.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import StatusPill from '@/components/StatusPill.vue'
import EmptyState from '@/components/EmptyState.vue'
import FormField from '@/components/form/FormField.vue'
import PasswordInput from '@/components/form/PasswordInput.vue'
import { useResource } from '@/api/cache'
import { authn, type WebAuthnKey } from '@/api/sections/authn'
import { ApiError } from '@/api/client'
import { createCredential, unavailableReason, webauthnError } from '@/lib/webauthn'
import { session, refreshUser } from '@/stores/session'
import { toast, errorText } from '@/stores/toast'
import { passwordCheckError } from '@/lib/authErrors'

const props = withDefaults(defineProps<{ enrol?: boolean }>(), { enrol: false })
const emit = defineEmits<{ added: [WebAuthnKey] }>()

const keys = useResource(() => 'me:webauthn', (s) => authn.keys(s), { enabled: () => !props.enrol })
const list = computed(() => keys.data.value ?? [])
const blocked = unavailableReason()
const ssoOnly = computed(() => session.user?.has_password === false)

const needsPassword = (e: unknown) => e instanceof ApiError && e.status === 400 && !!(e.body as { password_required?: boolean } | null)?.password_required

function suggestName(): string {
  const ua = navigator.userAgent
  const os = /iPhone|iPad/.test(ua) ? 'iPhone' : /Android/.test(ua) ? 'Android phone' : /Mac OS X/.test(ua) ? 'Mac' : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : ''
  return os ? `Passkey on ${os}` : 'Security key'
}

// ---- add
const add = ref({ open: false, name: '', password: '', askPassword: false, busy: false, error: '' })
const nameEl = ref<HTMLInputElement | null>(null)
async function openAdd() {
  add.value = { open: true, name: suggestName(), password: '', askPassword: false, busy: false, error: '' }
  await nextTick()
  nameEl.value?.select()
}
async function register() {
  const a = add.value
  if (a.busy) return
  a.busy = true
  a.error = ''
  try {
    const start = await authn.beginRegister(a.askPassword ? a.password : undefined)
    const cred = await createCredential(start.options)
    const k = await authn.finishRegister(start.ceremony, a.name.trim(), cred)
    a.open = false
    toast.success('Security key added', `“${k.name}” can now sign you in.`)
    void keys.refresh()
    await refreshUser()
    emit('added', k)
  } catch (e) {
    if (needsPassword(e)) {
      a.askPassword = true
      a.error = a.password ? passwordCheckError(e) : 'Your sign-in is older than 10 minutes: confirm your password to add a key.'
    } else if (e instanceof ApiError) {
      a.error = e.status === 401 || e.status === 429 ? passwordCheckError(e) : e.message
    } else {
      a.error = webauthnError(e, 'register')
    }
  } finally {
    a.busy = false
  }
}

// ---- rename
const ren = ref<{ key: WebAuthnKey; name: string; busy: boolean; error: string } | null>(null)
async function rename() {
  const r = ren.value
  if (!r) return
  r.busy = true
  r.error = ''
  try {
    await authn.renameKey(r.key.id, r.name.trim())
    ren.value = null
    toast.success('Key renamed')
    void keys.refresh()
  } catch (e) { r.error = errorText(e) } finally { if (ren.value) ren.value.busy = false }
}

// ---- remove
const rm = ref<{ key: WebAuthnKey; password: string; askPassword: boolean; busy: boolean; error: string } | null>(null)
async function remove() {
  const r = rm.value
  if (!r) return
  r.busy = true
  r.error = ''
  try {
    await authn.removeKey(r.key.id, r.askPassword ? r.password : undefined)
    rm.value = null
    toast.success('Key removed', 'It can no longer sign you in.')
    void keys.refresh()
    await refreshUser()
  } catch (e) {
    if (needsPassword(e)) {
      r.askPassword = true
      r.error = r.password ? passwordCheckError(e) : 'Your sign-in is older than 10 minutes: confirm your password to remove a key.'
    } else r.error = e instanceof ApiError && (e.status === 401 || e.status === 429) ? passwordCheckError(e) : errorText(e)
  } finally { if (rm.value) rm.value.busy = false }
}

const kind = (k: WebAuthnKey) => (k.backup_eligible ? 'Synced passkey' : k.attachment === 'platform' ? 'Device passkey' : 'Security key')
</script>

<template>
  <div class="sk">
    <Callout v-if="ssoOnly" status="info" title="Managed by your identity provider">Your account signs in through single sign-on. Security keys and two-factor authentication are set up at your identity provider.</Callout>
    <template v-else>
      <Callout v-if="blocked" status="warn" title="Passkeys are not available on this page" data-testid="webauthn-blocked">{{ blocked }}</Callout>
      <template v-if="!enrol">
        <div v-if="keys.loading.value && !keys.data.value" class="row"><Spinner /> Loading keys</div>
        <Callout v-else-if="keys.error.value" status="critical" title="Could not load your keys" :retry="keys.refresh">{{ keys.error.value.message }}</Callout>
        <EmptyState v-else-if="!list.length" title="No security keys or passkeys yet" icon="fingerprint" compact>Add one to sign in without a password, or as the second step after it.</EmptyState>
        <ul v-else class="keys" data-testid="key-list">
          <li v-for="k in list" :key="k.id" :data-testid="`key-${k.name}`">
            <span class="ic"><Icon name="fingerprint" :size="18" /></span>
            <div class="meta">
              <div class="nm">{{ k.name }} <StatusPill v-if="k.clone_warning" status="critical" label="blocked: may be copied" size="sm" /></div>
              <div class="sub">{{ kind(k) }} · added <AbsoluteTime :time="k.created_at" /> · last used <RelativeTime :time="k.last_used_at" never="never" /><template v-if="k.discoverable === false"> · second step only</template></div>
            </div>
            <span class="acts">
              <button type="button" class="btn btn-sm btn-ghost" :aria-label="`Rename ${k.name}`" @click="ren = { key: k, name: k.name, busy: false, error: '' }"><Icon name="pencil" :size="14" />Rename</button>
              <button type="button" class="btn btn-sm btn-ghost" :aria-label="`Remove ${k.name}`" :data-testid="`key-remove-${k.name}`" @click="rm = { key: k, password: '', askPassword: false, busy: false, error: '' }"><Icon name="trash-2" :size="14" />Remove</button>
            </span>
          </li>
        </ul>
      </template>
      <div class="form-actions"><button type="button" class="btn" :class="{ 'btn-primary': enrol || !list.length }" :disabled="!!blocked" data-testid="key-add" @click="openAdd"><Icon name="plus" />Add a security key or passkey</button></div>
    </template>

    <Modal :open="add.open" title="Add a security key or passkey" description="Your browser asks you to touch the key or confirm on your phone or computer." @update:open="(v: boolean) => (add.open = v)">
      <form id="key-add-form" class="form-grid" @submit.prevent="register">
        <FormField v-slot="f" label="Name" hint="So you can tell your keys apart, e.g. “YubiKey on the keyring”.">
          <input :id="f.id" ref="nameEl" v-model="add.name" class="input" maxlength="64" required data-testid="key-name" />
        </FormField>
        <FormField v-if="add.askPassword" v-slot="f" label="Your password"><PasswordInput :id="f.id" v-model="add.password" autocomplete="current-password" data-testid="key-password" /></FormField>
        <p v-if="add.error" class="err" role="alert" data-testid="key-error">{{ add.error }}</p>
      </form>
      <template #footer>
        <button type="button" class="btn" @click="add.open = false">Cancel</button>
        <button type="submit" form="key-add-form" class="btn btn-primary" :disabled="add.busy || !add.name.trim() || (add.askPassword && !add.password)" data-testid="key-register"><Spinner v-if="add.busy" :size="13" />{{ add.busy ? 'Waiting for the key' : 'Continue' }}</button>
      </template>
    </Modal>

    <Modal :open="!!ren" title="Rename key" @update:open="(v: boolean) => { if (!v) ren = null }">
      <form v-if="ren" id="key-ren-form" class="form-grid" @submit.prevent="rename">
        <FormField v-slot="f" label="Name" :error="ren.error"><input :id="f.id" v-model="ren.name" class="input" maxlength="64" required /></FormField>
      </form>
      <template #footer><button type="button" class="btn" @click="ren = null">Cancel</button><button type="submit" form="key-ren-form" class="btn btn-primary" :disabled="ren?.busy || !ren?.name.trim()"><Spinner v-if="ren?.busy" :size="13" />Save</button></template>
    </Modal>

    <Modal :open="!!rm" :title="`Remove “${rm?.key.name}”?`" description="It can no longer sign you in. Remove a lost key at once; keep at least one other way to sign in." @update:open="(v: boolean) => { if (!v) rm = null }">
      <form v-if="rm" id="key-rm-form" class="form-grid" @submit.prevent="remove">
        <FormField v-if="rm.askPassword" v-slot="f" label="Your password"><PasswordInput :id="f.id" v-model="rm.password" autocomplete="current-password" /></FormField>
        <p v-if="rm.error" class="err" role="alert" data-testid="key-remove-error">{{ rm.error }}</p>
      </form>
      <template #footer><button type="button" class="btn" @click="rm = null">Cancel</button><button type="submit" form="key-rm-form" class="btn btn-danger" :disabled="rm?.busy || (rm?.askPassword && !rm?.password)" data-testid="key-remove-confirm"><Spinner v-if="rm?.busy" :size="13" />Remove key</button></template>
    </Modal>
  </div>
</template>

<style scoped>
.sk { display: flex; flex-direction: column; gap: 12px; }
.row { display: flex; gap: 8px; align-items: center; color: var(--text-muted); }
.keys { list-style: none; margin: 0; padding: 0; display: grid; gap: 0; border: 1px solid var(--border); border-radius: var(--r); }
.keys li { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; gap: 12px; align-items: center; padding: 10px 12px; }
.keys li + li { border-top: 1px solid var(--border); }
.ic { display: grid; place-items: center; width: 32px; height: 32px; border-radius: 50%; background: var(--accent-soft); color: var(--accent-text); }
.nm { font-weight: var(--fw-medium); display: flex; gap: 8px; align-items: center; flex-wrap: wrap; overflow-wrap: anywhere; }
.sub { font-size: var(--fs-sm); color: var(--text-muted); }
.acts { display: flex; gap: 4px; flex-wrap: wrap; justify-content: flex-end; }
.err { color: var(--critical-text); font-size: var(--fs-sm); }
.form-actions { display: flex; gap: 8px; }
@media (max-width: 560px) { .keys li { grid-template-columns: auto minmax(0, 1fr); } .acts { grid-column: 1 / -1; justify-content: flex-start; } }
</style>
