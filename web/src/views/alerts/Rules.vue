<script setup lang="ts">
// Notification rules: which alerts reach which channels, and when. Plus "who would be notified for…":
// the server evaluates every rule against a hypothetical alert with the runner's own matching code.
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import CIcon from '@/components/config/CIcon.vue'
import SubNav from '@/components/config/SubNav.vue'
import RuleEditor from '@/components/config/RuleEditor.vue'
import DataTable from '@/components/DataTable.vue'
import EmptyState from '@/components/EmptyState.vue'
import SeverityBadge from '@/components/SeverityBadge.vue'
import StatusPill from '@/components/StatusPill.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import FormField from '@/components/form/FormField.vue'
import Spinner from '@/components/Spinner.vue'
import Callout from '@/components/Callout.vue'
import { useResource, invalidate } from '@/api/cache'
import { cfg, list, canCfg, type Rule, type SimulateResult, type Severity } from '@/api/sections/config'
import { ALERT_TYPES, ALERT_GROUPS, alertType } from '@/lib/alertTypes'
import { KIND_META } from '@/components/config/channelKinds'
import { session } from '@/stores/session'
import { toast, errorText } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import type { ColumnDef } from '@/lib/table'
import '@/components/config/section.css'

const rules = useResource(() => 'cfg:rules', (s) => cfg.rules.list(s))
const channels = useResource(() => 'cfg:channels', (s) => cfg.channels.list(s))
const canEdit = computed(() => canCfg(session.role, 'POST /notifications/rules'))
const chName = (id: string) => list(channels.data.value).find((c) => c.id === id)

const cols: ColumnDef<Rule>[] = [
  { key: 'name', label: 'Rule', required: true },
  { key: 'enabled', label: 'Enabled', width: '96px', value: (r) => (r.enabled ? 1 : 0) },
  { key: 'what', label: 'Alerts', value: (r) => (r.kind === 'digest' ? 'digest' : list(r.event_types).join(' ')) },
  { key: 'scope', label: 'Scope', value: (r) => [r.host_glob, r.repo_glob, r.object_glob].join(' ') },
  { key: 'channels', label: 'Channels', value: (r) => list(r.channel_ids).map((id) => chName(id)?.name ?? id).join(' ') },
  { key: 'when', label: 'Timing', value: (r) => r.repeat_minutes },
  { key: 'actions', label: '', sortable: false, noExport: true, align: 'right', width: '120px', required: true },
]
const typesText = (r: Rule) => {
  const t = list(r.event_types)
  if (!t.length) return 'Every type'
  return t.map((x) => alertType(x).label).join(', ')
}
const repeatText = (m: number) => (m === 0 ? 'no reminders' : m % 1440 === 0 ? `remind every ${m / 1440 === 1 ? 'day' : `${m / 1440} days`}` : m % 60 === 0 ? `remind every ${m / 60} h` : `remind every ${m} min`)

const editor = ref<{ open: boolean; rule: Rule | null }>({ open: false, rule: null })
function onSaved() { invalidate('cfg:rules'); void rules.refresh() }
async function setEnabled(r: Rule, on: boolean) {
  const { id: _id, created: _c, last_digest_at: _l, ...rest } = r
  try { await cfg.rules.update(r.id, { ...rest, event_types: list(r.event_types), channel_ids: list(r.channel_ids), enabled: on }); toast.success(on ? 'Rule enabled' : 'Rule disabled'); onSaved() } catch (e) { toast.error(e, 'Could not change the rule') }
}
async function remove(r: Rule) {
  if (!(await confirm({ title: `Delete the rule “${r.name}”?`, message: 'Its channels stop getting the alerts it matched. Open alerts are not changed.', danger: true, typeToConfirm: r.name, confirmLabel: 'Delete rule' }))) return
  try { await cfg.rules.remove(r.id); toast.success('Rule deleted'); onSaved() } catch (e) { toast.error(e, 'Could not delete the rule') }
}

