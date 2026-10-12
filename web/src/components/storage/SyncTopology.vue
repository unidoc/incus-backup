<script setup lang="ts">
// Replication at a glance: this server (and repository) on the left, every remote server on the right,
// one arrow per sync job in the direction data flows (push: towards the remote, pull: towards us),
// coloured AND labelled by health. The initiator is always this server.
import { computed } from 'vue'
import type { Status } from '@/lib/status'

export interface TopoJob { name: string; remote: string; remoteRepo: string; direction: 'push' | 'pull'; status: Status; label: string }
const props = defineProps<{ server: string; repo: string; jobs: TopoJob[] }>()
const emit = defineEmits<{ select: [string] }>()

const remotes = computed(() => {
  const m = new Map<string, TopoJob[]>()
  for (const j of props.jobs) { if (!m.has(j.remote)) m.set(j.remote, []); m.get(j.remote)!.push(j) }
  return [...m.entries()].map(([addr, jobs]) => ({ addr, jobs, repos: [...new Set(jobs.map((j) => j.remoteRepo))].join(', ') }))
})
const W = 720
const ROW = 96
const H = computed(() => Math.max(150, 40 + remotes.value.length * ROW))
const cx = 120, rx = W - 140
const cy = computed(() => H.value / 2)
const ry = (i: number) => 20 + ROW / 2 + i * ROW + (H.value - 40 - remotes.value.length * ROW) / 2
const color: Record<Status, string> = { ok: 'var(--ok)', warn: 'var(--warn)', critical: 'var(--critical)', info: 'var(--info)', neutral: 'var(--neutral)' }
const statusId = (s: Status) => `ar-${s}`

function edge(i: number, k: number, n: number, dir: 'push' | 'pull') {
  const step = n > 1 ? Math.min(34, 52 / (n - 1)) : 0
  const spread = (k - (n - 1) / 2) * step
  const x1 = cx + 96, y1 = cy.value + spread, x2 = rx - 96, y2 = ry(i) + spread * 0.8
  const mx = (x1 + x2) / 2
  const d = dir === 'push' ? `M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2}` : `M${x2},${y2} C${mx},${y2} ${mx},${y1} ${x1},${y1}`
  const my = (y1 + y2) / 2
  return { d, lx: mx, ly: spread < 0 || n === 1 ? my - 8 : my + 16 }
}
</script>

<template>
  <figure class="topo" data-testid="sync-topology">
    <svg :viewBox="`0 0 ${W} ${H}`" role="img" :aria-label="`Replication of ${repo}: ${jobs.map((j) => `${j.name} ${j.direction} ${j.direction === 'push' ? 'to' : 'from'} ${j.remote}, ${j.label}`).join('; ') || 'no sync jobs'}`">
      <defs>
        <marker v-for="s in (['ok', 'warn', 'critical', 'info', 'neutral'] as Status[])" :id="statusId(s)" :key="s" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
          <path d="M0,0 L10,5 L0,10 z" :style="{ fill: color[s] }" />
        </marker>
      </defs>
      <!-- this server -->
      <g class="node me">
        <rect :x="cx - 96" :y="cy - 34" width="192" height="68" rx="8" />
        <text :x="cx" :y="cy - 8" text-anchor="middle" class="t1">{{ server }}</text>
        <text :x="cx" :y="cy + 12" text-anchor="middle" class="t2">this server · {{ repo }}</text>
      </g>
      <!-- remotes and edges -->
      <g v-for="(r, i) in remotes" :key="r.addr">
        <g v-for="(j, k) in r.jobs" :key="j.name" class="edge" tabindex="0" role="button" :aria-label="`Open sync job ${j.name}`" @click="emit('select', j.name)" @keydown.enter="emit('select', j.name)">
          <path :d="edge(i, k, r.jobs.length, j.direction).d" :style="{ stroke: color[j.status] }" :marker-end="`url(#${statusId(j.status)})`" :class="{ dash: j.status === 'neutral' }" />
          <text :x="edge(i, k, r.jobs.length, j.direction).lx" :y="edge(i, k, r.jobs.length, j.direction).ly" text-anchor="middle" class="el">{{ j.name }} · {{ j.direction }} · {{ j.label }}</text>
        </g>
        <g class="node">
          <rect :x="rx - 96" :y="ry(i) - 30" width="192" height="60" rx="8" />
          <text :x="rx" :y="ry(i) - 6" text-anchor="middle" class="t1 mono">{{ r.addr }}</text>
          <text :x="rx" :y="ry(i) + 13" text-anchor="middle" class="t2">repository {{ r.repos }}</text>
        </g>
      </g>
      <text v-if="!remotes.length" :x="W / 2 + 60" :y="cy + 4" text-anchor="middle" class="t2">No sync job: this repository is not replicated.</text>
    </svg>
  </figure>
</template>

<style scoped>
.topo { margin: 0; }
svg { width: 100%; max-width: 860px; margin: 0 auto; height: auto; display: block; }
.node rect { fill: var(--surface); stroke: var(--border-strong); stroke-width: 1.2; }
.me rect { fill: var(--accent-soft); stroke: var(--accent); }
.t1 { font-size: 13px; font-weight: 600; fill: var(--text); }
.t2 { font-size: 11px; fill: var(--text-muted); }
.mono { font-family: var(--font-mono); font-size: 12px; }
.edge path { fill: none; stroke-width: 2.2; }
.edge path.dash { stroke-dasharray: 5 4; }
.edge { cursor: pointer; outline: none; }
.edge:hover path, .edge:focus path { stroke-width: 3.4; }
.el { font-size: 11px; fill: var(--text-2); paint-order: stroke; stroke: var(--surface); stroke-width: 4px; stroke-linejoin: round; }
</style>
