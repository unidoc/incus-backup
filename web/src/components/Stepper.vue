<script setup lang="ts">
// Progress through a wizard: <Stepper :steps="['Host', 'Policy', 'Command']" :current="1" />.
import Icon from '@/components/icons/Icon.vue'
defineProps<{ steps: string[]; current: number }>()
</script>

<template>
  <ol class="stepper" aria-label="Progress">
    <li v-for="(s, i) in steps" :key="s" :class="{ done: i < current, cur: i === current }" :aria-current="i === current ? 'step' : undefined">
      <span class="n"><Icon v-if="i < current" name="check" :size="12" :stroke="3" /><template v-else>{{ i + 1 }}</template></span>
      <span class="l">{{ s }}</span>
    </li>
  </ol>
</template>

<style scoped>
.stepper { display: flex; gap: 0; list-style: none; margin: 0 0 var(--s-5); padding: 0; counter-reset: s; flex-wrap: wrap; row-gap: 8px; }
li { display: flex; align-items: center; gap: 8px; color: var(--text-muted); font-size: var(--fs-sm); }
li + li::before { content: ''; width: 28px; height: 1px; background: var(--border-strong); margin: 0 10px; }
.n { display: grid; place-items: center; width: 22px; height: 22px; border-radius: 50%; border: 1px solid var(--border-strong); font-size: var(--fs-xs); font-weight: var(--fw-semibold); background: var(--surface); }
.cur { color: var(--text); font-weight: var(--fw-medium); }
.cur .n { border-color: var(--accent); color: var(--accent-text); box-shadow: 0 0 0 3px var(--accent-soft); }
.done .n { background: var(--accent); border-color: var(--accent); color: var(--on-accent); }
.done { color: var(--text-2); }
@media (max-width: 520px) { .l { display: none; } .cur .l { display: inline; } li + li::before { width: 14px; margin: 0 6px; } }
</style>
