<script setup lang="ts">
// Forced password change (an administrator created the account or reset the password with
// "must change"). Nothing else opens until it is done.
import { ref, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AuthLayout from '@/components/app/AuthLayout.vue'
import FormField from '@/components/form/FormField.vue'
import PasswordInput from '@/components/form/PasswordInput.vue'
import Spinner from '@/components/Spinner.vue'
import { api } from '@/api/endpoints'
import { refreshUser, session, logout } from '@/stores/session'
import { passwordStrength } from '@/lib/password'
import { errorText, toast } from '@/stores/toast'

const route = useRoute()
const router = useRouter()
const current = ref('')
const pw = ref('')
const confirm = ref('')
const busy = ref(false)
const error = ref('')
const ok = computed(() => current.value && passwordStrength(pw.value, [session.name]).acceptable && pw.value === confirm.value && pw.value !== current.value)

async function submit() {
  if (!ok.value || busy.value) return
  busy.value = true
  error.value = ''
  try {
    await api.me.changePassword({ current: current.value, new: pw.value })
    await refreshUser()
    toast.success('Password changed', 'Your other sessions were signed out.')
    const next = typeof route.query.next === 'string' && route.query.next.startsWith('/') ? route.query.next : '/'
    await router.replace(next)
  } catch (e) {
    error.value = errorText(e)
  } finally {
    busy.value = false
  }
}
async function cancel() { await logout(); await router.replace({ name: 'login' }) }
</script>

<template>
  <AuthLayout title="Choose a new password" :lede="`${session.name}, your password was set by an administrator. Choose your own before you continue.`">
    <form class="form-grid" data-testid="change-password-form" @submit.prevent="submit">
      <FormField v-slot="f" label="Current password">
        <PasswordInput :id="f.id" v-model="current" autocomplete="current-password" name="current" />
      </FormField>
      <FormField v-slot="f" label="New password" :error="pw && pw === current ? 'Choose a password different from the current one.' : ''">
        <PasswordInput :id="f.id" v-model="pw" strength :context="[session.name]" autocomplete="new-password" name="new" />
      </FormField>
      <FormField v-slot="f" label="Repeat the new password" :error="confirm && confirm !== pw ? 'The passwords do not match.' : ''">
        <PasswordInput :id="f.id" v-model="confirm" autocomplete="new-password" name="confirm" />
      </FormField>
      <p v-if="error" class="err" role="alert">{{ error }}</p>
      <button type="submit" class="btn btn-primary btn-block" :disabled="!ok || busy"><Spinner v-if="busy" :size="13" />Change password</button>
      <button type="button" class="btn btn-ghost btn-block" @click="cancel">Sign out instead</button>
    </form>
  </AuthLayout>
</template>

<style scoped>
.err { color: var(--critical-text); font-size: var(--fs-sm); }
</style>
