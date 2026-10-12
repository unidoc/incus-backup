<script setup lang="ts">
// Confirmation before an action. For destructive actions set `danger` and `typeToConfirm` to the name
// of the thing: the button only enables once the user typed it exactly.
// Prefer the promise form for one-off confirmations: `if (await confirm({...})) …` (useConfirm.ts).
import { ref, computed, watch, nextTick } from 'vue'
import Modal from './Modal.vue'
import Icon from '@/components/icons/Icon.vue'

const props = withDefaults(defineProps<{ open: boolean; title: string; message?: string; confirmLabel?: string; cancelLabel?: string; danger?: boolean; typeToConfirm?: string; busy?: boolean }>(), { confirmLabel: 'Confirm', cancelLabel: 'Cancel' })
const emit = defineEmits<{ 'update:open': [boolean]; confirm: []; cancel: [] }>()
const typed = ref('')
const input = ref<HTMLInputElement | null>(null)
const ok = computed(() => !props.typeToConfirm || typed.value === props.typeToConfirm)
watch(() => props.open, async (o) => { if (o) { typed.value = ''; await nextTick(); input.value?.focus() } })
function cancel() { emit('update:open', false); emit('cancel') }
function confirm() { if (ok.value && !props.busy) emit('confirm') }
</script>

<template>
  <Modal :open="open" :title="title" width="460px" @update:open="(v) => !v && cancel()">
    <form class="cd" @submit.prevent="confirm">
      <div v-if="danger" class="warnline"><Icon name="triangle-alert" :size="16" /><span>This cannot be undone.</span></div>
      <p v-if="message" class="msg">{{ message }}</p>
      <slot />
      <label v-if="typeToConfirm" class="type">
        <span>Type <code class="name">{{ typeToConfirm }}</code> to confirm</span>
        <input ref="input" v-model="typed" class="input mono" autocomplete="off" spellcheck="false" data-testid="confirm-input" />
      </label>
    </form>
    <template #footer>
      <button type="button" class="btn" @click="cancel">{{ cancelLabel }}</button>
      <button type="button" class="btn" :class="danger ? 'btn-danger' : 'btn-primary'" :disabled="!ok || busy" data-testid="confirm-button" @click="confirm">{{ confirmLabel }}</button>
    </template>
  </Modal>
</template>

<style scoped>
.cd { display: flex; flex-direction: column; gap: 12px; }
.warnline { display: flex; gap: 8px; align-items: center; color: var(--critical-text); font-weight: var(--fw-medium); }
.msg { color: var(--text-2); }
.type { display: flex; flex-direction: column; gap: 6px; font-size: var(--fs-sm); color: var(--text-2); }
.name { background: var(--surface-3); padding: 1px 5px; border-radius: var(--r-sm); color: var(--text); user-select: all; }
</style>
