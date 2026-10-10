<script setup lang="ts">
// Password field with show/hide and (with `strength`) the guidance meter for new passwords.
import { ref, computed } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import { passwordStrength } from '@/lib/password'

const props = defineProps<{ modelValue: string; id?: string; strength?: boolean; context?: string[]; autocomplete?: string; describedby?: string; invalid?: 'true' | 'false'; name?: string }>()
const emit = defineEmits<{ 'update:modelValue': [string] }>()
const show = ref(false)
const st = computed(() => passwordStrength(props.modelValue, props.context))
</script>

<template>
  <div class="pw">
    <div class="wrap">
      <input :id="id" :name="name" class="input" :type="show ? 'text' : 'password'" :value="modelValue" :autocomplete="autocomplete ?? 'current-password'" :aria-describedby="describedby" :aria-invalid="invalid" spellcheck="false" @input="emit('update:modelValue', ($event.target as HTMLInputElement).value)" />
      <button type="button" class="eye" :aria-label="show ? 'Hide password' : 'Show password'" :aria-pressed="show" @click="show = !show"><Icon :name="show ? 'eye-off' : 'eye'" :size="15" /></button>
    </div>
    <div v-if="strength && modelValue" class="st" :class="`sc${st.score}`" aria-live="polite">
      <div class="bars" aria-hidden="true"><span v-for="i in 4" :key="i" :class="{ on: i <= st.score }" /></div>
      <span class="lbl">{{ st.label }}</span>
      <ul v-if="st.hints.length" class="hints"><li v-for="h in st.hints" :key="h">{{ h }}</li></ul>
    </div>
  </div>
</template>

<style scoped>
.pw { display: flex; flex-direction: column; gap: 6px; }
.wrap { position: relative; }
.wrap .input { padding-right: 36px; }
.eye { position: absolute; right: 2px; top: 2px; bottom: 2px; width: 30px; display: grid; place-items: center; border: 0; background: none; color: var(--text-muted); cursor: pointer; border-radius: var(--r-sm); }
.eye:hover { color: var(--text); }
.st { display: grid; grid-template-columns: auto 1fr; gap: 4px 10px; align-items: center; font-size: var(--fs-xs); }
.bars { display: flex; gap: 3px; }
.bars span { width: 26px; height: 4px; border-radius: 2px; background: var(--surface-3); }
.sc0 .bars span.on, .sc1 .bars span.on { background: var(--critical); }
.sc2 .bars span.on { background: var(--warn); }
.sc3 .bars span.on, .sc4 .bars span.on { background: var(--ok); }
.lbl { color: var(--text-2); font-weight: var(--fw-medium); }
.hints { grid-column: 1 / -1; margin: 0; padding-left: 16px; color: var(--text-muted); }
</style>
