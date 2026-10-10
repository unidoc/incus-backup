<script setup lang="ts">
// A JSON schema from the OpenAPI document as a compact field list: name, type, required, description;
// object fields and array items expand (references resolved, depth-limited against recursion).
import { computed } from 'vue'
import { deref, typeLabel, type OpenAPIDoc, type OSchema } from '@/api/sections/gapcore'

defineOptions({ name: 'SchemaTree' })
const props = withDefaults(defineProps<{ doc: OpenAPIDoc; schema: OSchema | undefined; depth?: number }>(), { depth: 0 })

const resolved = computed(() => {
  let s = deref(props.doc, props.schema)
  if (s?.anyOf) s = deref(props.doc, s.anyOf.find((x) => x.type !== 'null'))
  if (s?.allOf?.length === 1) s = deref(props.doc, s.allOf[0])
  if (s?.type === 'array' || (Array.isArray(s?.type) && s?.type.includes('array'))) return { array: true, s: deref(props.doc, s?.items) }
  return { array: false, s }
})
const fields = computed(() => {
  const s = resolved.value.s
  if (!s?.properties) return []
  const req = new Set(s.required ?? [])
  return Object.entries(s.properties).map(([name, f]) => ({ name, f, required: req.has(name), nested: !!deref(props.doc, f.anyOf?.find((x) => x.type !== 'null') ?? (f.allOf?.[0] ?? (f.type === 'array' ? f.items : f)))?.properties }))
})
const valueMap = computed(() => {
  const s = resolved.value.s
  return s?.type === 'object' && !s.properties && s.additionalProperties && typeof s.additionalProperties === 'object' ? s.additionalProperties : null
})
</script>

<template>
  <div class="tree" :class="{ top: depth === 0 }">
    <p v-if="depth === 0" class="head"><code>{{ typeLabel(schema) }}</code><span v-if="resolved.array" class="muted"> (a list; each item:)</span></p>
    <p v-if="valueMap" class="muted small">An object with any keys; each value: <code>{{ typeLabel(valueMap) }}</code></p>
    <ul v-if="fields.length && depth < 5">
      <li v-for="x in fields" :key="x.name">
        <details v-if="x.nested && depth < 4">
          <summary><code class="n">{{ x.name }}</code> <span class="t">{{ typeLabel(x.f) }}</span><span v-if="x.required" class="req">required</span><span v-if="x.f.description" class="d">{{ x.f.description }}</span></summary>
          <SchemaTree :doc="doc" :schema="x.f" :depth="depth + 1" />
        </details>
        <div v-else class="leaf"><code class="n">{{ x.name }}</code> <span class="t">{{ typeLabel(x.f) }}</span><span v-if="x.required" class="req">required</span><span v-if="x.f.description" class="d">{{ x.f.description }}</span></div>
      </li>
    </ul>
    <p v-else-if="depth === 0 && !valueMap && !resolved.s?.properties" class="muted small">{{ resolved.s?.description || 'A JSON value of this type.' }}</p>
  </div>
</template>

<style scoped>
.tree ul { list-style: none; margin: 0; padding: 0 0 0 14px; border-left: 1px solid var(--border); }
.tree.top > ul { padding-left: 0; border-left: 0; }
.tree li { padding: 3px 0; font-size: var(--fs-sm); }
.head { margin: 0 0 6px; font-size: var(--fs-sm); }
.n { font-weight: var(--fw-medium); }
.t { color: var(--accent-text); font-family: var(--font-mono); font-size: 0.92em; margin-left: 4px; }
.req { margin-left: 6px; font-size: var(--fs-xs); color: var(--warn-text); }
.d { display: block; color: var(--text-muted); margin-left: 2px; }
summary { cursor: pointer; }
.muted { color: var(--text-muted); }
.small { font-size: var(--fs-sm); }
</style>
