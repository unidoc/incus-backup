<script setup lang="ts">
// Acknowledge an alert with a comment ("I am on it"): reminders stop; escalation and resolution are
// still announced. The acknowledgement ends with the episode.
import { ref, watch } from 'vue'
import Modal from '@/components/Modal.vue'
import FormField from '@/components/form/FormField.vue'
import Spinner from '@/components/Spinner.vue'
import { cfg, type Alert } from '@/api/sections/config'
import { toast, errorText } from '@/stores/toast'

const props = defineProps<{ alert: Alert | null }>()
const emit = defineEmits<{ close: []; done: [Alert] }>()
const comment = ref('')
const busy = ref(false)
const err = ref('')
watch(() => props.alert, () => { comment.value = ''; err.value = '' })

async function submit() {
  if (!props.alert) return
  busy.value = true
  err.value = ''
  try {
    const a = await cfg.alerts.ack(props.alert.id, comment.value.trim())
    toast.success('Alert acknowledged', 'Reminders stop until it resolves. Escalations still go out.')
    emit('done', a)
    emit('close')
  } catch (e) {
    err.value = errorText(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Modal :open="!!alert" title="Acknowledge the alert" :description="alert?.title" width="480px" @update:open="(v) => !v && emit('close')">
    <form id="ack-form" class="cx-form" data-testid="ack-form" @submit.prevent="submit">
      <p class="cx-help">Acknowledging says “I am on it”. Reminders stop for this episode; an escalation to critical and the resolution are still sent. Everyone sees your name and comment.</p>
      <FormField v-slot="f" label="Comment" optional hint="What you are doing, a ticket number">
        <textarea :id="f.id" v-model="comment" class="textarea" rows="3" maxlength="500" placeholder="Looking at the host, disk full" data-testid="ack-comment" />
      </FormField>
      <p v-if="err" class="cx-err" role="alert">{{ err }}</p>
    </form>
    <template #footer>
      <button type="button" class="btn btn-ghost" @click="emit('close')">Cancel</button>
      <button type="submit" form="ack-form" class="btn btn-primary" :disabled="busy" data-testid="ack-submit"><Spinner v-if="busy" :size="13" />Acknowledge</button>
    </template>
  </Modal>
</template>
