<script setup lang="ts">
// Delete a whole object (every snapshot of one backup group) as a server task: typed confirmation, an
// explicit choice for protected snapshots, then the task followed live. Admin only (the server enforces).
import { computed, ref, watch } from 'vue'
import Modal from '@/components/Modal.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import FormField from '@/components/form/FormField.vue'
import TaskPanel from './TaskPanel.vue'
import { opsApi, type TaskDetail } from '@/api/sections/ops'
import { invalidate } from '@/api/cache'
import { toast } from '@/stores/toast'
import { plural } from '@/lib/format'

const props = defineProps<{ open: boolean; repository: string; group: string; name: string; snapshots: number; protected: number }>()
const emit = defineEmits<{ 'update:open': [boolean]; deleted: [] }>()
const typed = ref('')
const force = ref(false)
const busy = ref(false)
const err = ref('')
const taskId = ref('')
watch(() => props.open, (o) => { if (o) { typed.value = ''; force.value = false; err.value = ''; taskId.value = '' } })
const model = computed({ get: () => props.open, set: (x: boolean) => { if (!busy.value) emit('update:open', x) } })
const ready = computed(() => typed.value === props.name && (props.protected === 0 || force.value))

async function submit() {
  busy.value = true
  err.value = ''
  try {
    const r = await opsApi.deleteObject(props.repository, props.group, force.value)
    taskId.value = r.task.id
  } catch (e) {
    err.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
function done(d: TaskDetail) {
  if (d.state !== 'succeeded') return
  toast.success(`${props.name} deleted`, `${plural(props.snapshots, 'snapshot')} removed; the next garbage collection frees their chunks.`)
  invalidate('objects')
  invalidate(`snapshots:${props.repository}`)
  invalidate('overview')
  emit('update:open', false)
  emit('deleted')
}
</script>

<template>
  <Modal v-model:open="model" :title="`Delete ${name}?`" :description="`${repository} · ${group}`" width="580px">
    <div class="form-grid">
      <template v-if="!taskId">
        <Callout status="critical" title="Every snapshot of this object is deleted">
          {{ plural(snapshots, 'snapshot') }} of {{ name }} are removed from {{ repository }} and the object disappears from the lists. There is no undo; copies in other repositories stay. If the host still backs it up, the next backup starts a new history.
        </Callout>
        <template v-if="protected">
          <Callout status="warn" :title="`${plural(protected, 'snapshot')} ${protected === 1 ? 'is' : 'are'} protected`">Protected snapshots are kept on purpose (legal hold, before an upgrade). Delete them too only if that reason is gone.</Callout>
          <SwitchInput v-model="force" label="Also delete the protected snapshots" data-testid="delete-force" />
        </template>
        <FormField v-slot="f" :label="`Type ${name} to confirm`">
          <input :id="f.id" v-model="typed" class="input" autocomplete="off" spellcheck="false" data-testid="delete-confirm" />
        </FormField>
        <Callout v-if="err" status="critical" title="Could not delete the object">{{ err }}</Callout>
      </template>
      <template v-else>
        <TaskPanel :id="taskId" log-height="200px" @done="done" />
      </template>
    </div>
    <template #footer>
      <template v-if="!taskId">
        <button type="button" class="btn" :disabled="busy" @click="model = false">Cancel</button>
        <button type="button" class="btn btn-danger" :disabled="!ready || busy" data-testid="delete-submit" @click="submit"><Spinner v-if="busy" :size="13" />Delete object</button>
      </template>
      <button v-else type="button" class="btn" @click="model = false">Close</button>
    </template>
  </Modal>
</template>
