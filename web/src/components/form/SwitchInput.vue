<script setup lang="ts">
// On/off switch (role="switch"); the label is part of the control so it is clickable.
defineProps<{ modelValue: boolean; label?: string; disabled?: boolean; id?: string }>()
const emit = defineEmits<{ 'update:modelValue': [boolean] }>()
</script>

<template>
  <button :id="id" type="button" role="switch" class="sw" :aria-checked="modelValue" :disabled="disabled" @click="emit('update:modelValue', !modelValue)">
    <span class="track" aria-hidden="true"><span class="knob" /></span>
    <span v-if="label" class="l">{{ label }}</span>
  </button>
</template>

<style scoped>
.sw { display: inline-flex; align-items: center; gap: 8px; padding: 2px 0; text-align: left; border: 0; background: none; cursor: pointer; color: var(--text); }
.sw:disabled { opacity: .5; cursor: not-allowed; }
.track { position: relative; width: 30px; height: 18px; border-radius: 9px; background: var(--border-strong); transition: background var(--t-fast); }
.knob { position: absolute; top: 2px; left: 2px; width: 14px; height: 14px; border-radius: 50%; background: var(--surface); box-shadow: 0 1px 2px oklch(0% 0 0 / .25); transition: transform var(--t-fast) var(--ease); }
.sw[aria-checked='true'] .track { background: var(--accent); }
.sw[aria-checked='true'] .knob { transform: translateX(12px); }
</style>
