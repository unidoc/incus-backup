<script setup lang="ts">
// Create or edit a policy: schedule (with the next runs), targets, retention with a preview of what it
// keeps, the time zone retention periods are cut in, verification and restore-test schedules. For an
// existing policy also: the objects it governs, its assignments, and deleting it with the impact spelled out.
// The server validates again; its 400 text is shown next to the field it names.
import { computed, reactive, ref, watch } from 'vue'
import { RouterLink, useRouter, onBeforeRouteLeave } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import Tabs from '@/components/Tabs.vue'
import FormField from '@/components/form/FormField.vue'
import TagInput from '@/components/form/TagInput.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import Skeleton from '@/components/Skeleton.vue'
import EmptyState from '@/components/EmptyState.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import ScheduleField from '@/components/storage/ScheduleField.vue'
import TimezoneSelect from '@/components/storage/TimezoneSelect.vue'
import RetentionPreview from '@/components/storage/RetentionPreview.vue'
import AssignmentMatrix from '@/components/storage/AssignmentMatrix.vue'
import { api, list } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import { storageApi, type EffectivePolicy } from '@/api/sections/storage'
import { repos, repoNone, selectRepo } from '@/stores/repos'
import { hasRole } from '@/stores/session'
import { toast, errorText } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import { rulesOf, RULE_ORDER, type RuleName } from '@/lib/retention'
import { deleteImpact, sourceLabel } from '@/lib/policy'
import { plural } from '@/lib/format'
import type { ColumnDef } from '@/lib/table'
import type { Policy } from '@/api/types'

const props = defineProps<{ name?: string }>()
const router = useRouter()
const isNew = computed(() => !props.name)
const canEdit = computed(() => hasRole('operator'))
const enabled = () => !!repos.current && !repoNone.value
const pols = useResource(() => `policies:${repos.current}`, (s) => api.policies.list(s), { enabled })
const asg = useResource(() => `assignments:${repos.current}`, (s) => api.assignments.list(s), { enabled })
const eff = useResource(() => `effective:${repos.current}`, (s) => storageApi.effectivePolicies(s), { enabled })
const current = computed(() => (props.name ? list(pols.data.value).find((p) => p.name === props.name) : undefined))

const blank = (): Policy => ({ name: '', schedule: '03:00', targets: [], commit_required: 'all', keep_last: 0, keep_hourly: 0, keep_daily: 7, keep_weekly: 4, keep_monthly: 6, keep_yearly: 0, timezone: 'UTC', reverify_days: 0, verify_schedule: 'weekly', restore_test_schedule: 'monthly' })
const f = reactive<Policy>(blank())
const original = ref('')
function load(p: Policy | undefined) {
  if (!p) return
  Object.assign(f, blank(), JSON.parse(JSON.stringify(p)), { targets: p.targets ?? [], timezone: p.timezone || 'UTC', reverify_days: p.reverify_days ?? 0 })
  original.value = JSON.stringify(f)
}
watch(current, (p) => load(p), { immediate: true })
if (isNew.value) original.value = JSON.stringify(f)
const dirty = computed(() => JSON.stringify(f) !== original.value)

const valid = reactive({ schedule: true, verify: true, restore: true })
const nameError = computed(() => (!isNew.value || !f.name ? '' : /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(f.name) ? (list(pols.data.value).some((p) => p.name === f.name) ? 'A policy with this name exists; open it to edit.' : '') : 'Letters, digits, dot, dash and underscore; start with a letter or digit.'))
const keepErrors = computed(() => RULE_ORDER.filter((k) => { const v = f[`keep_${k}` as keyof Policy] as number; return !Number.isInteger(v) || v < 0 }))
const reverifyError = computed(() => (!Number.isInteger(f.reverify_days ?? 0) || (f.reverify_days ?? 0) < 0 ? 'A whole number of days, 0 or more.' : ''))
const serverError = ref('')
const fieldError = (field: string) => {
  const e = serverError.value.toLowerCase()
  const map: Record<string, RegExp> = { schedule: /^schedule:/, verify: /^verify schedule:/, restore: /^restore test schedule:/, timezone: /time zone/, commit: /commit_required/, reverify: /reverify/ }
  return map[field]?.test(e) ? serverError.value : ''
}
const canSave = computed(() => canEdit.value && dirty.value && !!f.name && !nameError.value && valid.schedule && valid.verify && valid.restore && !keepErrors.value.length && !reverifyError.value)

