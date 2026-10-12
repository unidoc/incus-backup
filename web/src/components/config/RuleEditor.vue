<script setup lang="ts">
// Create or edit a notification rule (drawer): which alerts (types, severity, host/repository/object
// globs), which channels, and when (reminders, quiet hours with time zone, resolved messages), or a
// daily digest. PUT replaces the rule, so the whole rule is always sent.
import { computed, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import Modal from '@/components/Modal.vue'
import FormField from '@/components/form/FormField.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import CIcon from './CIcon.vue'
import { cfg, defaultRule, list, type Channel, type Rule, type RuleInput } from '@/api/sections/config'
import { ALERT_GROUPS, ALERT_TYPES } from '@/lib/alertTypes'
import { KIND_META } from './channelKinds'
import { toast, errorText } from '@/stores/toast'

const props = defineProps<{ open: boolean; rule: Rule | null; channels: Channel[] }>()
const emit = defineEmits<{ 'update:open': [boolean]; saved: [Rule] }>()
const creating = computed(() => !props.rule)
const r = ref<RuleInput>(defaultRule())
const allTypes = ref(true)
const custom = ref('')
const quiet = ref(false)
const repeatPreset = ref('1440')
const busy = ref(false)
const err = ref('')
const touched = ref(false)

const known = new Set(ALERT_TYPES.map((t) => t.type))
watch(() => props.open, (o) => {
  if (!o) return
  const src = props.rule
  r.value = src ? { ...defaultRule(), ...src, event_types: [...list(src.event_types)], channel_ids: [...list(src.channel_ids)] } : { ...defaultRule(), timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC', channel_ids: props.channels.length === 1 ? [props.channels[0].id] : [] }
  delete (r.value as Partial<Rule>).id; delete (r.value as Partial<Rule>).created; delete (r.value as Partial<Rule>).last_digest_at
  allTypes.value = !list(r.value.event_types).length
  custom.value = list(r.value.event_types).filter((t) => !known.has(t)).join(', ')
  r.value.event_types = list(r.value.event_types).filter((t) => known.has(t))
  quiet.value = !!r.value.quiet_start
  if (!quiet.value) { r.value.quiet_start = '22:00'; r.value.quiet_end = '07:00' }
  repeatPreset.value = ['0', '60', '240', '720', '1440'].includes(String(r.value.repeat_minutes)) ? String(r.value.repeat_minutes) : 'custom'
  err.value = ''
  touched.value = false
}, { immediate: true })

function toggleType(t: string) {
  const s = new Set(list(r.value.event_types))
  s.has(t) ? s.delete(t) : s.add(t)
  r.value.event_types = [...s]
}
function toggleGroup(g: string, on: boolean) {
  const s = new Set(list(r.value.event_types))
  for (const t of ALERT_TYPES.filter((x) => x.group === g)) on ? s.add(t.type) : s.delete(t.type)
  r.value.event_types = [...s]
}
const groupState = (g: string) => {
  const ts = ALERT_TYPES.filter((x) => x.group === g)
  const n = ts.filter((t) => list(r.value.event_types).includes(t.type)).length
  return n === 0 ? 'none' : n === ts.length ? 'all' : 'some'
}
function toggleChannel(id: string) {
  const s = new Set(list(r.value.channel_ids))
  s.has(id) ? s.delete(id) : s.add(id)
  r.value.channel_ids = [...s]
}
watch(repeatPreset, (p) => { if (p !== 'custom') r.value.repeat_minutes = Number(p) })

const zones = (() => {
  try { return (Intl as unknown as { supportedValuesOf(k: string): string[] }).supportedValuesOf('timeZone') } catch { return ['UTC'] }
})()
const zoneOpts = computed(() => (zones.includes('UTC') ? zones : ['UTC', ...zones]).map((z) => ({ value: z, label: z })))
const HHMM = /^([01]\d|2[0-3]):[0-5]\d$/
const types = computed(() => allTypes.value ? [] : [...list(r.value.event_types), ...custom.value.split(',').map((x) => x.trim()).filter(Boolean)])
const errors = computed(() => {
  const e: Record<string, string> = {}
  if (!/^[A-Za-z0-9._-]{1,64}$/.test(r.value.name)) e.name = 'Letters, digits, dot, dash and underscore; up to 64 characters.'
  if (!list(r.value.channel_ids).length) e.channels = 'Pick at least one channel.'
  if (r.value.kind === 'alert' && !allTypes.value && !types.value.length) e.types = 'Pick at least one alert type, or choose every type.'
  if (quiet.value && r.value.kind === 'alert' && (!HHMM.test(r.value.quiet_start) || !HHMM.test(r.value.quiet_end))) e.quiet = 'Times as HH:MM, 24-hour clock.'
  if (r.value.kind === 'digest' && !HHMM.test(r.value.digest_at)) e.digest = 'A time as HH:MM.'
  if (!(r.value.repeat_minutes >= 0)) e.repeat = 'Zero (never) or more minutes.'
  return e
})
const shown = (k: string) => (touched.value ? errors.value[k] ?? '' : '')

async function save() {
  touched.value = true
  if (Object.keys(errors.value).length) { err.value = 'Fix the marked fields.'; return }
  busy.value = true
  err.value = ''
  const body: RuleInput = { ...r.value, event_types: types.value, quiet_start: quiet.value ? r.value.quiet_start : '', quiet_end: quiet.value ? r.value.quiet_end : '' }
  try {
    const x = creating.value ? await cfg.rules.create(body) : await cfg.rules.update(props.rule!.id, body)
    toast.success(creating.value ? 'Rule created' : 'Rule saved')
    emit('saved', x)
    emit('update:open', false)
  } catch (e) {
    err.value = errorText(e)
  } finally {
    busy.value = false
  }
}
const repeatOpts = [{ value: '0', label: 'Never' }, { value: '60', label: 'Every hour' }, { value: '240', label: 'Every 4 hours' }, { value: '720', label: 'Every 12 hours' }, { value: '1440', label: 'Every day' }, { value: 'custom', label: 'Custom…' }]
</script>

<template>
  <Modal :open="open" drawer :title="creating ? 'Add a rule' : `Edit rule ${rule?.name}`" description="A rule sends matching alerts to its channels." width="680px" @update:open="(v: boolean) => emit('update:open', v)">
    <form id="rule-form" class="cx-form" data-testid="rule-form" novalidate @submit.prevent="save">
      <div class="cx-cols">
        <FormField v-slot="ff" label="Name" :error="shown('name')"><input :id="ff.id" v-model="r.name" class="input" placeholder="page-on-call" autocomplete="off" data-testid="rule-name" /></FormField>
        <FormField label="Enabled"><SwitchInput v-model="r.enabled" :label="r.enabled ? 'Active' : 'Off'" /></FormField>
      </div>
      <FormField label="What it sends">
        <div class="cx-presets" role="radiogroup" aria-label="Rule kind">
          <button type="button" role="radio" :aria-checked="r.kind === 'alert'" :aria-pressed="r.kind === 'alert'" @click="r.kind = 'alert'">Alerts as they happen</button>
          <button type="button" role="radio" :aria-checked="r.kind === 'digest'" :aria-pressed="r.kind === 'digest'" data-testid="rule-kind-digest" @click="r.kind = 'digest'">A daily digest</button>
        </div>
      </FormField>

      <template v-if="r.kind === 'alert'">
        <fieldset class="cx-fieldset">
          <legend>Which alerts</legend>
          <SwitchInput v-model="allTypes" label="Every alert type, also future ones" data-testid="rule-all-types" />
          <div v-if="!allTypes" class="groups" data-testid="rule-types">
            <div v-for="g in ALERT_GROUPS" :key="g" class="grp">
              <label class="gh"><input type="checkbox" :checked="groupState(g) === 'all'" :indeterminate.prop="groupState(g) === 'some'" @change="toggleGroup(g, ($event.target as HTMLInputElement).checked)" />{{ g }}</label>
              <label v-for="t in ALERT_TYPES.filter((x) => x.group === g)" :key="t.type" class="ty" :title="t.fires">
                <input type="checkbox" :checked="list(r.event_types).includes(t.type)" :data-type="t.type" @change="toggleType(t.type)" />
                <span><span class="tl">{{ t.label }}</span><span class="td">{{ t.fires }}</span></span>
              </label>
            </div>
            <FormField v-slot="ff" label="More types" optional hint="Comma-separated globs for event types, e.g. login_*"><input :id="ff.id" v-model="custom" class="input mono" spellcheck="false" /></FormField>
            <p v-if="shown('types')" class="cx-err">{{ shown('types') }}</p>
          </div>
          <FormField label="Minimum severity">
            <div class="cx-seg" role="group" aria-label="Minimum severity">
              <button v-for="s in (['info', 'warning', 'critical'] as const)" :key="s" type="button" :aria-pressed="r.min_severity === s" :data-sev="s" @click="r.min_severity = s">{{ s === 'info' ? 'Info and up' : s === 'warning' ? 'Warning and up' : 'Critical only' }}</button>
            </div>
          </FormField>
          <div class="cx-cols3">
            <FormField v-slot="ff" label="Hosts" optional hint="Glob; empty = any"><input :id="ff.id" v-model="r.host_glob" class="input mono" placeholder="node*" spellcheck="false" /></FormField>
            <FormField v-slot="ff" label="Repositories" optional hint="Glob"><input :id="ff.id" v-model="r.repo_glob" class="input mono" placeholder="custody" spellcheck="false" /></FormField>
            <FormField v-slot="ff" label="Objects" optional hint="project/name"><input :id="ff.id" v-model="r.object_glob" class="input mono" placeholder="default/db-*" spellcheck="false" /></FormField>
          </div>
        </fieldset>
      </template>
      <Callout v-else status="info" title="Daily digest">One summary a day of the last 24 hours: backup runs, protected objects, verifications, stored bytes and growth, the open alerts, and how many alerts opened and resolved. It is sent once, even with two servers or a restart.</Callout>

      <fieldset class="cx-fieldset">
        <legend>Send to</legend>
        <p v-if="!channels.length" class="cx-help">No channel exists yet. <RouterLink :to="{ name: 'notifications' }">Add a channel</RouterLink> first.</p>
        <div v-else class="chs" data-testid="rule-channels">
          <label v-for="c in channels" :key="c.id" class="ch" :class="{ off: !c.enabled }">
            <input type="checkbox" :checked="list(r.channel_ids).includes(c.id)" :data-channel="c.name" @change="toggleChannel(c.id)" />
            <CIcon :name="KIND_META[c.kind]?.icon ?? 'send'" :size="15" />
            <span class="cn">{{ c.name }}</span><span class="muted small">{{ c.target }}</span><span v-if="!c.enabled" class="cx-tag">disabled</span>
          </label>
        </div>
        <p v-if="shown('channels')" class="cx-err">{{ shown('channels') }}</p>
      </fieldset>

      <fieldset class="cx-fieldset">
        <legend>When</legend>
        <template v-if="r.kind === 'alert'">
          <div class="cx-cols">
            <FormField v-slot="ff" label="Remind while it fires" hint="Unacknowledged, unsilenced alerts only" :error="shown('repeat')"><SelectInput :id="ff.id" v-model="repeatPreset" :options="repeatOpts" /></FormField>
            <FormField v-if="repeatPreset === 'custom'" v-slot="ff" label="Every"><div class="cx-unit"><input :id="ff.id" v-model.number="r.repeat_minutes" type="number" min="1" class="input num" /><span class="u">minutes</span></div></FormField>
          </div>
          <SwitchInput v-model="r.send_resolved" label="Send a message when it resolves (only if a firing message went out)" />
          <SwitchInput v-model="quiet" label="Quiet hours" data-testid="rule-quiet" />
          <div v-if="quiet" class="cx-cols3">
            <FormField v-slot="ff" label="From" :error="shown('quiet')"><input :id="ff.id" v-model="r.quiet_start" type="time" class="input" /></FormField>
            <FormField v-slot="ff" label="Until"><input :id="ff.id" v-model="r.quiet_end" type="time" class="input" /></FormField>
            <FormField label="Critical alerts"><SwitchInput v-model="r.critical_bypass" label="Ignore quiet hours" /></FormField>
          </div>
          <p v-if="quiet" class="cx-help">Quiet hours hold messages, they do not drop them: what is still true when the window ends is sent then. The window may wrap midnight.</p>
        </template>
        <FormField v-else v-slot="ff" label="Send at" :error="shown('digest')"><input :id="ff.id" v-model="r.digest_at" type="time" class="input" /></FormField>
        <FormField v-slot="ff" label="Time zone" hint="For quiet hours, the digest time and times in messages"><SelectInput :id="ff.id" v-model="r.timezone" :options="zoneOpts" /></FormField>
      </fieldset>
      <Callout v-if="err" status="critical" :title="creating ? 'Could not create the rule' : 'Could not save the rule'">{{ err }}</Callout>
    </form>
    <template #footer>
      <button type="button" class="btn btn-ghost" @click="emit('update:open', false)">Cancel</button>
      <button type="submit" form="rule-form" class="btn btn-primary" :disabled="busy" data-testid="rule-save"><Spinner v-if="busy" :size="13" />{{ creating ? 'Add rule' : 'Save' }}</button>
    </template>
  </Modal>
</template>

<style scoped>
.groups { display: grid; grid-template-columns: minmax(0, 1fr); gap: 12px; }
.grp { display: grid; grid-template-columns: minmax(0, 1fr); gap: 4px; }
.gh { display: flex; align-items: center; gap: 8px; font-weight: var(--fw-semibold); font-size: var(--fs-sm); color: var(--text-2); }
.ty { display: grid; grid-template-columns: auto 1fr; gap: 8px; align-items: start; padding: 4px 0 4px 22px; cursor: pointer; }
.ty input { margin-top: 3px; }
.tl { display: block; font-weight: var(--fw-medium); }
.td { display: block; font-size: var(--fs-xs); color: var(--text-muted); line-height: 1.4; }
.chs { display: grid; grid-template-columns: minmax(0, 1fr); gap: 6px; }
.ch { display: flex; align-items: center; gap: 8px; min-width: 0; cursor: pointer; flex-wrap: wrap; }
.ch .cn { font-weight: var(--fw-medium); }
.ch.off { opacity: .7; }
</style>
