<script setup lang="ts">
// An administrator's view of how a user signs in through identity providers: the links (bound to a
// subject, or pending: linked by e-mail, waiting for the first sign-in), add a link, remove one.
// Linking is explicit on purpose: the server never matches accounts by e-mail alone.
import { computed, ref, watch } from 'vue'
import Modal from '@/components/Modal.vue'
import Spinner from '@/components/Spinner.vue'
import Callout from '@/components/Callout.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import FormField from '@/components/form/FormField.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import { authn, type Identity, type OIDCProvider } from '@/api/sections/authn'
import { toast, errorText } from '@/stores/toast'
import type { User } from '@/api/types'

const props = defineProps<{ user: User | null }>()
const emit = defineEmits<{ close: []; changed: [] }>()

const ids = ref<Identity[]>([])
const providers = ref<OIDCProvider[]>([])
const loading = ref(false)
const error = ref('')
const form = ref({ provider: '', email: '', busy: false, error: '' })

watch(() => props.user, async (u) => {
  if (!u) return
  loading.value = true
  error.value = ''
  form.value = { provider: '', email: u.email, busy: false, error: '' }
  try {
    const [i, p] = await Promise.all([authn.identities(u.name), authn.providers()])
    ids.value = i
    providers.value = p
    form.value.provider = p.find((x) => !i.some((y) => y.provider === x.name))?.name ?? ''
  } catch (e) { error.value = errorText(e) } finally { loading.value = false }
}, { immediate: true })

const free = computed(() => providers.value.filter((p) => !ids.value.some((i) => i.provider === p.name)).map((p) => ({ value: p.name, label: p.display_name || p.name })))

async function link() {
  const u = props.user
  if (!u) return
  form.value.busy = true
  form.value.error = ''
  try {
    await authn.link(u.name, form.value.provider, form.value.email.trim())
    ids.value = await authn.identities(u.name)
    toast.success('Linked', `The first sign-in of ${form.value.email.trim()} at ${form.value.provider} opens ${u.name}.`)
    emit('changed')
  } catch (e) { form.value.error = errorText(e) } finally { form.value.busy = false }
}
async function unlink(i: Identity) {
  const u = props.user
  if (!u) return
  try {
    await authn.unlink(u.name, i.provider)
    ids.value = await authn.identities(u.name)
    toast.success('Link removed', `${u.name} can no longer sign in through ${i.provider}.`)
    emit('changed')
  } catch (e) { toast.error(e, 'Could not remove the link') }
}
</script>

<template>
  <Modal :open="!!user" :title="`Single sign-on of ${user?.name}`" width="600px" @update:open="(v: boolean) => { if (!v) emit('close') }">
    <div class="ui" data-testid="identities-dialog">
      <div v-if="loading" class="row"><Spinner /> Loading</div>
      <Callout v-else-if="error" status="critical" title="Could not load the links">{{ error }}</Callout>
      <template v-else>
        <ul v-if="ids.length" class="ids">
          <li v-for="i in ids" :key="i.provider">
            <div>
              <div class="nm">{{ i.provider }} <StatusPill :status="i.pending ? 'warn' : 'ok'" :label="i.pending ? 'waiting for first sign-in' : 'linked'" size="sm" /></div>
              <div class="sub">{{ i.email }}<template v-if="!i.pending"> · subject <code>{{ i.subject }}</code> · last sign-in <RelativeTime :time="i.last_login_at" never="never" /></template></div>
            </div>
            <button type="button" class="btn btn-sm" @click="unlink(i)">Remove link</button>
          </li>
        </ul>
        <p v-else class="sub">Not linked to an identity provider.</p>
        <form v-if="free.length" id="link-form" class="form-grid" @submit.prevent="link">
          <div class="cols">
            <FormField v-slot="f" label="Identity provider"><SelectInput :id="f.id" v-model="form.provider" :options="free" /></FormField>
            <FormField v-slot="f" label="E-mail at the provider" hint="Must be verified there"><input :id="f.id" v-model="form.email" type="email" class="input" required data-testid="link-email" /></FormField>
          </div>
          <p class="sub">The first sign-in at that provider with this verified address becomes this account (once). After that, only the provider's subject id matches, never the address. The provider's role mappings then decide the role at every sign-in.</p>
          <Callout v-if="form.error" status="critical" title="Could not link">{{ form.error }}</Callout>
        </form>
        <p v-else-if="!providers.length" class="sub">No identity provider is set up yet (Settings → Authentication).</p>
      </template>
    </div>
    <template #footer>
      <button type="button" class="btn" @click="emit('close')">Close</button>
      <button v-if="free.length && !loading" type="submit" form="link-form" class="btn btn-primary" :disabled="form.busy || !form.provider || !form.email.includes('@')" data-testid="link-submit"><Spinner v-if="form.busy" :size="13" />Link</button>
    </template>
  </Modal>
</template>

<style scoped>
.ui { display: grid; gap: 14px; }
.row { display: flex; gap: 8px; align-items: center; color: var(--text-muted); }
.ids { list-style: none; margin: 0; padding: 0; border: 1px solid var(--border); border-radius: var(--r); }
.ids li { display: flex; justify-content: space-between; gap: 12px; align-items: center; padding: 10px 12px; }
.ids li + li { border-top: 1px solid var(--border); }
.nm { font-weight: var(--fw-medium); display: flex; gap: 8px; align-items: center; }
.sub { color: var(--text-muted); font-size: var(--fs-sm); overflow-wrap: anywhere; }
.cols { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1.3fr); gap: 12px; }
@media (max-width: 560px) { .cols { grid-template-columns: 1fr; } .ids li { flex-direction: column; align-items: flex-start; } }
</style>
