<script setup lang="ts">
// Native <select> (keyboard and mobile friendly) with the console's look.
// <SelectInput v-model="role" :options="[{ value: 'readonly', label: 'Read-only' }]" />
export interface Option { value: string; label: string; disabled?: boolean }
defineProps<{ options: Array<Option | string>; modelValue: string; id?: string; placeholder?: string; disabled?: boolean; size?: 'sm' | 'md' }>()
const emit = defineEmits<{ 'update:modelValue': [string] }>()
const norm = (o: Option | string): Option => (typeof o === 'string' ? { value: o, label: o } : o)
</script>

<template>
  <select :id="id" class="select" :class="{ sm: size === 'sm' }" :value="modelValue" :disabled="disabled" @change="emit('update:modelValue', ($event.target as HTMLSelectElement).value)">
    <option v-if="placeholder" value="" disabled>{{ placeholder }}</option>
    <option v-for="o in options.map(norm)" :key="o.value" :value="o.value" :disabled="o.disabled">{{ o.label }}</option>
  </select>
</template>

<style scoped>
.sm { height: var(--control-h-sm); font-size: var(--fs-sm); }
</style>
