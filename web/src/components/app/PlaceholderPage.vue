<script setup lang="ts">
// Stand-in for a page a section agent has not built yet. Shows what the page is, who may open it and
// which API routes it will use, so navigation and role checks can be verified end to end.
// Delete the <PlaceholderPage> from the view file when you build the real page.
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { roleFor, atLeast } from '@/api/perms'
import { session } from '@/stores/session'
import StatusPill from '@/components/StatusPill.vue'

const props = defineProps<{ note?: string }>()
const route = useRoute()
const role = computed(() => route.meta.role ?? 'readonly')
const record = computed(() => route.matched[route.matched.length - 1])
const apis = computed(() => (route.meta.apis ?? []).map((a) => {
  const [m, p] = a.split(' ')
  const key = `${m} ${p.split('?')[0]}`
  const need = roleFor(key)
  return { route: a, need, ok: atLeast(session.role, need) }
}))
const params = computed(() => Object.entries(route.params).map(([k, val]) => `${k} = ${Array.isArray(val) ? val.join('/') : val}`))
</script>

<template>
  <section class="ph" data-testid="placeholder" :data-route="String(route.name)" :data-role="role">
    <header class="page-head">
      <div>
        <h1>{{ route.meta.title }}</h1>
        <p class="lede">{{ props.note ?? 'This page is a placeholder: the section is being built.' }}</p>
      </div>
    </header>
    <div class="panel">
      <div class="panel-head"><h2>Page contract</h2><span class="sub">for the section that replaces this view</span></div>
      <dl class="kv">
        <dt>Route</dt><dd><code>{{ String(route.name) }}</code> &nbsp;<code>{{ record?.path }}</code></dd>
        <dt>Parameters</dt><dd><code v-for="p in params" :key="p" class="param">{{ p }}</code><span v-if="!params.length" class="muted">none</span></dd>
        <dt>View file</dt><dd><code>web/src/views/{{ route.meta.file }}</code></dd>
        <dt>Required role</dt>
        <dd class="row"><code data-testid="required-role">{{ role }}</code><StatusPill :status="atLeast(session.role, role) ? 'ok' : 'critical'" :label="atLeast(session.role, role) ? `you are ${session.role}` : `you are ${session.role}: no access`" /></dd>
        <dt>Backend</dt><dd><StatusPill :status="route.meta.planned ? 'warn' : 'ok'" :label="route.meta.planned ? 'planned routes (draft types)' : 'routes available'" /></dd>
      </dl>
      <div v-if="apis.length" class="apis-wrap"><table class="apis">
        <thead><tr><th>API route</th><th>Role</th><th>You</th></tr></thead>
        <tbody>
          <tr v-for="a in apis" :key="a.route"><td><code>{{ a.route }}</code></td><td>{{ a.need }}</td><td><StatusPill :status="a.ok ? 'ok' : 'neutral'" :label="a.ok ? 'allowed' : 'hidden'" /></td></tr>
        </tbody>
      </table></div>
    </div>
  </section>
</template>

<style scoped>
.kv { display: grid; grid-template-columns: 140px 1fr; gap: 8px 16px; margin: 0; padding: var(--s-4); }
.kv dt { color: var(--text-muted); }
.kv dd { margin: 0; min-width: 0; overflow-wrap: anywhere; }
.param { margin-right: 8px; }
.apis-wrap { overflow-x: auto; }
.apis td code { overflow-wrap: anywhere; }
.apis { width: 100%; border-collapse: collapse; border-top: 1px solid var(--border); }
.apis th, .apis td { text-align: left; padding: 7px var(--s-4); border-bottom: 1px solid var(--border); font-size: var(--fs-sm); }
.apis th { color: var(--text-muted); font-weight: var(--fw-medium); background: var(--surface-2); }
.apis tr:last-child td { border-bottom: 0; }
@media (max-width: 600px) { .apis th, .apis td { padding: 7px 10px; } .kv { grid-template-columns: 1fr; gap: 2px; } .kv dd { margin-bottom: 8px; } }
</style>
