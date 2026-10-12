<script setup lang="ts">
// A connection / health check as a checklist: every step with pass, warning, failure, skipped or
// unknown, the server's detail and what to do about it (advice keyed by the check id).
import { computed } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import type { IconName } from '@/components/icons'
import { checkAdvice } from '@/lib/storage'
import type { CheckItem } from '@/api/sections/storage'

const props = withDefaults(defineProps<{ checks: CheckItem[]; kind?: string; compact?: boolean }>(), { kind: 's3' })
const icon: Record<CheckItem['level'], IconName> = { pass: 'circle-check', warn: 'triangle-alert', fail: 'circle-alert', skip: 'circle-dashed', unknown: 'circle-help' }
const word: Record<CheckItem['level'], string> = { pass: 'Passed', warn: 'Warning', fail: 'Failed', skip: 'Not run', unknown: 'Unknown' }
const counts = computed(() => {
  const c = { pass: 0, warn: 0, fail: 0, skip: 0, unknown: 0 }
  for (const x of props.checks) c[x.level]++
  return c
})
</script>

<template>
  <div class="cl" :class="{ compact }" data-testid="checklist">
    <p class="sum" aria-live="polite">
      <span class="ok"><Icon name="circle-check" :size="14" />{{ counts.pass }} passed</span>
      <span v-if="counts.warn" class="warn"><Icon name="triangle-alert" :size="14" />{{ counts.warn }} {{ counts.warn === 1 ? 'warning' : 'warnings' }}</span>
      <span v-if="counts.fail" class="fail"><Icon name="circle-alert" :size="14" />{{ counts.fail }} failed</span>
      <span v-if="counts.unknown" class="muted"><Icon name="circle-help" :size="14" />{{ counts.unknown }} unknown</span>
      <span v-if="counts.skip" class="muted">{{ counts.skip }} not run</span>
    </p>
    <ol class="items">
      <li v-for="c in checks" :key="c.id + c.label" :class="`l-${c.level}`" :data-check="c.id" :data-level="c.level">
        <Icon :name="icon[c.level]" :size="16" class="ic" :label="word[c.level]" />
        <div class="body">
          <p class="t">{{ c.label }}<span class="lv">{{ word[c.level] }}</span></p>
          <p v-if="c.detail" class="d">{{ c.detail }}</p>
          <p v-if="checkAdvice(c.id, c.level, kind)" class="adv"><Icon name="info" :size="12" />{{ checkAdvice(c.id, c.level, kind) }}</p>
        </div>
      </li>
    </ol>
  </div>
</template>

<style scoped>
.cl { display: flex; flex-direction: column; gap: 8px; }
.sum { display: flex; flex-wrap: wrap; gap: 14px; font-size: var(--fs-sm); font-weight: var(--fw-medium); }
.sum span { display: inline-flex; align-items: center; gap: 5px; }
.sum .ok { color: var(--ok-text); } .sum .warn { color: var(--warn-text); } .sum .fail { color: var(--critical-text); }
.items { list-style: none; margin: 0; padding: 0; border: 1px solid var(--border); border-radius: var(--r); overflow: hidden; }
li { display: flex; gap: 10px; padding: 9px 12px; border-bottom: 1px solid var(--border); background: var(--surface); }
li:last-child { border-bottom: 0; }
.compact li { padding: 6px 10px; }
.ic { margin-top: 2px; flex: none; }
.l-pass .ic { color: var(--ok); } .l-warn .ic { color: var(--warn); } .l-fail .ic { color: var(--critical); } .l-skip .ic, .l-unknown .ic { color: var(--text-faint); }
.l-warn { background: color-mix(in srgb, var(--warn-bg) 55%, var(--surface)); }
.l-fail { background: color-mix(in srgb, var(--critical-bg) 60%, var(--surface)); }
.body { min-width: 0; flex: 1; }
.t { font-weight: var(--fw-medium); display: flex; gap: 8px; align-items: baseline; flex-wrap: wrap; }
.lv { font-size: var(--fs-xs); color: var(--text-muted); font-weight: var(--fw-regular); }
.l-skip .t, .l-unknown .t { color: var(--text-2); }
.d { color: var(--text-2); font-size: var(--fs-sm); margin-top: 2px; overflow-wrap: anywhere; }
.adv { display: flex; gap: 6px; align-items: flex-start; color: var(--text-muted); font-size: var(--fs-sm); margin-top: 4px; }
.adv .icon { margin-top: 3px; }
</style>