// ---- simulator
const sim = ref({ type: 'backup_missed', severity: 'critical' as Severity, host: '', repository: '', object: '' })
const simBusy = ref(false)
const simErr = ref('')
const simRes = ref<SimulateResult | null>(null)
const typeOptions = ALERT_GROUPS.flatMap((g) => ALERT_TYPES.filter((t) => t.group === g).map((t) => ({ value: t.type, label: `${g}: ${t.label}` })))
async function simulate() {
  simBusy.value = true
  simErr.value = ''
  try { simRes.value = await cfg.rules.simulate({ ...sim.value }) } catch (e) { simErr.value = errorText(e); simRes.value = null } finally { simBusy.value = false }
}
const recipients = computed(() => {
  const r = simRes.value
  if (!r) return []
  const m = new Map<string, { name: string; kind: string; enabled: boolean; rules: string[]; quiet: boolean }>()
  for (const x of r.rules.filter((y) => y.matched)) {
    for (const c of x.channels) {
      const e = m.get(c.id) ?? { name: c.name, kind: c.kind, enabled: c.enabled, rules: [], quiet: true }
      e.rules.push(x.rule_name)
      e.quiet = e.quiet && x.quiet
      m.set(c.id, e)
    }
  }
  return [...m.values()]
})
</script>

