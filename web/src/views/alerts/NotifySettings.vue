<script setup lang="ts">
// Alert conditions and the engine's noise control: per condition enabled, base severity and
// thresholds (typed fields with units and the defaults), plus dedupe, flapping, retries, retention.
import { computed, ref, watch } from 'vue'
import SubNav from '@/components/config/SubNav.vue'
import FormField from '@/components/form/FormField.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import SeverityBadge from '@/components/SeverityBadge.vue'
import Callout from '@/components/Callout.vue'
import Skeleton from '@/components/Skeleton.vue'
import Spinner from '@/components/Spinner.vue'
import Modal from '@/components/Modal.vue'
import { useResource } from '@/api/cache'
import { cfg, list, canCfg, type Condition, type NotifySettings, type Severity } from '@/api/sections/config'
import { ALERT_GROUPS, alertType, type AlertTypeDef } from '@/lib/alertTypes'
import { session } from '@/stores/session'
import { toast, errorText } from '@/stores/toast'
import '@/components/config/section.css'

const res = useResource(() => 'cfg:notify-settings', (s) => cfg.notify.get(s))
const canEdit = computed(() => canCfg(session.role, 'PUT /notifications/settings'))

// ---- engine settings
const DEFAULTS: NotifySettings = { dedupe_minutes: 10, flap_window_minutes: 60, flap_threshold: 3, max_attempts: 8, backoff_base_seconds: 60, backoff_max_seconds: 21600, event_expire_minutes: 1440, alert_retention_days: 90, delivery_retention_days: 30 }
const FIELDS: { k: keyof NotifySettings; label: string; unit: string; min: number; help: string; group: string }[] = [
  { k: 'dedupe_minutes', label: 'Dedupe window', unit: 'min', min: 0, group: 'Noise', help: 'A rule never sends a firing or reminder for the same alert twice within this window.' },
  { k: 'flap_threshold', label: 'Flapping after', unit: 'firings', min: 2, group: 'Noise', help: 'An alert that fires this often within the window is flapping: nothing is sent until it settles.' },
  { k: 'flap_window_minutes', label: 'Flapping window', unit: 'min', min: 1, group: 'Noise', help: 'It is released after staying in one state for a whole window.' },
  { k: 'event_expire_minutes', label: 'Events expire after', unit: 'min', min: 1, group: 'Noise', help: 'Events (sign-in security, failed jobs) have no clearing condition: they resolve silently after this long.' },
  { k: 'max_attempts', label: 'Attempts', unit: 'tries', min: 1, group: 'Delivery', help: 'After this many failures a message is dead and waits in the delivery log.' },
  { k: 'backoff_base_seconds', label: 'First retry after', unit: 's', min: 1, group: 'Delivery', help: 'Each retry waits twice as long as the one before.' },
  { k: 'backoff_max_seconds', label: 'Longest pause', unit: 's', min: 1, group: 'Delivery', help: 'The pause between retries never grows beyond this.' },
  { k: 'alert_retention_days', label: 'Keep resolved alerts', unit: 'days', min: 1, group: 'Retention', help: 'Resolved alerts and ended silences are deleted after this.' },
  { k: 'delivery_retention_days', label: 'Keep delivery log', unit: 'days', min: 1, group: 'Retention', help: 'Sent and dead messages are deleted after this.' },
]
const st = ref<NotifySettings>({ ...DEFAULTS })
watch(() => res.data.value, (d) => { if (d) st.value = { ...d.settings } }, { immediate: true })
const dirty = computed(() => !!res.data.value && JSON.stringify(st.value) !== JSON.stringify(res.data.value.settings))
const stErr = computed(() => FIELDS.find((f) => !(Number.isInteger(st.value[f.k]) && st.value[f.k] >= f.min)))
const schedule = computed(() => {
  const out: string[] = []
  let w = st.value.backoff_base_seconds
  for (let i = 1; i < Math.min(st.value.max_attempts, 9); i++) { out.push(fmtS(Math.min(w, st.value.backoff_max_seconds))); w *= 2 }
  return out.join(', ')
})
function fmtS(s: number) { return s < 120 ? `${s} s` : s < 7200 ? `${Math.round(s / 60)} min` : `${Math.round(s / 360) / 10} h` }
const savingSt = ref(false)
async function saveSettings() {
  if (stErr.value) return
  savingSt.value = true
  try { await cfg.notify.put({ ...st.value }); toast.success('Notification settings saved'); void res.refresh() } catch (e) { toast.error(e, 'Could not save the notification settings') } finally { savingSt.value = false }
}