const saving = ref(false)
async function save() {
  if (!canSave.value) return
  saving.value = true
  serverError.value = ''
  try {
    const body: Policy = { ...f, targets: [...(f.targets ?? [])], name: f.name.trim() }
    await api.policies.put(body.name, body)
    original.value = JSON.stringify(f)
    invalidate(`policies:${repos.current}`)
    invalidate(`effective:${repos.current}`)
    toast.success(isNew.value ? `Policy ${body.name} created` : `Policy ${body.name} saved`, 'Hosts pick it up with their next contact.')
    if (isNew.value) void router.replace({ name: 'policy', params: { name: body.name } })
    else await pols.refresh()
  } catch (e) {
    serverError.value = errorText(e)
  } finally { saving.value = false }
}
onBeforeRouteLeave(async () => {
  if (!dirty.value || saving.value || !canEdit.value) return true
  return await confirm({ title: 'Leave without saving?', message: 'Your changes to this policy are lost.', confirmLabel: 'Leave', danger: false })
})

const RULE_HELP: Record<RuleName, string> = {
  last: 'the newest N snapshots, whatever their age',
  hourly: 'the newest snapshot of each of the last N hours that have one',
  daily: 'one per day (the newest of that day) for the last N days with a backup',
  weekly: 'one per ISO week',
  monthly: 'one per calendar month',
  yearly: 'one per calendar year',
}
const rules = computed(() => rulesOf(f))

// ---- existing policy: objects, assignments, delete
const tab = ref('settings')
const governed = computed(() => list(eff.data.value).filter((e) => e.policy === props.name))
const tabs = computed(() => [{ id: 'settings', label: 'Settings' }, { id: 'objects', label: 'Objects', count: governed.value.length }, { id: 'assignments', label: 'Assignments', count: list(asg.data.value).filter((a) => a.policy === props.name).length }])
const objCols: ColumnDef<EffectivePolicy>[] = [
  { key: 'object', label: 'Object', value: (e) => `${e.project}/${e.name}`, required: true },
  { key: 'kind', label: 'Kind' },
  { key: 'host', label: 'Host' },
  { key: 'source', label: 'Why this policy', format: (e) => sourceLabel[e.source] },
  { key: 'declared_policy', label: 'Host declares', hidden: true },
  { key: 'snapshots', label: 'Snapshots', align: 'right' },
]
const impact = computed(() => (props.name ? deleteImpact(props.name, list(eff.data.value), list(asg.data.value)) : null))
const deleting = ref(false)
async function remove() {
  const name = props.name
  const im = impact.value
  if (!name || !im) return
  const fb = Object.entries(im.fallback).map(([p, n]) => `${plural(n, 'object')} → ${p || 'no policy (keeps everything)'}`).join('; ')
  if (!(await confirm({ title: `Delete policy ${name}?`, danger: true, typeToConfirm: name, confirmLabel: 'Delete policy',
    message: im.objects ? `It governs ${plural(im.objects, 'object')} and has ${plural(im.assignments, 'assignment')}, which are deleted with it. Afterwards: ${fb}. Hosts that still declare it in their own configuration stop backing up those objects until you assign another policy.` : `No object is governed by it${im.assignments ? `; its ${plural(im.assignments, 'assignment')} are deleted with it` : ''}.` }))) return
  deleting.value = true
  try {
    await api.policies.remove(name)
    original.value = JSON.stringify(f)
    invalidate(`policies:${repos.current}`)
    invalidate(`assignments:${repos.current}`)
    invalidate(`effective:${repos.current}`)
    toast.success(`Policy ${name} deleted`)
    void router.push({ name: 'policies' })
  } catch (e) { toast.error(e, 'Could not delete the policy') } finally { deleting.value = false }
}
function openObject(e: EffectivePolicy) { void router.push({ name: 'object', params: { group: e.group_id } }) }
</script>

