// Close a popover on outside click or Escape (focus returns to the trigger).
import { onBeforeUnmount, onMounted, type Ref } from 'vue'

export function useDismiss(root: Ref<HTMLElement | null>, open: Ref<boolean>, trigger?: Ref<HTMLElement | null>) {
  const onDown = (e: MouseEvent) => { if (open.value && root.value && !root.value.contains(e.target as Node)) open.value = false }
  const onKey = (e: KeyboardEvent) => { if (open.value && e.key === 'Escape') { open.value = false; trigger?.value?.focus() } }
  onMounted(() => { document.addEventListener('mousedown', onDown); document.addEventListener('keydown', onKey) })
  onBeforeUnmount(() => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey) })
}
