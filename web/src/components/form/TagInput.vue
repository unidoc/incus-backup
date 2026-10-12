<script setup lang="ts">
// A list of short strings (targets, group filters, e-mail recipients). Enter or comma adds, Backspace on
// an empty input removes the last, × removes one. `validate` returns an error text or ''.
import { ref } from 'vue'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{ modelValue: string[]; id?: string; placeholder?: string; validate?: (v: string) => string }>()
const emit = defineEmits<{ 'update:modelValue': [string[]] }>()
const draft = ref('')
const error = ref('')

function add() {
  const v = draft.value.trim().replace(/,$/, '')
  if (!v) return
  const e = props.validate?.(v) ?? ''
  if (e) { error.value = e; return }
  if (!props.modelValue.includes(v)) emit('update:modelValue', [...props.modelValue, v])
  draft.value = ''
  error.value = ''
}
function remove(i: number) { emit('update:modelValue', props.modelValue.filter((_, j) => j !== i)) }
function onKey(e: KeyboardEvent) {
  if (e.key === 'Enter' || e.key === ',') { e.preventDefault(); add() }
  else if (e.key === 'Backspace' && !draft.value && props.modelValue.length) remove(props.modelValue.length - 1)
}
</script>

<template>
  <div class="tags" :class="{ invalid: !!error }">
    <span v-for="(t, i) in modelValue" :key="t" class="tag">{{ t }}<button type="button" :aria-label="`Remove ${t}`" @click="remove(i)"><Icon name="x" :size="12" /></button></span>
    <input :id="id" v-model="draft" class="in" :placeholder="modelValue.length ? '' : placeholder" @keydown="onKey" @blur="add" />
  </div>
  <p v-if="error" class="err" role="alert">{{ error }}</p>
</template>

<style scoped>
.tags { display: flex; flex-wrap: wrap; gap: 4px; align-items: center; min-height: var(--control-h); padding: 3px 4px; border: 1px solid var(--border-strong); border-radius: var(--r); background: var(--surface); }
.tags:focus-within { border-color: var(--focus); box-shadow: 0 0 0 3px color-mix(in srgb, var(--focus) 22%, transparent); }
.tags.invalid { border-color: var(--critical); }
.tag { display: inline-flex; align-items: center; gap: 2px; height: 22px; padding: 0 2px 0 7px; border-radius: var(--r-sm); background: var(--surface-3); font-size: var(--fs-sm); font-family: var(--font-mono); }
.tag button { display: grid; place-items: center; width: 18px; height: 18px; border: 0; background: none; color: var(--text-muted); cursor: pointer; border-radius: var(--r-sm); }
.tag button:hover { background: var(--border); color: var(--text); }
.in { flex: 1; min-width: 80px; height: 24px; border: 0; outline: 0; background: transparent; padding: 0 6px; }
.err { margin-top: 4px; font-size: var(--fs-sm); color: var(--critical-text); }
</style>