<template>
  <div>
    <header class="page-head">
      <div>
        <h1>Notification rules</h1>
        <p class="lede">A rule sends matching alerts to channels: once when they fire, again when they escalate, reminders while nobody acknowledges them, and a message when they resolve.</p>
      </div>
      <div class="actions">
        <button v-if="canEdit" type="button" class="btn btn-primary" data-testid="rule-new" @click="editor = { open: true, rule: null }"><Icon name="plus" :size="14" />Add a rule</button>
      </div>
    </header>
    <SubNav group="notifications" />

    <div class="cx-stack">
      <DataTable :rows="list(rules.data.value)" :columns="cols" row-key="id" :loading="rules.loading.value" :error="rules.error.value" :retry="rules.refresh" storage-key="cfg-rules" csv-name="rules" data-testid="rule-table">
        <template #cell-name="{ row }"><span class="nm"><strong>{{ row.name }}</strong><span v-if="row.kind === 'digest'" class="cx-tag info">digest {{ row.digest_at }}</span></span></template>
        <template #cell-enabled="{ row }"><SwitchInput :model-value="row.enabled" :disabled="!canEdit" :label="row.enabled ? 'On' : 'Off'" @update:model-value="(v: boolean) => setEnabled(row, v)" /></template>
        <template #cell-what="{ row }">
          <span v-if="row.kind === 'digest'" class="muted">daily summary</span>
          <span v-else class="wh"><SeverityBadge :severity="row.min_severity" /><span class="muted small">and up ·</span><span class="tt" :title="typesText(row)">{{ typesText(row) }}</span></span>
        </template>
        <template #cell-scope="{ row }">
          <span class="cx-where">
            <span v-if="row.host_glob"><span class="k">host</span>{{ row.host_glob }}</span>
            <span v-if="row.repo_glob"><span class="k">repo</span>{{ row.repo_glob }}</span>
            <span v-if="row.object_glob"><span class="k">object</span>{{ row.object_glob }}</span>
            <span v-if="!row.host_glob && !row.repo_glob && !row.object_glob" class="muted">everything</span>
          </span>
        </template>
        <template #cell-channels="{ row }">
          <span class="chs"><span v-for="id in list(row.channel_ids)" :key="id" class="cx-tag"><CIcon :name="KIND_META[chName(id)?.kind ?? 'webhook'].icon" :size="11" />{{ chName(id)?.name ?? 'deleted' }}</span></span>
        </template>
        <template #cell-when="{ row }">
          <span v-if="row.kind === 'digest'" class="small">{{ row.digest_at }} {{ row.timezone }}</span>
          <span v-else class="small tm">{{ repeatText(row.repeat_minutes) }}<template v-if="row.quiet_start"> · quiet {{ row.quiet_start }}–{{ row.quiet_end }} {{ row.timezone }}<template v-if="row.critical_bypass"> (critical bypasses)</template></template><template v-if="!row.send_resolved"> · no resolved</template></span>
        </template>
        <template #cell-actions="{ row }">
          <span class="cx-actions">
            <button type="button" class="btn btn-sm btn-ghost" :disabled="!canEdit" :data-testid="`rule-edit-${row.name}`" @click="editor = { open: true, rule: row }"><Icon name="pencil" :size="14" />Edit</button>
            <button type="button" class="btn btn-sm btn-ghost btn-icon" :disabled="!canEdit" :aria-label="`Delete ${row.name}`" @click="remove(row)"><Icon name="trash-2" :size="14" /></button>
          </span>
        </template>
        <template #empty>
          <EmptyState title="No rules yet" icon="send">Alerts are shown in the console but nobody is told. Add a rule that sends warnings and critical alerts to a channel.
            <template v-if="canEdit" #actions><button type="button" class="btn btn-primary btn-sm" @click="editor = { open: true, rule: null }">Add a rule</button></template>
          </EmptyState>
        </template>
      </DataTable>

      <section class="panel" data-testid="simulator">
        <div class="panel-head"><h2>Who would be notified?</h2><span class="sub">Asks the server; nothing is sent</span></div>
        <form class="panel-body cx-form" @submit.prevent="simulate">
          <div class="simf">
            <FormField v-slot="ff" label="Alert type"><SelectInput :id="ff.id" v-model="sim.type" :options="typeOptions" data-testid="sim-type" /></FormField>
            <FormField v-slot="ff" label="Severity"><SelectInput :id="ff.id" v-model="sim.severity" :options="['info', 'warning', 'critical']" /></FormField>
            <FormField v-slot="ff" label="Host" optional><input :id="ff.id" v-model="sim.host" class="input" placeholder="node1" /></FormField>
            <FormField v-slot="ff" label="Repository" optional><input :id="ff.id" v-model="sim.repository" class="input" placeholder="custody" /></FormField>
            <FormField v-slot="ff" label="Object" optional><input :id="ff.id" v-model="sim.object" class="input" placeholder="default/web" /></FormField>
            <div class="go"><button type="submit" class="btn" :disabled="simBusy" data-testid="sim-run"><Spinner v-if="simBusy" :size="13" />Check</button></div>
          </div>
          <Callout v-if="simErr" status="critical" title="Could not check">{{ simErr }}</Callout>
          <div v-if="simRes" class="simr" data-testid="sim-result">
            <div class="cx-answer" :class="simRes.deliver ? 'ok' : 'bad'">
              <template v-if="simRes.silenced_by.length">An active silence holds it: nobody would be notified until the silence ends. <RouterLink :to="{ name: 'alert-silences' }">Silences</RouterLink></template>
              <template v-else-if="simRes.deliver">Notified now through {{ recipients.filter((x) => x.enabled && !x.quiet).map((x) => x.name).join(', ') }}.</template>
              <template v-else-if="recipients.length">Matched, but nothing goes out now: {{ recipients.every((x) => !x.enabled) ? 'every matching channel is disabled' : 'quiet hours hold it until they end' }}.</template>
              <template v-else>Nobody would be notified. The alert would only show in this console.</template>
            </div>
            <ul class="vl">
              <li v-for="x in simRes.rules" :key="x.rule_id">
                <StatusPill :status="x.matched ? (x.quiet ? 'warn' : 'ok') : 'neutral'" :label="x.matched ? (x.quiet ? 'held (quiet hours)' : 'matches') : 'no match'" size="sm" />
                <strong>{{ x.rule_name }}</strong>
                <span v-if="x.matched" class="muted small">→ {{ x.channels.map((c) => c.name + (c.enabled ? '' : ' (disabled)')).join(', ') }}</span>
                <span v-else class="muted small">{{ x.reason }}</span>
              </li>
              <li v-if="!simRes.rules.length" class="muted small">There are no rules.</li>
            </ul>
          </div>
        </form>
      </section>
    </div>

    <RuleEditor v-model:open="editor.open" :rule="editor.rule" :channels="list(channels.data.value)" @saved="onSaved" />
  </div>
</template>

<style scoped>
.nm { display: inline-flex; align-items: center; gap: 8px; white-space: nowrap; }
.wh { display: inline-flex; align-items: center; gap: 6px; min-width: 0; }
.tt { max-width: 34ch; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.chs { display: inline-flex; flex-wrap: wrap; gap: 4px; }
.tm { color: var(--text-2); }
.simf { display: grid; grid-template-columns: 1.6fr 1fr 1fr 1fr 1fr auto; gap: 10px; align-items: end; }
.go { padding-bottom: 1px; }
.simr { display: grid; grid-template-columns: minmax(0, 1fr); gap: 10px; }
.vl { list-style: none; margin: 0; padding: 0; display: grid; grid-template-columns: minmax(0, 1fr); gap: 6px; }
.vl li { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: var(--fs-sm); }
@media (max-width: 1100px) { .simf { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 560px) { .simf { grid-template-columns: minmax(0, 1fr); } }
</style>
