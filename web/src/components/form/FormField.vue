<script setup lang="ts">
// Label + control + hint/error, wired for screen readers. The slot gets { id, describedby, invalid }:
// <FormField label="Name" :error="errors.name" hint="Lower-case, no spaces" v-slot="f">
//   <input :id="f.id" v-model="name" class="input" :aria-describedby="f.describedby" :aria-invalid="f.invalid" />
// </FormField>
import { computed, useId } from 'vue'
const props = defineProps<{ label: string; hint?: string; error?: string | null; required?: boolean; optional?: boolean; inline?: boolean }>()
const id = useId()
const hintId = `${id}-hint`
const errId = `${id}-err`
const invalid = computed<'true' | undefined>(() => (props.error ? 'true' : undefined))
const describedby = computed(() => [props.error ? errId : '', props.hint ? hintId : ''].filter(Boolean).join(' ') || undefined)
</script>

<template>
  <div class="field" :class="{ inline, invalid: !!error }">
    <label :for="id" class="lbl">{{ label }}<span v-if="required" class="req" aria-hidden="true">*</span><span v-if="optional" class="opt">optional</span></label>
    <div class="ctl"><slot :id="id" :describedby="describedby" :invalid="invalid" /></div>
    <p v-if="error" :id="errId" class="err" role="alert">{{ error }}</p>
    <p v-if="hint" :id="hintId" class="hint">{{ hint }}</p>
  </div>
</template>

<style scoped>
.field { display: flex; flex-direction: column; gap: 5px; min-width: 0; }
.field.inline { flex-direction: row; align-items: center; gap: 12px; }
.lbl { font-size: var(--fs-sm); font-weight: var(--fw-medium); color: var(--text-2); }
.req { color: var(--critical); margin-left: 2px; }
.opt { margin-left: 6px; font-weight: var(--fw-regular); color: var(--text-faint); }
.hint { font-size: var(--fs-xs); color: var(--text-muted); }
.err { font-size: var(--fs-sm); color: var(--critical-text); }
</style>
