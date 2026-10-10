<script setup lang="ts">
// Repository switcher: only shown when more than one repository exists. Mode is shown next to the name.
import { computed } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import { repos, repoMany, selectRepo, currentRepo } from '@/stores/repos'
import { statusOf } from '@/lib/status'

const mode = computed(() => currentRepo.value?.mode ?? 'online')
</script>

<template>
  <label v-if="repoMany" class="rs" :class="`s-${statusOf(mode)}`">
    <Icon name="database" :size="14" />
    <span class="sr-only">Repository</span>
    <select :value="repos.current" data-testid="repo-select" @change="selectRepo(($event.target as HTMLSelectElement).value)">
      <option v-for="r in repos.list" :key="r.name" :value="r.name">{{ r.name }}{{ r.mode !== 'online' ? ` (${r.mode})` : '' }}</option>
    </select>
    <Icon name="chevron-down" :size="13" class="chev" />
  </label>
</template>

<style scoped>
.rs { position: relative; display: inline-flex; align-items: center; gap: 6px; height: 30px; padding: 0 8px; border: 1px solid var(--border-strong); border-radius: var(--r); background: var(--surface); color: var(--text-muted); }
.rs:focus-within { border-color: var(--focus); }
select { appearance: none; border: 0; background: transparent; color: var(--text); font-size: var(--fs-sm); font-weight: var(--fw-medium); padding-right: 14px; max-width: 160px; cursor: pointer; outline: none; }
.chev { position: absolute; right: 8px; pointer-events: none; }
.s-warn { border-color: var(--warn-border); background: var(--warn-bg); }
.s-critical { border-color: var(--critical-border); background: var(--critical-bg); }
</style>
