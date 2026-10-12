<script setup lang="ts">
// Maintenance schedules of this repository (GET/PUT /maintenance): what runs when, in plain English,
// with the next runs computed by the server and whether a job is overdue. GC grace and the default
// re-verification age live here too. Only changed fields are sent; gc_grace is sent as gc_grace_hours
// (GET returns gc_grace in NANOSECONDS, which PUT refuses).
import { computed, reactive, ref, watch } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import FormField from '@/components/form/FormField.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import Skeleton from '@/components/Skeleton.vue'
import ScheduleField from '@/components/storage/ScheduleField.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import { useResource, invalidate } from '@/api/cache'
import type { RepoApi } from '@/api/sections/storage'
import { hasRole } from '@/stores/session'
import { toast, errorText } from '@/stores/toast'
import { list } from '@/api/endpoints'
import { lastRun } from '@/lib/storage'
import type { MaintenanceUpdate, MaintenanceView, RepoView } from '@/api/types'

const props = defineProps<{ repo: RepoView; api: RepoApi }>()
const m = useResource(() => `maintenance:${props.repo.name}`, (s) => props.api.maintenance(s))
const runs = useResource(() => `runs:${props.repo.name}:100`, (s) => props.api.runs(100, s), { refreshOn: ['run'] })
const admin = computed(() => hasRole('admin'))

type Key = 'capacity' | 'reconcile' | 'prune' | 'gc' | 'verify' | 'scrub'
const JOBS: Array<{ key: Key; title: string; what: string; kinds: string[] }> = [
  { key: 'verify', title: 'Verify', what: 'Checks new snapshots, and re-checks those older than their policy\'s re-verify age, without any key (chunk trailers, indexes, manifests). Corrupt chunks are quarantined.', kinds: ['verify'] },
  { key: 'scrub', title: 'Scrub', what: 'Full verification: every snapshot and every chunk object, each read once. Slow on large repositories; monthly is typical.', kinds: ['scrub'] },
  { key: 'prune', title: 'Retention (prune)', what: 'Applies each object\'s retention policy and removes the snapshots it no longer keeps. Protected snapshots and the newest one always stay.', kinds: ['retention'] },
  { key: 'gc', title: 'Garbage collection', what: 'Deletes chunks no snapshot references any more, once they are older than the grace period. Frees the space retention released.', kinds: ['gc'] },
  { key: 'reconcile', title: 'Reconcile', what: 'Re-reads the datastore and repairs the database records (snapshots, protection, notes, verification state).', kinds: ['reconcile'] },
  { key: 'capacity', title: 'Capacity sample', what: 'Measures used and free space for the capacity chart and the fill forecast. Lists the whole datastore.', kinds: [] },
]

const form = reactive<Record<Key, string> & { grace: number; reverify: number }>({ capacity: '', reconcile: '', prune: '', gc: '', verify: '', scrub: '', grace: 24, reverify: 30 })
const valid = reactive<Record<Key, boolean>>({ capacity: true, reconcile: true, prune: true, gc: true, verify: true, scrub: true })
function load(v: MaintenanceView | undefined) {
  if (!v) return
  for (const j of JOBS) form[j.key] = v[j.key]
  form.grace = v.gc_grace_hours
  form.reverify = v.default_reverify_days
}
watch(() => m.data.value, load, { immediate: true })

const graceError = computed(() => (!Number.isInteger(form.grace) ? 'Whole hours only.' : form.grace < 12 ? 'At least 12 hours: the grace protects uploads whose session was lost.' : ''))
const reverifyError = computed(() => (!Number.isInteger(form.reverify) || form.reverify < 0 ? 'A whole number of days, 0 or more.' : ''))
const diff = computed<MaintenanceUpdate>(() => {
  const v = m.data.value
  if (!v) return {}
  const d: MaintenanceUpdate = {}
  for (const j of JOBS) if (form[j.key].trim() !== v[j.key]) d[j.key] = form[j.key].trim()
  if (form.grace !== v.gc_grace_hours) d.gc_grace_hours = form.grace
  if (form.reverify !== v.default_reverify_days) d.default_reverify_days = form.reverify
  return d
})
const dirty = computed(() => Object.keys(diff.value).length > 0)
const ok = computed(() => dirty.value && Object.values(valid).every(Boolean) && !graceError.value && !reverifyError.value)
const saving = ref(false)
const error = ref('')
async function save() {
  if (!ok.value) return
  saving.value = true
  error.value = ''
  try {
    await props.api.putMaintenance(diff.value)
    toast.success('Schedules saved', `${Object.keys(diff.value).length} ${Object.keys(diff.value).length === 1 ? 'setting' : 'settings'} changed on ${props.repo.name}.`)
    invalidate(`maintenance:${props.repo.name}`)
    await m.refresh()
  } catch (e) { error.value = errorText(e) } finally { saving.value = false }
}
const lastOf = (kinds: string[]) => (kinds.length ? lastRun(list(runs.data.value), kinds) : undefined)
</script>

