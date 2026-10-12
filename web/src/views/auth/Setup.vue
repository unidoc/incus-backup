<script setup lang="ts">
// First run: no user exists. The one-time setup token is printed in the server log at start and by
// `incus-backup-server user setup-token`. Creates the first administrator and signs in.
import { ref, computed } from 'vue'
import { useRouter } from 'vue-router'
import AuthLayout from '@/components/app/AuthLayout.vue'
import FormField from '@/components/form/FormField.vue'
import PasswordInput from '@/components/form/PasswordInput.vue'
import CodeBlock from '@/components/CodeBlock.vue'
import Spinner from '@/components/Spinner.vue'
import { api } from '@/api/endpoints'
import { login, initSession } from '@/stores/session'
import { passwordStrength } from '@/lib/password'
import { errorText } from '@/stores/toast'
import { crossSiteHint } from '@/lib/authErrors'

const router = useRouter()
const token = ref('')
const name = ref('admin')
const password = ref('')
const confirm = ref('')
const busy = ref(false)
const error = ref('')
const nameError = computed(() => (name.value && /[\s/:]/.test(name.value) ? 'No spaces, “/” or “:” in user names.' : ''))
const pwOk = computed(() => passwordStrength(password.value, [name.value]).acceptable)
const mismatch = computed(() => confirm.value !== '' && confirm.value !== password.value)
const canSubmit = computed(() => token.value.trim() && name.value && !nameError.value && pwOk.value && confirm.value === password.value && !busy.value)

async function submit() {
  if (!canSubmit.value) return
  busy.value = true
  error.value = ''
  try {
    await api.setup.complete({ token: token.value.trim(), name: name.value.trim(), password: password.value })
    await login({ name: name.value.trim(), password: password.value })
    password.value = confirm.value = ''
    await router.replace({ name: 'dashboard' })
  } catch (e) {
    error.value = crossSiteHint(e) ?? errorText(e)
    if (!crossSiteHint(e) && /token/i.test(error.value)) error.value = `${error.value.replace(/^bad request: /, '').replace(/\.?$/, '.')} Use the newest token from the server log, or print a new one with the command below.`
    await initSession(true)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <AuthLayout title="Set up incus-backup" lede="No user exists yet. Create the first administrator account." wide>
    <form class="form-grid" data-testid="setup-form" @submit.prevent="submit">
      <FormField v-slot="f" label="Setup token" hint="Printed in the server log at start-up. Only the newest token works, and only once.">
        <input :id="f.id" v-model="token" class="input mono" autocomplete="off" spellcheck="false" :aria-describedby="f.describedby" data-testid="setup-token" required />
      </FormField>
      <CodeBlock code="incus-backup-server user setup-token" prompt label="Or print it on the server" />
      <FormField v-slot="f" label="Administrator name" :error="nameError">
        <input :id="f.id" v-model="name" class="input" autocomplete="username" autocapitalize="none" spellcheck="false" :aria-invalid="f.invalid" :aria-describedby="f.describedby" data-testid="setup-name" required />
      </FormField>
      <FormField v-slot="f" label="Password" hint="At least 12 characters. A passphrase of several words works well.">
        <PasswordInput :id="f.id" v-model="password" strength :context="[name]" autocomplete="new-password" :describedby="f.describedby" name="new-password" />
      </FormField>
      <FormField v-slot="f" label="Repeat the password" :error="mismatch ? 'The passwords do not match.' : ''">
        <PasswordInput :id="f.id" v-model="confirm" autocomplete="new-password" :describedby="f.describedby" :invalid="f.invalid" name="confirm-password" />
      </FormField>
      <p v-if="error" class="err" role="alert" data-testid="setup-error">{{ error }}</p>
      <button type="submit" class="btn btn-primary btn-block" :disabled="!canSubmit"><Spinner v-if="busy" :size="13" />Create administrator and sign in</button>
    </form>
  </AuthLayout>
</template>

<style scoped>
.err { color: var(--critical-text); font-size: var(--fs-sm); }
</style>