// ---- conditions
const conds = computed(() => list(res.data.value?.conditions))
const grouped = computed(() => {
  const g: { group: string; items: { c: Condition; def: AlertTypeDef }[] }[] = []
  for (const name of [...ALERT_GROUPS, 'Other']) {
    const items = conds.value.map((c) => ({ c, def: alertType(c.type) })).filter((x) => x.def.group === name)
    if (items.length) g.push({ group: name, items })
  }
  return g
})
const edit = ref<{ c: Condition; def: AlertTypeDef; enabled: boolean; severity: Severity; params: Record<string, string> } | null>(null)
const editErr = ref('')
const editBusy = ref(false)
function openEdit(c: Condition) {
  const def = alertType(c.type)
  edit.value = { c, def, enabled: c.enabled, severity: c.severity, params: Object.fromEntries(def.params.map((p) => [p.key, String(c.params?.[p.key] ?? p.def)])) }
  editErr.value = ''
}
async function saveCondition() {
  const e = edit.value
  if (!e) return
  const params: Record<string, number | string> = {}
  for (const p of e.def.params) {
    const v = e.params[p.key]
    if (p.severity) { params[p.key] = v; continue }
    const n = Number(v)
    if (v === '' || !Number.isFinite(n) || n < 0) { editErr.value = `${p.label}: a number, zero or more.`; return }
    params[p.key] = n
  }
  editBusy.value = true
  editErr.value = ''
  try {
    await cfg.notify.setCondition(e.c.type, { enabled: e.enabled, severity: e.severity, params })
    toast.success(`${e.def.label} saved`, e.enabled ? 'Applies at the next evaluation (within a minute).' : 'Its open alerts resolve without a message.')
    edit.value = null
    void res.refresh()
  } catch (x) { editErr.value = errorText(x) } finally { editBusy.value = false }
}
async function toggle(c: Condition, on: boolean) {
  try { await cfg.notify.setCondition(c.type, { enabled: on, severity: c.severity, params: {} }); toast.success(on ? `${alertType(c.type).label} on` : `${alertType(c.type).label} off`, on ? undefined : 'Its open alerts resolve without a message.'); void res.refresh() } catch (e) { toast.error(e, 'Could not change the condition') }
}
const paramText = (c: Condition, d: AlertTypeDef) => d.params.map((p) => {
  const v = c.params?.[p.key] ?? p.def
  return `${p.label.toLowerCase()} ${v}${p.unit && !p.severity ? ' ' + p.unit : ''}${String(v) !== String(p.def) ? ' (default ' + p.def + ')' : ''}`
}).join(' · ')
</script>

