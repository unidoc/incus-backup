<script setup lang="ts">
// Top-bar indicator: how many server tasks run right now (GET /tasks?state=active, refreshed on the SSE
// event "task"). Hidden when nothing runs; links to the task list.
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import Spinner from '@/components/Spinner.vue'
import { useResource } from '@/api/cache'
import { opsApi } from '@/api/sections/ops'
import { list } from '@/api/endpoints'
import { taskKindLabel } from '@/lib/tasks'

const r = useResource(() => 'tasks:active', (s) => opsApi.tasks.list({ state: 'active', limit: 50 }, s), { refreshOn: ['task'], poll: 30_000, ttl: 2_000 })
const active = computed(() => list(r.data.value))
const title = computed(() => active.value.map((t) => `${taskKindLabel(t.kind)}${t.repository ? ` on ${t.repository}` : ''}: ${t.progress}%`).join('\n'))
</script>

<template>
  <RouterLink v-if="active.length" :to="{ name: 'tasks' }" class="rt" :title="title" data-testid="running-tasks">
    <Spinner :size="12" label="Tasks running" />
    <span class="n num">{{ active.length }}</span><span class="lbl">{{ active.length === 1 ? 'task' : 'tasks' }} running</span>
  </RouterLink>
</template>

<style scoped>
.rt { display: inline-flex; align-items: center; gap: 6px; height: 28px; padding: 0 10px; border-radius: 14px; border: 1px solid var(--info-border); background: var(--info-bg); color: var(--info-text); font-size: var(--fs-sm); white-space: nowrap; }
.rt:hover { text-decoration: none; filter: brightness(0.97); }
.n { font-weight: var(--fw-semibold); }
@media (max-width: 600px) { .lbl { display: none; } }
</style>
