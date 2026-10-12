<script setup lang="ts">
// Modal dialog on the native <dialog> (focus is trapped and restored by the browser, Esc closes).
// <Modal v-model:open="show" title="Rename host"> body <template #footer>buttons</template></Modal>
// `drawer` slides in from the right instead (detail panels, long forms).
import { ref, watch, onMounted, onBeforeUnmount, useId } from 'vue'
import Icon from '@/components/icons/Icon.vue'

const props = withDefaults(defineProps<{ open: boolean; title: string; description?: string; width?: string; drawer?: boolean; dismissible?: boolean }>(), { width: '520px', dismissible: true })
const emit = defineEmits<{ 'update:open': [boolean]; close: [] }>()
const dlg = ref<HTMLDialogElement | null>(null)
const titleId = useId()
const descId = useId()

function sync() {
  const d = dlg.value
  if (!d) return
  if (props.open && !d.open) d.showModal()
  if (!props.open && d.open) d.close()
}
watch(() => props.open, sync)
onMounted(sync)
onBeforeUnmount(() => dlg.value?.open && dlg.value.close())

function close() {
  emit('update:open', false)
  emit('close')
}
function onCancel(e: Event) {
  e.preventDefault()
  if (props.dismissible) close()
}
function onBackdrop(e: MouseEvent) {
  if (props.dismissible && e.target === dlg.value) close()
}
</script>

<template>
  <dialog ref="dlg" class="modal" :class="{ drawer }" :style="{ '--w': width }" :aria-labelledby="titleId" :aria-describedby="description ? descId : undefined" @cancel="onCancel" @mousedown="onBackdrop">
    <div v-if="open" class="frame">
      <header class="head">
        <div class="ht">
          <h2 :id="titleId">{{ title }}</h2>
          <p v-if="description" :id="descId" class="desc">{{ description }}</p>
        </div>
        <button v-if="dismissible" type="button" class="btn btn-ghost btn-icon btn-sm" aria-label="Close" @click="close"><Icon name="x" /></button>
      </header>
      <div class="body"><slot /></div>
      <footer v-if="$slots.footer" class="foot"><slot name="footer" /></footer>
    </div>
  </dialog>
</template>

<style scoped>
.modal { padding: 0; border: 1px solid var(--border-strong); border-radius: var(--r-lg); background: var(--surface); color: var(--text); width: min(var(--w), calc(100vw - 24px)); max-height: calc(100dvh - 48px); box-shadow: var(--shadow-pop); }
.modal::backdrop { background: var(--scrim); }
.modal[open] { animation: pop var(--t) var(--ease); }
.frame { display: flex; flex-direction: column; max-height: calc(100dvh - 48px); }
.head { display: flex; align-items: flex-start; gap: 12px; padding: 14px 16px 12px; border-bottom: 1px solid var(--border); }
.ht { flex: 1; min-width: 0; }
.head h2 { font-size: var(--fs-lg); }
.desc { color: var(--text-muted); font-size: var(--fs-sm); margin-top: 3px; }
.body { padding: 16px; overflow: auto; }
.foot { display: flex; justify-content: flex-end; gap: 8px; padding: 12px 16px; border-top: 1px solid var(--border); background: var(--surface-2); border-radius: 0 0 var(--r-lg) var(--r-lg); flex-wrap: wrap; }
.drawer { margin: 0 0 0 auto; height: 100dvh; max-height: 100dvh; border-radius: 0; border-width: 0 0 0 1px; width: min(var(--w), 100vw); }
.drawer .frame { height: 100%; max-height: 100dvh; }
.drawer .body { flex: 1; }
.drawer .foot { border-radius: 0; }
.drawer[open] { animation: slide var(--t) var(--ease); }
@keyframes pop { from { opacity: 0; transform: translateY(6px) scale(.99); } }
@keyframes slide { from { transform: translateX(24px); opacity: 0; } }
</style>
