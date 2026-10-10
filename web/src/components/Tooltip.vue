<script setup lang="ts">
// Hover/focus tooltip around inline content. Keyboard users get it on focus (the trigger is focusable
// when `focusable`); screen readers get the text through aria-describedby.
import { ref, useId } from 'vue'

withDefaults(defineProps<{ text?: string; placement?: 'top' | 'bottom'; focusable?: boolean }>(), { placement: 'top', focusable: true })
const open = ref(false)
const id = useId()
let t: ReturnType<typeof setTimeout> | null = null
const show = () => { if (t) clearTimeout(t); t = setTimeout(() => (open.value = true), 250) }
const hide = () => { if (t) clearTimeout(t); open.value = false }
</script>

<template>
  <span class="tt" @mouseenter="show" @mouseleave="hide" @focusin="open = true" @focusout="hide" @keydown.esc="hide">
    <span :tabindex="focusable ? 0 : undefined" :aria-describedby="open ? id : undefined" class="tt-trigger"><slot /></span>
    <span v-if="open" :id="id" role="tooltip" class="tt-pop" :class="placement"><slot name="content">{{ text }}</slot></span>
  </span>
</template>

<style scoped>
.tt { position: relative; display: inline-flex; max-width: 100%; }
.tt-trigger { display: inline-flex; min-width: 0; border-radius: var(--r-sm); }
.tt-pop {
  position: absolute; left: 50%; transform: translateX(-50%); z-index: var(--z-tooltip);
  width: max-content; max-width: 320px; padding: 6px 9px; border-radius: var(--r);
  background: var(--text); color: var(--bg); font-size: var(--fs-xs); line-height: 1.45; font-weight: var(--fw-regular);
  box-shadow: var(--shadow-pop); pointer-events: none; white-space: normal; text-align: left;
}
.tt-pop.top { bottom: calc(100% + 6px); }
.tt-pop.bottom { top: calc(100% + 6px); }
</style>
