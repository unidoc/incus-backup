<script setup lang="ts">
// Maintenance mode: online / read-only / offline, enforced below the object model
// (datastore.ModeBackend). The consequences are spelled out per mode; going offline needs the name typed.
import { computed, ref, watch } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import FormField from '@/components/form/FormField.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import StatusPill from '@/components/StatusPill.vue'
import { invalidate } from '@/api/cache'
import type { RepoApi } from '@/api/sections/storage'
import { loadRepos } from '@/stores/repos'
import { hasRole } from '@/stores/session'
import { toast, errorText } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import type { RepoMode, RepoView } from '@/api/types'

const props = defineProps<{ repo: RepoView; api: RepoApi }>()
const admin = computed(() => hasRole('admin'))
const want = ref<RepoMode>(props.repo.mode)
const reason = ref(props.repo.mode_reason ?? '')
watch(() => props.repo.mode, (m) => { want.value = m; reason.value = props.repo.mode_reason ?? '' })
const busy = ref(false)
const error = ref('')

const MODES: Array<{ id: RepoMode; title: string; icon: 'circle-check' | 'lock' | 'octagon-x'; works: string[]; stops: string[] }> = [
  { id: 'online', title: 'Online', icon: 'circle-check', works: ['Backups, restores, browsing, sync in both directions', 'Scheduled verify, retention, GC, scrub'], stops: [] },
  { id: 'read-only', title: 'Read-only', icon: 'lock', works: ['Restores, file browsing and downloads', 'Sync from this repository to another server'],
    stops: ['New backups: hosts get "temporarily unavailable" and retry later; an upload in progress fails and is retried', 'Retention, GC, verification (it records results) and sync into this repository'] },
  { id: 'offline', title: 'Offline', icon: 'octagon-x', works: ['Nothing touches the datastore: safe to unmount the disk or rotate S3 access'],
    stops: ['Backups, restores, browsing and sync', 'All maintenance, including scheduled runs', 'Hosts retry until the repository is online again'] },
]
const changed = computed(() => want.value !== props.repo.mode || (want.value !== 'online' && reason.value.trim() !== (props.repo.mode_reason ?? '')))

async function apply() {
  if (!changed.value) return
  const m = want.value
  const ok = m === 'online'
    ? await confirm({ title: `Put ${props.repo.name} back online?`, message: 'Hosts resume their backups at their next scheduled or retried run; scheduled maintenance resumes.', confirmLabel: 'Set online' })
    : m === 'read-only'
      ? await confirm({ title: `Make ${props.repo.name} read-only?`, message: 'New backups and every maintenance write are refused until you set it online again. Restores keep working.', confirmLabel: 'Set read-only' })
      : await confirm({ title: `Take ${props.repo.name} offline?`, danger: true, typeToConfirm: props.repo.name, confirmLabel: 'Take offline',
        message: 'Every access to the datastore is refused: backups, restores, sync and maintenance stop until you set it online again.' })
  if (!ok) return
  busy.value = true
  error.value = ''
  try {
    await props.api.setMode({ mode: m, reason: m === 'online' ? undefined : reason.value.trim() || undefined })
    toast.success(`${props.repo.name} is ${m}`, m === 'online' ? 'Backups and maintenance resume.' : reason.value.trim() ? `Reason: ${reason.value.trim()}` : undefined)
    invalidate('repositories')
    invalidate('overview')
    await loadRepos()
  } catch (e) {
    error.value = errorText(e)
  } finally { busy.value = false }
}
</script>