<template>
  <section class="panel">
    <div class="panel-head"><h2>Maintenance schedules</h2><span class="sub">run by this server, on its own clock</span></div>
    <div class="panel-body">
      <Skeleton v-if="m.loading.value && !m.data.value" :lines="6" height="16px" />
      <Callout v-else-if="m.error.value && !m.data.value" status="critical" title="Could not load the schedules" :retry="m.refresh">{{ m.error.value.message }}</Callout>
      <form v-else class="form-grid" data-testid="schedules" @submit.prevent="save">
        <div v-for="j in JOBS" :key="j.key" class="job" :data-job="j.key">
          <div class="jl">
            <p class="jt">{{ j.title }}</p>
            <p class="jw">{{ j.what }}</p>
            <p v-if="j.kinds.length" class="jlast small">Last run:
              <template v-if="lastOf(j.kinds)"><RelativeTime :time="lastOf(j.kinds)!.started" />, {{ lastOf(j.kinds)!.status || 'running' }}</template>
              <span v-else class="muted">never</span></p>
          </div>
          <ScheduleField v-model="form[j.key]" :disabled="!admin" clock="server" :count="4" :after="lastOf(j.kinds)?.started ?? null" @valid="(v) => (valid[j.key] = v)" />
        </div>
        <div class="job">
          <div class="jl">
            <p class="jt">GC grace period</p>
            <p class="jw">GC only deletes unreferenced chunks older than this. Running backups are safe regardless (their chunks are pinned); the grace covers uploads whose session was lost. Minimum 12 hours.</p>
          </div>
          <FormField v-slot="f" label="Hours" :error="graceError">
            <input :id="f.id" v-model.number="form.grace" type="number" min="12" step="1" class="input num short" :disabled="!admin" :aria-invalid="f.invalid" data-testid="gc-grace" />
          </FormField>
        </div>
        <div class="job">
          <div class="jl">
            <p class="jt">Default re-verify age</p>
            <p class="jw">Snapshots whose policy sets no re-verify age are checked again after this many days by the daily verify. 0 = verify each snapshot once.</p>
          </div>
          <FormField v-slot="f" label="Days" :error="reverifyError">
            <input :id="f.id" v-model.number="form.reverify" type="number" min="0" step="1" class="input num short" :disabled="!admin" :aria-invalid="f.invalid" />
          </FormField>
        </div>
        <Callout v-if="error" status="critical" title="Could not save the schedules">{{ error }}</Callout>
        <div v-if="admin" class="form-actions sticky">
          <button type="submit" class="btn btn-primary" :disabled="!ok || saving" data-testid="schedules-save"><Spinner v-if="saving" :size="13" />Save schedules</button>
          <button type="button" class="btn" :disabled="!dirty || saving" @click="load(m.data.value)">Discard changes</button>
          <span v-if="dirty" class="muted small">{{ Object.keys(diff).length }} unsaved {{ Object.keys(diff).length === 1 ? 'change' : 'changes' }}</span>
        </div>
        <p v-else class="muted small"><Icon name="lock" :size="13" /> Changing schedules needs the admin role.</p>
      </form>
    </div>
  </section>
</template>

<style scoped>
.job { display: grid; grid-template-columns: minmax(220px, 1fr) minmax(0, 1.3fr); gap: var(--s-4); padding: 12px 0; border-bottom: 1px solid var(--border); }
.job:first-child { padding-top: 0; }
.jt { font-weight: var(--fw-semibold); }
.jw { color: var(--text-muted); font-size: var(--fs-sm); margin-top: 2px; }
.jlast { margin-top: 6px; color: var(--text-2); }
.short { max-width: 120px; }
.sticky { position: sticky; bottom: 0; background: var(--surface); padding: 10px 0 2px; }
@media (max-width: 800px) { .job { grid-template-columns: 1fr; } }
</style>
