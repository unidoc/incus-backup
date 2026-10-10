<script setup lang="ts">
// "Back up now" for a host or one object: queues backup_now host jobs (POST /jobs). A job selects by
// project, policy or object, so a whole host is one job per project. The host pulls the jobs over its
// own connection; an offline host picks them up when it returns (queued jobs expire after 24 h).
import { computed, ref, watch } from 'vue'
import Modal from '@/components/Modal.vue'
import Callout from '@/components/Callout.vue'
import FormField from '@/components/form/FormField.vue'
import Icon from '@/components/icons/Icon.vue'
import { infraApi, type Job, type ObjectStat } from '@/api/sections/infra'
import { toast, errorText } from '@/stores/toast'
import { invalidate } from '@/api/cache'
import { kindInfo } from '@/lib/infra'

type ObjLite = Pick<ObjectStat, 'project' | 'name' | 'kind' | 'group_id'>
const props = defineProps<{ open: boolean; repository: string; host: string; objects: ObjLite[]; object?: ObjLite | null; offline?: boolean }>()
const emit = defineEmits<{ 'update:open': [boolean]; queued: [Job[]] }>()

type Mode = 'host' | 'project' | 'object'
const mode = ref<Mode>('host')
const project = ref('')
const objectName = ref('')
const busy = ref(false)
const done = ref(0)
const failures = ref<string[]>([])

const projects = computed(() => [...new Set(props.objects.map((o) => o.project))].sort())
const candidates = computed(() => props.objects.filter((o) => o.kind !== 'instance').sort((a, b) => `${a.project}/${a.name}`.localeCompare(`${b.project}/${b.name}`)))
watch(() => props.open, (o) => {
  if (!o) return
  failures.value = []
  done.value = 0
  if (props.object) { mode.value = 'object'; project.value = props.object.project; objectName.value = props.object.name }
  else { mode.value = 'host'; project.value = projects.value[0] ?? ''; objectName.value = '' }
}, { immediate: true })

const requests = computed(() => {
  if (mode.value === 'host') return projects.value.map((p) => ({ project: p }))
  if (mode.value === 'project') return project.value ? [{ project: project.value }] : []
  const o = candidates.value.find((x) => `${x.project}\u0000${x.name}` === objectName.value) ?? (props.object && objectName.value === props.object.name ? props.object : null)
  return o ? [{ project: o.project, object: o.name }] : []
})
const objectOptions = computed(() => candidates.value.map((o) => ({ value: `${o.project}\u0000${o.name}`, label: `${o.project} / ${o.name} (${kindInfo(o.kind, o.name).short})` })))
watch(mode, (m) => { if (m === 'object' && !props.object && candidates.value[0] && !objectName.value) objectName.value = `${candidates.value[0].project}\u0000${candidates.value[0].name}` })

async function submit() {
  busy.value = true
  failures.value = []
  done.value = 0
  const jobs: Job[] = []
  for (const params of requests.value) {
    try {
      jobs.push(await infraApi.createJob({ type: 'backup_now', host: props.host, repository: props.repository, params }))
    } catch (e) {
      failures.value.push(`${params.project}${'object' in params ? ` / ${params.object}` : ''}: ${errorText(e)}`)
    }
    done.value++
  }
  busy.value = false
  invalidate('jobs')
  if (jobs.length) {
    toast.success(jobs.length === 1 ? `Backup queued on ${props.host}` : `${jobs.length} backup jobs queued on ${props.host}`,
      props.offline ? `${props.host} is offline: the jobs run when it is back (they expire after 24 hours).` : 'The host picks them up within a minute. Progress is on the Jobs tab.')
    emit('queued', jobs)
  }
  if (failures.value.length) toast.error(failures.value.join('\n'), failures.value.length === requests.value.length ? 'Could not queue the backup' : 'Some backup jobs could not be queued')
  else emit('update:open', false)
}
</script>

<template>
  <Modal :open="open" :title="object ? `Back up ${object.name} now` : `Back up ${host} now`" description="Runs a backup outside the schedule. Tonight's scheduled run still happens." @update:open="emit('update:open', $event)">
    <div class="form-grid" data-testid="backup-now">
      <fieldset v-if="!object" class="modes">
        <legend class="sr-only">What to back up</legend>
        <label class="mode"><input v-model="mode" type="radio" value="host" /><span><b>Everything on {{ host }}</b><span class="muted small">{{ projects.length }} {{ projects.length === 1 ? 'project' : 'projects' }}, one job per project</span></span></label>
        <label class="mode"><input v-model="mode" type="radio" value="project" :disabled="!projects.length" /><span><b>One project</b></span></label>
        <label class="mode"><input v-model="mode" type="radio" value="object" :disabled="!candidates.length" /><span><b>One object</b></span></label>
      </fieldset>
      <FormField v-if="mode === 'project' && !object" label="Project" v-slot="f">
        <select :id="f.id" v-model="project" class="select"><option v-for="p in projects" :key="p" :value="p">{{ p }}</option></select>
      </FormField>
      <FormField v-if="mode === 'object' && !object" label="Object" v-slot="f">
        <select :id="f.id" v-model="objectName" class="select"><option v-for="o in objectOptions" :key="o.value" :value="o.value">{{ o.label }}</option></select>
      </FormField>
      <p v-if="object" class="muted">Project <b>{{ object.project }}</b> on host <b>{{ host }}</b>.</p>
      <Callout v-if="offline" status="warn" title="The host is offline">The job waits in the queue until {{ host }} connects again, at most 24 hours.</Callout>
      <Callout v-if="!projects.length && !object" status="info" title="No objects known yet">The host has not reported any object. Its first scheduled run will create them.</Callout>
      <Callout v-if="failures.length" status="critical" title="Not every job was queued"><ul class="fails"><li v-for="f in failures" :key="f">{{ f }}</li></ul></Callout>
    </div>
    <template #footer>
      <span v-if="busy" class="muted small num">Queued {{ done }} of {{ requests.length }}</span>
      <button type="button" class="btn" @click="emit('update:open', false)">Cancel</button>
      <button type="button" class="btn btn-primary" :disabled="busy || !requests.length" data-testid="backup-now-submit" @click="submit"><Icon name="play" :size="14" />{{ requests.length > 1 ? `Queue ${requests.length} jobs` : 'Queue backup' }}</button>
    </template>
  </Modal>
</template>

<style scoped>
.modes { border: 0; margin: 0; padding: 0; display: grid; gap: 6px; }
.mode { display: flex; gap: 10px; align-items: flex-start; padding: 10px 12px; border: 1px solid var(--border); border-radius: var(--r); cursor: pointer; }
.mode:has(input:checked) { border-color: var(--accent); background: var(--accent-soft); }
.mode > span { display: flex; flex-direction: column; gap: 2px; }
.mode input { margin-top: 3px; accent-color: var(--accent); }
.fails { margin: 0; padding-left: 18px; }
</style>