<template>
  <div class="pe">
    <header class="page-head">
      <div>
        <h1>{{ isNew ? 'New policy' : name }}</h1>
        <p class="lede">{{ isNew ? 'When hosts back up, where to, and how long the server keeps what they send.' : `Policy of repository ${repos.current}.` }}</p>
      </div>
      <div class="actions">
        <RouterLink :to="{ name: 'policies' }" class="btn">All policies</RouterLink>
        <button v-if="!isNew && hasRole('admin') && current" type="button" class="btn btn-danger" :disabled="deleting" data-testid="policy-delete" @click="remove"><Icon name="trash-2" />Delete</button>
      </div>
    </header>

    <EmptyState v-if="repoNone" title="No repository yet" icon="database">Policies belong to a repository. Add one first.</EmptyState>
    <div v-else-if="!isNew && pols.loading.value && !pols.data.value" class="panel pad"><Skeleton :lines="8" height="16px" /></div>
    <Callout v-else-if="!isNew && pols.error.value && !pols.data.value" status="critical" title="Could not load the policy" :retry="pols.refresh">{{ pols.error.value.message }}</Callout>
    <EmptyState v-else-if="!isNew && pols.data.value !== undefined && !current" title="No such policy" icon="calendar-clock">
      Repository {{ repos.current }} has no policy called “{{ name }}”. It may exist in another repository: switch it in the top bar.
      <template #actions><RouterLink :to="{ name: 'policies' }" class="btn btn-sm">All policies</RouterLink></template>
    </EmptyState>
    <template v-else>
      <Tabs v-if="!isNew" v-model="tab" :tabs="tabs" label="Policy sections" />

      <form v-if="isNew || tab === 'settings'" class="layout" data-testid="policy-form" @submit.prevent="save">
        <div class="main">
          <section class="panel">
            <div class="panel-head"><h2>Backups</h2><span class="sub">run by each host</span></div>
            <div class="panel-body form-grid">
              <FormField v-if="isNew" v-slot="fl" label="Name" :error="nameError" required hint="Hosts and assignments refer to it by name; it cannot be renamed.">
                <input :id="fl.id" v-model="f.name" class="input short" autocomplete="off" spellcheck="false" placeholder="nightly" :aria-invalid="fl.invalid" data-testid="policy-name" />
              </FormField>
              <FormField v-slot="fl" label="Schedule" :error="fieldError('schedule')">
                <ScheduleField :id="fl.id" v-model="f.schedule" clock="host" :disabled="!canEdit" :allow-manual="true" @valid="(v) => (valid.schedule = v)" />
              </FormField>
              <FormField v-slot="fl" label="Targets" optional hint="Server names as configured on the hosts (incus-backup init). Empty: every server the host knows. Unknown names are ignored by the host.">
                <TagInput :id="fl.id" v-model="f.targets as string[]" placeholder="custody, offsite" />
              </FormField>
              <fieldset v-if="(f.targets?.length ?? 0) > 1" class="radios">
                <legend>A run counts as done when</legend>
                <label><input v-model="f.commit_required" type="radio" value="all" :disabled="!canEdit" /> <b>all</b> targets committed <span class="muted">(a lagging target keeps its base and catches up next run)</span></label>
                <label><input v-model="f.commit_required" type="radio" value="any" :disabled="!canEdit" /> <b>any</b> target committed</label>
                <p v-if="fieldError('commit')" class="err">{{ fieldError('commit') }}</p>
              </fieldset>
            </div>
          </section>

          <section class="panel">
            <div class="panel-head"><h2>Retention</h2><span class="sub">applied by the server; rules add up (PBS semantics)</span></div>
            <div class="panel-body form-grid">
              <div class="keeps">
                <label v-for="k in RULE_ORDER" :key="k" class="keep" :class="{ on: (f[`keep_${k}` as keyof Policy] as number) > 0, bad: keepErrors.includes(k) }">
                  <span class="kl">Keep {{ k }}</span>
                  <input v-model.number="(f as unknown as Record<string, number>)[`keep_${k}`]" type="number" min="0" step="1" class="input num" :disabled="!canEdit" :data-testid="`keep-${k}`" />
                  <span class="kh">{{ RULE_HELP[k] }}</span>
                </label>
              </div>
              <p class="muted small">0 turns a rule off. With no rule at all, every snapshot is kept. The newest snapshot of an object and protected snapshots are always kept.</p>
              <FormField v-slot="fl" label="Time zone of the periods" :error="fieldError('timezone')" hint="Days, weeks (ISO), months and years are cut in this zone. Use the zone where your day starts.">
                <TimezoneSelect :id="fl.id" v-model="f.timezone as string" :disabled="!canEdit" />
              </FormField>
              <RetentionPreview :rules="rules" :tz="f.timezone || 'UTC'" :schedule="f.schedule" />
            </div>
          </section>

          <section class="panel">
            <div class="panel-head"><h2>Verification and restore tests</h2></div>
            <div class="panel-body form-grid">
              <FormField v-slot="fl" label="Verify schedule" :error="fieldError('verify')" hint="Hosts verify with the key (decrypt and recompute chunk ids): proves restorability. The server's own keyless verify is in the repository's schedules.">
                <ScheduleField :id="fl.id" v-model="f.verify_schedule" clock="host" :disabled="!canEdit" :count="3" @valid="(v) => (valid.verify = v)" />
              </FormField>
              <FormField v-slot="fl" label="Re-verify after (days)" :error="reverifyError || fieldError('reverify')" hint="The server's daily verify checks snapshots of this policy again after this many days. 0: use the repository's default.">
                <input :id="fl.id" v-model.number="f.reverify_days" type="number" min="0" step="1" class="input num short" :disabled="!canEdit" />
              </FormField>
              <FormField v-slot="fl" label="Restore-test schedule" :error="fieldError('restore')" hint="A host restores a snapshot into a scratch instance and compares it.">
                <ScheduleField :id="fl.id" v-model="f.restore_test_schedule" clock="host" :disabled="!canEdit" :count="3" @valid="(v) => (valid.restore = v)" />
              </FormField>
            </div>
          </section>
        </div>

        <aside class="side">
          <div class="panel sticky">
            <div class="panel-body form-grid">
              <Callout v-if="serverError" status="critical" title="The server refused the policy" data-testid="policy-error">{{ serverError }}</Callout>
              <p v-if="!canEdit" class="muted small"><Icon name="lock" :size="13" /> Editing policies needs the operator role.</p>
              <p v-else-if="!dirty" class="muted small">{{ isNew ? 'Fill in the policy, then create it.' : 'No unsaved changes.' }}</p>
              <p v-else class="small">Unsaved changes.</p>
              <button v-if="canEdit" type="submit" class="btn btn-primary btn-block" :disabled="!canSave || saving" data-testid="policy-save"><Spinner v-if="saving" :size="13" />{{ isNew ? 'Create policy' : 'Save policy' }}</button>
              <button v-if="canEdit && dirty && !isNew" type="button" class="btn btn-block" @click="load(current)">Discard changes</button>
              <template v-if="!isNew">
                <hr class="divider" />
                <p class="small"><b class="num">{{ governed.length }}</b> objects governed · <b class="num">{{ list(asg.data.value).filter((a) => a.policy === name).length }}</b> assignments</p>
                <p class="muted small">Changes to retention apply at the next retention run of the repository; preview it on the repository's Tasks tab.</p>
              </template>
            </div>
          </div>
        </aside>
      </form>

      <section v-else-if="tab === 'objects'" class="panel">
        <div class="panel-head"><h2>Objects governed by {{ name }}</h2><span class="sub">the server's own resolution</span></div>
        <DataTable :rows="eff.data.value === undefined ? undefined : governed" :columns="objCols" row-key="group_id" storage-key="policy-objects" :csv-name="`policy-${name}-objects`" :loading="eff.loading.value" :error="eff.error.value" :retry="eff.refresh" dense row-clickable @row-click="openObject">
          <template #cell-source="{ row }"><StatusPill :status="row.source === 'pinned' ? 'neutral' : 'info'" :label="sourceLabel[row.source]" size="sm" variant="plain" /></template>
          <template #empty><EmptyState title="No object uses this policy" icon="box" compact>Assign it to a host, a project or an object on the Assignments tab.</EmptyState></template>
        </DataTable>
      </section>

      <section v-else class="panel pad">
        <AssignmentMatrix :policies="list(pols.data.value).map((p) => p.name)" :focus="name" />
      </section>
    </template>
  </div>
