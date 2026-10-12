<script setup lang="ts">
// Create or edit a notification channel (drawer). Per-kind fields; secrets are write-only: an existing
// secret is never shown, only "stored", and can be kept, replaced (all of its parts) or removed.
import { computed, ref, watch } from 'vue'
import Modal from '@/components/Modal.vue'
import FormField from '@/components/form/FormField.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import TagInput from '@/components/form/TagInput.vue'
import PasswordInput from '@/components/form/PasswordInput.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import CIcon from './CIcon.vue'
import { cfg, type Channel, type ChannelKind } from '@/api/sections/config'
import { emptyForm, formFromChannel, secretOptional, toInput, validateForm, type ChannelForm } from './channelForm'
import { toast, errorText } from '@/stores/toast'
import { KIND_META } from './channelKinds'

const props = defineProps<{ open: boolean; channel: Channel | null }>()
const emit = defineEmits<{ 'update:open': [boolean]; saved: [Channel] }>()
const creating = computed(() => !props.channel)
const f = ref<ChannelForm>(emptyForm())
const touched = ref(false)
const busy = ref(false)
const err = ref('')
const advanced = ref(false)

watch(() => props.open, (o) => {
  if (!o) return
  f.value = props.channel ? formFromChannel(props.channel) : emptyForm('webhook')
  touched.value = false
  err.value = ''
  advanced.value = false
}, { immediate: true })

const errors = computed(() => validateForm(f.value, creating.value))
const shown = (k: string) => (touched.value ? errors.value[k] ?? '' : '')
const sendSecret = computed(() => creating.value || f.value.secretMode === 'replace')

function pickKind(k: ChannelKind) {
  if (!creating.value) return
  const name = f.value.name
  f.value = emptyForm(k)
  f.value.name = name
  if (k === 'smtp') f.value.port = 587
}
function onTLS(v: string) {
  f.value.tls = v as ChannelForm['tls']
  if (v === 'tls' && f.value.port === 587) f.value.port = 465
  if (v === 'starttls' && f.value.port === 465) f.value.port = 587
}

async function save() {
  touched.value = true
  if (Object.keys(errors.value).length) { err.value = 'Fix the marked fields.'; return }
  busy.value = true
  err.value = ''
  try {
    const body = toInput(f.value, creating.value)
    const c = creating.value ? await cfg.channels.create(body) : await cfg.channels.update(props.channel!.id, body)
    toast.success(creating.value ? 'Channel created' : 'Channel saved', creating.value ? 'Send a test message to check that it arrives.' : undefined)
    emit('saved', c)
    emit('update:open', false)
  } catch (e) {
    err.value = errorText(e)
  } finally {
    busy.value = false
  }
}
const kinds = Object.entries(KIND_META) as [ChannelKind, (typeof KIND_META)[ChannelKind]][]
const tlsOpts = [{ value: 'starttls', label: 'STARTTLS (port 587)' }, { value: 'tls', label: 'TLS from the start (port 465)' }, { value: 'none', label: 'None (no password allowed)' }]
const authOpts = [{ value: '', label: 'Pick from what the server offers' }, { value: 'plain', label: 'PLAIN' }, { value: 'login', label: 'LOGIN' }]
const formatOpts = [{ value: 'json', label: 'JSON (incus-backup payload, signed)' }, { value: 'slack', label: 'Slack incoming webhook' }, { value: 'discord', label: 'Discord webhook' }]
const validEmail = (v: string) => (/^[^\s@<>]+@[^\s@<>]+$/.test(v) ? '' : 'Not an e-mail address')
</script>