<template>
  <section class="panel">
    <div class="panel-head"><h2>Maintenance mode</h2><span class="sub">Currently</span><StatusPill :state="repo.mode" size="sm" />
      <span v-if="repo.mode_reason" class="muted small truncate">“{{ repo.mode_reason }}”</span></div>
    <div class="panel-body stack">
      <p class="muted">Use read-only for a storage migration or a snapshot of the datastore, offline to take the storage away. The mode is enforced below every code path that touches the datastore and survives restarts.</p>
      <div class="modes" role="radiogroup" aria-label="Maintenance mode">
        <label v-for="m in MODES" :key="m.id" class="mode" :class="[`m-${m.id}`, { on: want === m.id, cur: repo.mode === m.id, dis: !admin }]">
          <input v-model="want" type="radio" name="mode" :value="m.id" :disabled="!admin" :data-testid="`mode-${m.id}`" />
          <span class="mh"><Icon :name="m.icon" :size="16" /><b>{{ m.title }}</b><span v-if="repo.mode === m.id" class="now">current</span></span>
          <ul class="works"><li v-for="x in m.works" :key="x"><Icon name="check" :size="12" />{{ x }}</li></ul>
          <ul v-if="m.stops.length" class="stops"><li v-for="x in m.stops" :key="x"><Icon name="x" :size="12" />{{ x }}</li></ul>
        </label>
      </div>
      <FormField v-if="want !== 'online'" v-slot="f" label="Reason" optional hint="Shown on the dashboard, in the repository selector and to operators: say what is going on and until when.">
        <input :id="f.id" v-model="reason" class="input" maxlength="200" placeholder="Disk replacement, back by 14:00" :disabled="!admin" data-testid="mode-reason" />
      </FormField>
      <Callout v-if="error" status="critical" title="Could not change the mode">{{ error }}</Callout>
      <div class="form-actions">
        <button v-if="admin" type="button" class="btn" :class="want === 'offline' ? 'btn-danger' : 'btn-primary'" :disabled="!changed || busy" data-testid="mode-apply" @click="apply">
          <Spinner v-if="busy" :size="13" />{{ want === 'online' ? 'Set online' : want === 'read-only' ? 'Set read-only' : 'Take offline' }}</button>
        <span v-else class="muted small"><Icon name="lock" :size="13" /> Changing the mode needs the admin role.</span>
      </div>
    </div>
  </section>
</template>

<style scoped>
.modes { display: grid; grid-template-columns: repeat(3, 1fr); gap: var(--s-3); }
.mode { display: flex; flex-direction: column; gap: 8px; padding: 12px 14px; border: 1px solid var(--border-strong); border-radius: var(--r-lg); cursor: pointer; background: var(--surface); position: relative; }
.mode input { position: absolute; opacity: 0; pointer-events: none; }
.mode:focus-within { box-shadow: 0 0 0 3px color-mix(in srgb, var(--focus) 30%, transparent); }
.mode.dis { cursor: default; }
.mode.on { border-color: var(--accent); box-shadow: 0 0 0 1px var(--accent) inset; }
.m-read-only.on { border-color: var(--warn); box-shadow: 0 0 0 1px var(--warn) inset; background: color-mix(in srgb, var(--warn-bg) 45%, var(--surface)); }
.m-offline.on { border-color: var(--critical); box-shadow: 0 0 0 1px var(--critical) inset; background: color-mix(in srgb, var(--critical-bg) 45%, var(--surface)); }
.mh { display: flex; align-items: center; gap: 8px; }
.m-online .mh .icon { color: var(--ok); } .m-read-only .mh .icon { color: var(--warn); } .m-offline .mh .icon { color: var(--critical); }
.now { margin-left: auto; font-size: var(--fs-xs); padding: 1px 6px; border-radius: var(--r-pill); background: var(--surface-3); color: var(--text-2); }
ul { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 4px; font-size: var(--fs-sm); }
li { display: flex; gap: 6px; align-items: flex-start; color: var(--text-2); }
li .icon { margin-top: 3px; flex: none; }
.works .icon { color: var(--ok); } .stops .icon { color: var(--critical); }
.truncate { max-width: 40ch; }
@media (max-width: 900px) { .modes { grid-template-columns: 1fr; } }
</style>
