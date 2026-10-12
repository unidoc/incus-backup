<script setup lang="ts">
// "Run restore test": queues a restore_test host job (POST /jobs). The host proves the newest snapshot
// restores with its own key; nothing touches the original object. deep = decrypt and check every
// chunk; extract = restore a filesystem into scratch space and compare every file; full = restore into
// a scratch Incus volume and compare its hash. (POST /restore-tests/run cannot be used: it drops the
// project the job needs.)
import { computed, ref, watch } from 'vue'
import Modal from '@/components/Modal.vue'
import Callout from '@/components/Callout.vue'
import FormField from '@/components/form/FormField.vue'
import Icon from '@/components/icons/Icon.vue'
import { infraApi, type Job, type ObjectStat } from '@/api/sections/infra'
import { toast } from '@/stores/toast'
import { invalidate } from '@/api/cache'
import { kindInfo } from '@/lib/infra'
import { formatDateTime } from '@/lib/format'

const props = defineProps<{ open: boolean; repository: string; host: string; objects: ObjectStat[]; object?: ObjectStat | null; offline?: boolean }>()
const emit = defineEmits<{ 'update:open': [boolean]; queued: [Job] }>()
const candidates = computed(() => props.objects.filter((o) => o.kind !== 'instance' && o.snapshots > 0))
const sel = ref('')
const mode = ref<'deep' | 'extract' | 'full'>('deep')
const busy = ref(false)
const err = ref('')
const chosen = computed(() => props.object ?? candidates.value.find((o) => o.group_id === sel.value) ?? null)
watch(() => props.open, (o) => { if (o) { err.value = ''; sel.value = props.object?.group_id ?? candidates.value[0]?.group_id ?? ''; mode.value = 'deep' } }, { immediate: true })
watch(chosen, (c) => { if (c && c.kind !== 'filesystem' && mode.value === 'extract') mode.value = 'deep' })

async function submit() {
  const o = chosen.value
  if (!o) return
  busy.value = true
  err.value = ''
  try {
    const j = await infraApi.createJob({ type: 'restore_test', host: props.host, repository: props.repository, params: { project: o.project, object: o.name, mode: mode.value } })
    invalidate('jobs')
    toast.success(`Restore test queued on ${props.host}`, `${o.project} / ${o.name}, ${mode.value} test of the newest snapshot.`)
    emit('queued', j)
    emit('update:open', false)
  } catch (e) {
    err.value = e instanceof Error ? e.message : String(e)
    toast.error(e, 'Could not queue the restore test')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Modal :open="open" title="Run a restore test" :description="`The host ${host} restores the newest snapshot with its own key and checks it. The original is not touched.`" @update:open="emit('update:open', $event)">
    <div class="form-grid" data-testid="restore-test">
      <FormField v-if="!object" label="Object" v-slot="f">
        <select :id="f.id" v-model="sel" class="select" :disabled="!candidates.length">
          <option v-for="o in candidates" :key="o.group_id" :value="o.group_id">{{ o.project }} / {{ o.name }} ({{ kindInfo(o.kind, o.name).short }})</option>
        </select>
      </FormField>
      <p v-if="chosen?.latest" class="muted small">Newest snapshot: <span class="mono">{{ chosen.latest.id }}</span>, {{ formatDateTime(chosen.latest.created) }}</p>
      <fieldset class="modes">
        <legend class="lg">Method</legend>
        <label class="mode"><input v-model="mode" type="radio" value="deep" /><span><b>Deep</b><span class="muted small">Download and decrypt every chunk and recompute its id. Fast, no scratch space.</span></span></label>
        <label class="mode" :class="{ dis: chosen?.kind !== 'filesystem' }"><input v-model="mode" type="radio" value="extract" :disabled="chosen?.kind !== 'filesystem'" /><span><b>Extract</b><span class="muted small">Filesystems only: restore every file into scratch space and compare its checksum.</span></span></label>
        <label class="mode"><input v-model="mode" type="radio" value="full" /><span><b>Full</b><span class="muted small">Restore into a scratch Incus volume, read it back and compare. Slowest; needs pool space.</span></span></label>
      </fieldset>
      <Callout v-if="!candidates.length && !object" status="info" title="Nothing to test yet">The host has no object with a snapshot.</Callout>
      <Callout v-if="offline" status="warn" title="The host is offline">The test waits in the queue until {{ host }} connects again, at most 24 hours.</Callout>
      <Callout v-if="err" status="critical" title="Could not queue the restore test">{{ err }}</Callout>
    </div>
    <template #footer>
      <button type="button" class="btn" @click="emit('update:open', false)">Cancel</button>
      <button type="button" class="btn btn-primary" :disabled="busy || !chosen" data-testid="restore-test-submit" @click="submit"><Icon name="shield-check" :size="14" />Queue test</button>
    </template>
  </Modal>
</template>

<style scoped>
.modes { border: 0; margin: 0; padding: 0; display: grid; gap: 6px; }
.lg { font-weight: var(--fw-medium); margin-bottom: 6px; padding: 0; }
.mode { display: flex; gap: 10px; align-items: flex-start; padding: 10px 12px; border: 1px solid var(--border); border-radius: var(--r); cursor: pointer; }
.mode.dis { opacity: .55; cursor: not-allowed; }
.mode:has(input:checked) { border-color: var(--accent); background: var(--accent-soft); }
.mode > span { display: flex; flex-direction: column; gap: 2px; }
.mode input { margin-top: 3px; accent-color: var(--accent); }
.mono { font-family: var(--font-mono); }
</style>
