import { onBeforeUnmount, onMounted, ref, type Ref } from 'vue'

/** Width of an element, kept current with a ResizeObserver (charts size themselves to their container). */
export function useElementWidth(el: Ref<HTMLElement | null>, fallback = 600) {
  const width = ref(fallback)
  let ro: ResizeObserver | null = null
  onMounted(() => {
    if (!el.value) return
    width.value = el.value.clientWidth || fallback
    if (typeof ResizeObserver === 'undefined') return
    ro = new ResizeObserver((e) => { const w = e[0]?.contentRect.width; if (w) width.value = Math.round(w) })
    ro.observe(el.value)
  })
  onBeforeUnmount(() => ro?.disconnect())
  return width
}