<template>
  <Modal :open="open" drawer :title="creating ? 'Add a notification channel' : `Edit ${channel?.name}`" description="Where messages go. Rules decide which alerts reach it." width="600px" @update:open="(v: boolean) => emit('update:open', v)">
    <form id="channel-form" class="cx-form" data-testid="channel-form" novalidate @submit.prevent="save">
      <FormField label="Kind" :hint="creating ? '' : 'The kind of a channel cannot change; add a new channel instead.'">
        <div class="kinds" role="radiogroup" aria-label="Channel kind">
          <button v-for="[k, m] in kinds" :key="k" type="button" role="radio" class="kind" :aria-checked="f.kind === k" :disabled="!creating && f.kind !== k" :data-kind="k" @click="pickKind(k)">
            <CIcon :name="m.icon" :size="18" /><span class="kn">{{ m.label }}</span><span class="kd">{{ m.short }}</span>
          </button>
        </div>
      </FormField>
      <div class="cx-cols">
        <FormField v-slot="ff" label="Name" :error="shown('name')" hint="Shown in rules and the delivery log">
          <input :id="ff.id" v-model="f.name" class="input" :aria-invalid="ff.invalid" placeholder="ops-mail" autocomplete="off" data-testid="ch-name" />
        </FormField>
        <FormField label="Enabled" hint="A disabled channel gets nothing; its queue is dead-lettered">
          <SwitchInput v-model="f.enabled" :label="f.enabled ? 'Sends messages' : 'Off'" />
        </FormField>
      </div>

      <!-- SMTP -->
      <template v-if="f.kind === 'smtp'">
        <div class="cx-cols">
          <FormField v-slot="ff" label="Mail server" :error="shown('host')"><input :id="ff.id" v-model="f.host" class="input" placeholder="smtp.example.com" spellcheck="false" /></FormField>
          <FormField v-slot="ff" label="Port" :error="shown('port')"><input :id="ff.id" v-model.number="f.port" type="number" min="1" max="65535" class="input num" /></FormField>
        </div>
        <div class="cx-cols">
          <FormField v-slot="ff" label="Encryption" :error="shown('tls')" hint="A password is never sent without TLS; STARTTLS never falls back to clear text"><SelectInput :id="ff.id" :model-value="f.tls" :options="tlsOpts" @update:model-value="onTLS" /></FormField>
          <FormField v-slot="ff" label="Authentication"><SelectInput :id="ff.id" v-model="f.auth" :options="authOpts" /></FormField>
        </div>
        <div class="cx-cols">
          <FormField v-slot="ff" label="User name" optional><input :id="ff.id" v-model="f.username" class="input" autocomplete="off" /></FormField>
          <FormField v-slot="ff" label="From" :error="shown('from')"><input :id="ff.id" v-model="f.from" class="input" placeholder="backup@example.com" /></FormField>
        </div>
        <FormField v-slot="ff" label="Recipients" :error="shown('to')" hint="Enter or comma adds one"><TagInput :id="ff.id" v-model="f.to" placeholder="ops@example.com" :validate="validEmail" /></FormField>
      </template>

      <!-- webhook -->
      <template v-else-if="f.kind === 'webhook'">
        <FormField v-slot="ff" label="Format"><SelectInput :id="ff.id" v-model="f.format" :options="formatOpts" /></FormField>
        <FormField v-slot="ff" label="Extra headers" optional :error="shown('headers')" hint="Not secret, one per line: Name: value. Put Authorization in the secret below.">
          <textarea :id="ff.id" v-model="f.headers" class="textarea mono" rows="2" placeholder="X-Environment: production" spellcheck="false" />
        </FormField>
      </template>

      <!-- ntfy -->
      <template v-else-if="f.kind === 'ntfy'">
        <div class="cx-cols">
          <FormField v-slot="ff" label="Server" :error="shown('server')"><input :id="ff.id" v-model="f.server" class="input" spellcheck="false" /></FormField>
          <FormField v-slot="ff" label="Topic" :error="shown('topic')"><input :id="ff.id" v-model="f.topic" class="input" placeholder="backup-alerts" spellcheck="false" /></FormField>
        </div>
        <FormField v-slot="ff" label="User name" optional hint="Basic authentication with the password below; without it the token is used"><input :id="ff.id" v-model="f.username" class="input" autocomplete="off" /></FormField>
      </template>

      <!-- gotify -->
      <template v-else>
        <FormField v-slot="ff" label="Gotify server" :error="shown('url')"><input :id="ff.id" v-model="f.url" class="input" placeholder="https://gotify.example.com" spellcheck="false" /></FormField>
      </template>

      <fieldset class="cx-fieldset" data-testid="secret-box">
        <legend>Secret</legend>
        <template v-if="!creating && channel?.has_secret">
          <p class="cx-help"><CIcon name="key" :size="13" /> A secret is stored. It is never shown again, not even to administrators.</p>
          <div class="cx-presets" role="radiogroup" aria-label="Stored secret">
            <button type="button" role="radio" :aria-checked="f.secretMode === 'keep'" :aria-pressed="f.secretMode === 'keep'" data-testid="secret-keep" @click="f.secretMode = 'keep'">Keep it</button>
            <button type="button" role="radio" :aria-checked="f.secretMode === 'replace'" :aria-pressed="f.secretMode === 'replace'" data-testid="secret-replace" @click="f.secretMode = 'replace'">Replace it</button>
            <button v-if="secretOptional(f)" type="button" role="radio" :aria-checked="f.secretMode === 'remove'" :aria-pressed="f.secretMode === 'remove'" @click="f.secretMode = 'remove'">Remove it</button>
          </div>
          <p v-if="f.secretMode === 'replace'" class="cx-help">Replacing writes a new secret: fill in every part you need, the old values are not kept.</p>
          <p v-if="f.secretMode === 'remove'" class="cx-help">The stored secret is deleted when you save.</p>
        </template>
        <p v-else-if="!creating" class="cx-help">No secret is stored.</p>
        <template v-if="sendSecret">
          <template v-if="f.kind === 'smtp'">
            <FormField v-slot="ff" label="Password" :optional="!f.username" :error="shown('secret.password')"><PasswordInput :id="ff.id" v-model="f.secret.password" autocomplete="new-password" /></FormField>
          </template>
          <template v-else-if="f.kind === 'webhook'">
            <FormField v-slot="ff" label="URL" :error="shown('secret.url')" hint="Stored as a secret: Slack and Discord URLs carry their token">
              <PasswordInput :id="ff.id" v-model="f.secret.url" autocomplete="off" name="webhook-url" />
            </FormField>
            <FormField v-slot="ff" label="Signing secret" optional hint="Adds X-Incus-Backup-Signature: HMAC-SHA256 of timestamp.body"><PasswordInput :id="ff.id" v-model="f.secret.hmac_secret" autocomplete="off" /></FormField>
            <FormField v-slot="ff" label="Secret headers" optional :error="shown('secret.headers')" hint="One per line, e.g. Authorization: Bearer …">
              <textarea :id="ff.id" v-model="f.secret.headers" class="textarea mono" rows="2" spellcheck="false" />
            </FormField>
          </template>
          <template v-else-if="f.kind === 'ntfy'">
            <FormField v-if="!f.username" v-slot="ff" label="Access token" optional><PasswordInput :id="ff.id" v-model="f.secret.token" autocomplete="off" /></FormField>
            <FormField v-else v-slot="ff" label="Password" :error="shown('secret.password')"><PasswordInput :id="ff.id" v-model="f.secret.password" autocomplete="new-password" /></FormField>
          </template>
          <template v-else>
            <FormField v-slot="ff" label="Application token" :error="shown('secret.token')" hint="Sent as X-Gotify-Key, never in the URL"><PasswordInput :id="ff.id" v-model="f.secret.token" autocomplete="off" /></FormField>
          </template>
        </template>
      </fieldset>

      <button type="button" class="btn btn-ghost btn-sm adv" :aria-expanded="advanced" @click="advanced = !advanced">{{ advanced ? 'Hide' : 'Show' }} advanced settings</button>
      <div v-if="advanced" class="cx-form">
        <div class="cx-cols">
          <FormField v-slot="ff" label="Rate limit" :error="shown('rate_per_hour')" hint="Messages per hour; more wait in the queue, never dropped. 0 = unlimited">
            <div class="cx-unit"><input :id="ff.id" v-model.number="f.rate_per_hour" type="number" min="0" class="input num" /><span class="u">per hour</span></div>
          </FormField>
          <FormField v-slot="ff" label="Timeout" :error="shown('timeout_seconds')"><div class="cx-unit"><input :id="ff.id" v-model.number="f.timeout_seconds" type="number" min="1" max="300" class="input num" /><span class="u">seconds</span></div></FormField>
        </div>
        <template v-if="f.kind === 'smtp'">
          <div class="cx-cols">
            <FormField v-slot="ff" label="HELO name" optional><input :id="ff.id" v-model="f.helo" class="input" /></FormField>
            <FormField v-slot="ff" label="Verify certificate name" optional hint="Instead of the server name"><input :id="ff.id" v-model="f.tls_server_name" class="input" /></FormField>
          </div>
        </template>
        <FormField v-if="f.kind === 'webhook'" v-slot="ff" label="Signature header" optional hint="Default X-Incus-Backup-Signature"><input :id="ff.id" v-model="f.signature_header" class="input" /></FormField>
        <div v-if="f.kind === 'ntfy' || f.kind === 'gotify'" class="cx-cols3">
          <FormField v-for="k in (['info', 'warning', 'critical', 'resolved'] as const)" :key="k" v-slot="ff" :label="`Priority: ${k}`" optional :error="shown(`priorities.${k}`)">
            <input :id="ff.id" v-model="f.priorities[k]" type="number" min="0" max="10" class="input num" :placeholder="String((f.kind === 'ntfy' ? { info: 3, warning: 4, critical: 5, resolved: 2 } : { info: 3, warning: 6, critical: 9, resolved: 2 })[k])" />
          </FormField>
        </div>
        <FormField v-slot="ff" label="Trusted CA (PEM)" optional hint="Trust this CA instead of the system roots, for a private mail or web server">
          <textarea :id="ff.id" v-model="f.tls_ca_pem" class="textarea mono" rows="3" placeholder="-----BEGIN CERTIFICATE-----" spellcheck="false" />
        </FormField>
      </div>
      <Callout v-if="err" status="critical" :title="creating ? 'Could not create the channel' : 'Could not save the channel'">{{ err }}</Callout>
    </form>
    <template #footer>
      <button type="button" class="btn btn-ghost" @click="emit('update:open', false)">Cancel</button>
      <button type="submit" form="channel-form" class="btn btn-primary" :disabled="busy" data-testid="ch-save"><Spinner v-if="busy" :size="13" />{{ creating ? 'Add channel' : 'Save' }}</button>
    </template>
  </Modal>
</template>

<style scoped>
.kinds { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 8px; }
.kind { appearance: none; font: inherit; text-align: left; display: grid; grid-template-columns: auto 1fr; grid-template-rows: auto auto; column-gap: 8px; align-items: center; padding: 10px; border: 1px solid var(--border-strong); border-radius: var(--r); background: var(--surface); color: var(--text); cursor: pointer; }
.kind .icon { grid-row: span 2; color: var(--text-muted); }
.kind .kn { font-weight: var(--fw-medium); }
.kind .kd { font-size: var(--fs-xs); color: var(--text-muted); }
.kind[aria-checked='true'] { border-color: var(--accent); background: var(--accent-soft); box-shadow: inset 0 0 0 1px var(--accent); }
.kind[aria-checked='true'] .icon { color: var(--accent-text); }
.kind:disabled { opacity: .45; cursor: not-allowed; }
.adv { justify-self: start; }
@media (max-width: 560px) { .kinds { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
</style>