</template>

<style scoped>
.pad { padding: var(--s-4); }
.layout { display: grid; grid-template-columns: minmax(0, 1fr) 280px; gap: var(--s-4); align-items: start; }
.main { display: flex; flex-direction: column; gap: var(--s-4); min-width: 0; }
.sticky { position: sticky; top: 72px; }
.short { max-width: 140px; }
.radios { border: 0; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 4px; font-size: var(--fs-sm); }
.radios legend { font-weight: var(--fw-medium); color: var(--text-2); margin-bottom: 4px; }
.radios input { accent-color: var(--accent); }
.keeps { display: grid; grid-template-columns: repeat(3, 1fr); gap: 8px; }
.keep { display: grid; grid-template-columns: 1fr 80px; grid-template-rows: auto auto; gap: 2px 8px; padding: 8px 10px; border: 1px solid var(--border); border-radius: var(--r); background: var(--surface); align-items: center; }
.keep.on { border-color: color-mix(in srgb, var(--accent) 45%, var(--border)); background: var(--accent-soft); }
.keep.bad { border-color: var(--critical); }
.kl { font-weight: var(--fw-medium); }
.keep .input { height: var(--control-h-sm); }
.kh { grid-column: 1 / -1; font-size: var(--fs-xs); color: var(--text-muted); }
.err { color: var(--critical-text); font-size: var(--fs-sm); }
.panel > :deep(.dt) { border: 0; border-radius: 0 0 var(--r-lg) var(--r-lg); }
@media (max-width: 1100px) { .layout { grid-template-columns: 1fr; } .sticky { position: static; } }
@media (max-width: 700px) { .keeps { grid-template-columns: 1fr 1fr; } }
</style>
