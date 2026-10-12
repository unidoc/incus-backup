<script setup lang="ts">
// Create a silence: matchers (globs on type, key, host, repository, object), an end time (presets or
// a date), a reason. Shows which open alerts it would hold right now (the same glob as the server).
// Opened empty from the silences page or pre-filled from an alert.
import { computed, ref, watch } from 'vue'
import Modal from '@/components/Modal.vue'
import FormField from '@/components/form/FormField.vue'
import Spinner from '@/components/Spinner.vue'
import SeverityBadge from '@/components/SeverityBadge.vue'
import { cfg, list, type Alert, type Silence, type SilenceInput } from '@/api/sections/config'
import { silenceMatches, hasMatcher } from '@/lib/glob'
import { alertType } from '@/lib/alertTypes'
import { toast, errorText } from '@/stores/toast'

const props = defineProps<{ open: boolean; prefill?: Partial<SilenceInput> | null; title?: string }>()
const emit = defineEmits<{ 'update:open': [boolean]; created: [Silence] }>()

const PRESETS = [
  { id: '1h', label: '1 hour', ms: 3600e3 }, { id: '4h', label: '4 hours', ms: 4 * 3600e3 }, { id: '12h', label: '12 hours', ms: 12 * 3600e3 },
  { id: '1d', label: '1 day', ms: 86400e3 }, { id: '3d', label: '3 days', ms: 3 * 86400e3 }, { id: '7d', label: '1 week', ms: 7 * 86400e3 },
]
const m = ref({ type_glob: '', key_glob: '', host_glob: '', repo_glob: '', object_glob: '' })
const preset = ref('4h')
const custom = ref('')
const reason = ref('')
const busy = ref(false)
const err = ref('')
const openAlerts = ref<Alert[]>([])

function localInput(d: Date) {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`
}

watch(() => props.open, async (o) => {
  if (!o) return
  const p = props.prefill ?? {}
  m.value = { type_glob: p.type_glob ?? '', key_glob: p.key_glob ?? '', host_glob: p.host_glob ?? '', repo_glob: p.repo_glob ?? '', object_glob: p.object_glob ?? '' }
  reason.value = p.reason ?? ''
  preset.value = '4h'
  custom.value = localInput(new Date(Date.now() + 86400e3))
  err.value = ''
  try { openAlerts.value = list(await cfg.alerts.list({ active: 1, limit: 1000 })) } catch { openAlerts.value = [] }
}, { immediate: true })

const until = computed<Date | null>(() => {
  if (preset.value === 'custom') {
    const d = new Date(custom.value)
    return isNaN(d.getTime()) ? null : d
  }
  const p = PRESETS.find((x) => x.id === preset.value)
  return p ? new Date(Date.now() + p.ms) : null
})
const matched = computed(() => openAlerts.value.filter((a) => silenceMatches(m.value, a)))
const problems = computed(() => {
  const out: string[] = []
  if (!hasMatcher(m.value)) out.push('Set at least one matcher: a silence never matches everything.')
  if (!until.value || until.value.getTime() <= Date.now()) out.push('The end must be in the future.')
  if (!reason.value.trim()) out.push('Give a reason, so others know why alerts are quiet.')
  return out
})

async function submit() {
  if (problems.value.length) { err.value = problems.value[0]; return }
  busy.value = true
  err.value = ''
  try {
    const s = await cfg.silences.create({ ...m.value, until: until.value!.toISOString(), reason: reason.value.trim() })
    toast.success('Silence created', `${matched.value.length} open alert${matched.value.length === 1 ? '' : 's'} held until ${until.value!.toLocaleString()}.`)
    emit('created', s)
    emit('update:open', false)
  } catch (e) {
    err.value = errorText(e)
  } finally {
    busy.value = false
  }
}
const fields = [
  { k: 'type_glob', label: 'Alert type', ph: 'host_offline or backup_*' },
  { k: 'host_glob', label: 'Host', ph: 'node1 or node*' },
  { k: 'repo_glob', label: 'Repository', ph: 'custody' },
  { k: 'object_glob', label: 'Object', ph: 'default/web-*' },
  { k: 'key_glob', label: 'Alert key', ph: 'exact key of one alert' },
] as const
</script>

<template>
  <Modal :open="open" :title="title ?? 'Silence alerts'" description="Nothing is sent for matching alerts while the silence lasts. They stay visible here." width="640px" @update:open="(v) => emit('update:open', v)">
    <form id="silence-form" class="cx-form" data-testid="silence-form" @submit.prevent="submit">
      <fieldset class="cx-fieldset">
        <legend>Matchers</legend>
        <p class="cx-help"><code>*</code> matches anything (also <code>/</code>), <code>?</code> one character. Empty matches everything; all set matchers must match.</p>
        <div class="cx-cols">
          <FormField v-for="f in fields" :key="f.k" v-slot="ff" :label="f.label" optional>
            <input :id="ff.id" v-model="m[f.k]" class="input mono" :placeholder="f.ph" autocomplete="off" spellcheck="false" :data-testid="`sil-${f.k}`" />
          </FormField>
        </div>
      </fieldset>
      <FormField label="Duration" :hint="until ? `Ends ${until.toLocaleString()} (your time zone)` : ''">
        <div class="cx-presets" role="group" aria-label="Duration">
          <button v-for="p in PRESETS" :key="p.id" type="button" :aria-pressed="preset === p.id" @click="preset = p.id">{{ p.label }}</button>
          <button type="button" :aria-pressed="preset === 'custom'" @click="preset = 'custom'">Until…</button>
        </div>
      </FormField>
      <FormField v-if="preset === 'custom'" v-slot="f" label="Ends at">
        <input :id="f.id" v-model="custom" type="datetime-local" class="input" />
      </FormField>
      <FormField v-slot="f" label="Reason" hint="Shown with the silence and in the alert timeline">
        <input :id="f.id" v-model="reason" class="input" maxlength="500" placeholder="Planned maintenance of node1, ticket OPS-112" data-testid="sil-reason" />
      </FormField>
      <div class="preview" data-testid="silence-preview">
        <div class="ph"><strong>{{ matched.length }}</strong> open alert{{ matched.length === 1 ? '' : 's' }} match now</div>
        <ul v-if="matched.length" class="ml">
          <li v-for="a in matched.slice(0, 6)" :key="a.id"><SeverityBadge :severity="a.severity" /> <span class="t">{{ a.title }}</span> <span class="muted small">{{ alertType(a.type).label }}</span></li>
          <li v-if="matched.length > 6" class="muted small">and {{ matched.length - 6 }} more</li>
        </ul>
        <p v-else-if="hasMatcher(m)" class="muted small">It will still hold alerts that start matching while it lasts.</p>
      </div>
      <p v-if="err" class="cx-err" role="alert">{{ err }}</p>
    </form>
    <template #footer>
      <button type="button" class="btn btn-ghost" @click="emit('update:open', false)">Cancel</button>
      <button type="submit" form="silence-form" class="btn btn-primary" :disabled="busy || problems.length > 0" :title="problems[0]" data-testid="silence-submit"><Spinner v-if="busy" :size="13" />Create silence</button>
    </template>
  </Modal>
</template>

<style scoped>
.preview { border: 1px dashed var(--border-strong); border-radius: var(--r); padding: 10px 12px; background: var(--surface-2); }
.ph { font-size: var(--fs-sm); color: var(--text-2); }
.ml { list-style: none; margin: 8px 0 0; padding: 0; display: grid; gap: 6px; }
.ml li { display: flex; align-items: center; gap: 8px; min-width: 0; font-size: var(--fs-sm); }
.ml .t { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; min-width: 0; }
</style>