<template>
  <div>
    <header class="page-head">
      <div>
        <h1>Alert conditions</h1>
        <p class="lede">What the server watches every minute, how loud each condition is, and the rules against noise. Defaults are conservative: an alert should mean “look at this now”.</p>
      </div>
    </header>
    <SubNav group="notifications" />
    <Callout v-if="res.error.value && !res.data.value" status="critical" title="Could not load the settings" :retry="res.refresh">{{ res.error.value.message }}</Callout>
    <div v-else class="cx-stack">
      <section class="panel" data-testid="conditions">
        <div class="panel-head"><h2>Conditions</h2><span class="sub">{{ conds.filter((c) => c.enabled).length }} of {{ conds.length }} on</span></div>
        <div v-if="res.loading.value" class="panel-body"><Skeleton :lines="8" /></div>
        <div v-else class="cl">
          <div v-for="g in grouped" :key="g.group" class="cg">
            <h3 class="gh">{{ g.group }}</h3>
            <div v-for="{ c, def } in g.items" :key="c.type" class="row" :class="{ off: !c.enabled }" :data-condition="c.type">
              <SwitchInput :model-value="c.enabled" :disabled="!canEdit" :label="''" :aria-label="`${def.label} on or off`" @update:model-value="(v: boolean) => toggle(c, v)" />
              <div class="cm">
                <div class="ct"><strong>{{ def.label }}</strong><SeverityBadge :severity="c.severity" /><span v-if="def.event" class="cx-tag">event</span></div>
                <p class="cd">{{ def.fires }}</p>
                <p v-if="def.params.length" class="cp">{{ paramText(c, def) }}</p>
              </div>
              <button type="button" class="btn btn-sm" :disabled="!canEdit" :title="canEdit ? '' : 'Needs the admin role'" :data-testid="`cond-edit-${c.type}`" @click="openEdit(c)">Edit</button>
            </div>
          </div>
        </div>
      </section>

      <section class="panel" data-testid="engine-settings">
        <div class="panel-head"><h2>Noise control, delivery and retention</h2></div>
        <form class="panel-body cx-form" @submit.prevent="saveSettings">
          <template v-for="grp in ['Noise', 'Delivery', 'Retention']" :key="grp">
            <fieldset class="cx-fieldset">
              <legend>{{ grp }}</legend>
              <div class="cx-cols">
                <FormField v-for="f in FIELDS.filter((x) => x.group === grp)" :key="f.k" v-slot="ff" :label="f.label" :hint="`${f.help} Default ${DEFAULTS[f.k]} ${f.unit}.`">
                  <div class="cx-unit"><input :id="ff.id" v-model.number="st[f.k]" type="number" :min="f.min" step="1" class="input num" :disabled="!canEdit" :data-setting="f.k" /><span class="u">{{ f.unit }}</span></div>
                </FormField>
              </div>
              <p v-if="grp === 'Delivery'" class="cx-help">Retries wait {{ schedule }}; then the message is dead.</p>
            </fieldset>
          </template>
          <p v-if="stErr" class="cx-err">{{ stErr.label }}: a whole number, at least {{ stErr.min }}.</p>
          <div class="form-actions">
            <button type="button" class="btn btn-ghost" :disabled="!dirty" @click="st = { ...res.data.value!.settings }">Discard changes</button>
            <button type="button" class="btn" :disabled="!canEdit" @click="st = { ...DEFAULTS }">Use the defaults</button>
            <button type="submit" class="btn btn-primary" :disabled="!canEdit || !dirty || !!stErr || savingSt" data-testid="settings-save"><Spinner v-if="savingSt" :size="13" />Save</button>
          </div>
        </form>
      </section>
    </div>

    <Modal :open="!!edit" :title="edit?.def.label ?? ''" :description="edit?.def.fires" width="560px" @update:open="(v: boolean) => { if (!v) edit = null }">
      <form v-if="edit" id="cond-form" class="cx-form" data-testid="cond-form" @submit.prevent="saveCondition">
        <div class="cx-cols">
          <FormField label="Enabled" hint="Off resolves its open alerts without a message"><SwitchInput v-model="edit.enabled" :label="edit.enabled ? 'Watching' : 'Off'" /></FormField>
          <FormField v-slot="ff" label="Base severity"><SelectInput :id="ff.id" v-model="edit.severity" :options="['info', 'warning', 'critical']" /></FormField>
        </div>
        <FormField v-for="p in edit.def.params" :key="p.key" v-slot="ff" :label="p.label" :hint="`${p.help} Default ${p.def}${p.unit && !p.severity ? ' ' + p.unit : ''}.`">
          <SelectInput v-if="p.severity" :id="ff.id" v-model="edit.params[p.key]" :options="['info', 'warning', 'critical']" />
          <div v-else class="cx-unit"><input :id="ff.id" v-model="edit.params[p.key]" type="number" min="0" :step="p.step ?? 1" class="input num" :data-param="p.key" /><span class="u">{{ p.unit }}</span></div>
        </FormField>
        <p v-if="!edit.def.params.length" class="cx-help">This condition has no thresholds.</p>
        <p v-if="editErr" class="cx-err" role="alert">{{ editErr }}</p>
      </form>
      <template #footer>
        <button type="button" class="btn btn-ghost" @click="edit = null">Cancel</button>
        <button type="submit" form="cond-form" class="btn btn-primary" :disabled="editBusy" data-testid="cond-save"><Spinner v-if="editBusy" :size="13" />Save</button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.cl { display: grid; }
.cg { border-top: 1px solid var(--border); }
.cg:first-child { border-top: 0; }
.gh { font-size: var(--fs-xs); text-transform: uppercase; letter-spacing: .06em; color: var(--text-muted); padding: 12px 16px 4px; }
.row { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; gap: 12px; align-items: start; padding: 10px 16px; }
.row + .row { border-top: 1px dashed var(--border); }
.row.off .cm { opacity: .6; }
.ct { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.cd { color: var(--text-2); font-size: var(--fs-sm); margin-top: 3px; }
.cp { color: var(--text-muted); font-size: var(--fs-xs); margin-top: 3px; font-variant-numeric: tabular-nums; }
</style>
