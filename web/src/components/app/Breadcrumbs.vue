<script setup lang="ts">
// Page trail from the route meta (parent chain) plus the identifying parameter of detail pages.
import { computed } from 'vue'
import { RouterLink, useRoute, useRouter, type RouteLocationRaw } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'

const route = useRoute()
const router = useRouter()
interface Crumb { label: string; to?: RouteLocationRaw }

function paramLabel(name: string): string | null {
  const p = route.params
  if (name === 'snapshot' || name === 'snapshot-browse') return p.snap ? String(p.snap).slice(0, 19) : null
  for (const k of ['name', 'group', 'id']) if (p[k]) return String(p[k])
  return null
}

const crumbs = computed<Crumb[]>(() => {
  const out: Crumb[] = []
  let name: string | undefined = String(route.name ?? '')
  const seen = new Set<string>()
  while (name && !seen.has(name)) {
    seen.add(name)
    const r = router.getRoutes().find((x) => x.name === name)
    if (!r) break
    const isCurrent = name === route.name
    const hasParam = r.path.includes(':')
    const label = hasParam ? paramLabel(name) ?? String(r.meta.title) : String(r.meta.title ?? name)
    const to: RouteLocationRaw | undefined = isCurrent ? undefined : { name, params: hasParam ? route.params : undefined }
    out.unshift({ label, to })
    name = r.meta.parent
  }
  return out
})
</script>

<template>
  <nav aria-label="Breadcrumb" class="bc">
    <ol>
      <li v-for="(c, i) in crumbs" :key="i">
        <Icon v-if="i > 0" name="chevron-right" :size="13" class="sep" />
        <RouterLink v-if="c.to" :to="c.to">{{ c.label }}</RouterLink>
        <span v-else aria-current="page" class="cur">{{ c.label }}</span>
      </li>
    </ol>
  </nav>
</template>

<style scoped>
.bc { min-width: 0; }
ol { display: flex; align-items: center; gap: 2px; list-style: none; margin: 0; padding: 0; min-width: 0; }
li { display: flex; align-items: center; gap: 2px; min-width: 0; }
li:not(:last-child) { flex-shrink: 0; }
a { color: var(--text-muted); padding: 2px 4px; border-radius: var(--r-sm); white-space: nowrap; }
a:hover { color: var(--text); background: var(--surface-3); text-decoration: none; }
.cur { color: var(--text); font-weight: var(--fw-medium); padding: 2px 4px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.sep { color: var(--text-faint); }
@media (max-width: 600px) { li:not(:last-child) { display: none; } }
</style>
