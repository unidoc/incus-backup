<script setup lang="ts" generic="T">
// The console's table. Sortable headers, a filter box (terms AND-ed, `col:term`, `-term`), a column
// chooser (choice remembered under `storageKey`), sticky header, row selection, CSV export of what is
// shown, loading/empty/error states, and windowed rendering above `virtualAbove` rows (fixed row height)
// so 10 000 rows scroll smoothly.
//
// <DataTable :rows="hosts" :columns="cols" row-key="name" storage-key="hosts" csv-name="hosts">
//   <template #cell-name="{ row }"><RouterLink :to="…">{{ row.name }}</RouterLink></template>
//   <template #toolbar><button class="btn">Add host</button></template>
//   <template #empty><EmptyState title="No hosts yet" /></template>
// </DataTable>
import { computed, ref, watch, onMounted, onBeforeUnmount } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import Skeleton from './Skeleton.vue'
import EmptyState from './EmptyState.vue'
import Callout from './Callout.vue'
import { cellValue, filterRows, nextSort, sortRows, toCSV, virtualWindow, type ColumnDef, type SortState } from '@/lib/table'
import { downloadText } from '@/composables/useClipboard'
import { formatNumber } from '@/lib/format'

const props = withDefaults(defineProps<{
  rows: readonly T[] | null | undefined
  columns: ColumnDef<T>[]
  rowKey: string | ((row: T) => string)
  loading?: boolean
  error?: Error | string | null
  /** Retry handler shown with the error. */
  retry?: () => unknown
  selectable?: boolean
  selected?: string[]
  filterable?: boolean
  filterPlaceholder?: string
  initialSort?: SortState | null
  storageKey?: string
  csvName?: string
  emptyTitle?: string
  /** Max height of the scroll area (sticky header scrolls inside it). Default: grows with the page. */
  maxHeight?: string
  virtualAbove?: number
  rowHeight?: number
  /** Click on a row (not on links/buttons inside it). */
  rowClickable?: boolean
  dense?: boolean
  caption?: string
  /** Show the toolbar (filter, count, columns, CSV). Off for small embedded tables. */
  toolbar?: boolean
}>(), { toolbar: true, filterable: true, virtualAbove: 300, rowHeight: 36, emptyTitle: 'Nothing here yet', filterPlaceholder: 'Filter', initialSort: null })

const emit = defineEmits<{ 'update:selected': [string[]]; rowClick: [T] }>()
defineSlots<{
  [key: `cell-${string}`]: (p: { row: T; value: unknown }) => unknown
  toolbar?: () => unknown
  bulk?: (p: { selected: string[]; clear: () => void }) => unknown
  empty?: () => unknown
}>()

const keyOf = (r: T): string => (typeof props.rowKey === 'function' ? props.rowKey(r) : String((r as Record<string, unknown>)[props.rowKey]))

// ---- columns + chooser (persisted)
const hidden = ref<Set<string>>(new Set(props.columns.filter((c) => c.hidden).map((c) => c.key)))
const storeKey = computed(() => (props.storageKey ? `ib.table.${props.storageKey}.hidden` : ''))
onMounted(() => {
  if (!storeKey.value) return
  try {
    const v = localStorage.getItem(storeKey.value)
    if (v) hidden.value = new Set((JSON.parse(v) as string[]).filter((k) => !props.columns.find((c) => c.key === k)?.required))
  } catch { /* ignore */ }
})
function toggleColumn(k: string) {
  const s = new Set(hidden.value)
  s.has(k) ? s.delete(k) : s.add(k)
  hidden.value = s
  if (storeKey.value) try { localStorage.setItem(storeKey.value, JSON.stringify([...s])) } catch { /* ignore */ }
}
const visibleCols = computed(() => props.columns.filter((c) => !hidden.value.has(c.key)))
const chooserOpen = ref(false)
const chooserEl = ref<HTMLElement | null>(null)
function onDocClick(e: MouseEvent) { if (chooserOpen.value && chooserEl.value && !chooserEl.value.contains(e.target as Node)) chooserOpen.value = false }
onMounted(() => document.addEventListener('mousedown', onDocClick))
onBeforeUnmount(() => document.removeEventListener('mousedown', onDocClick))

// ---- filter + sort
const query = ref('')
const sort = ref<SortState | null>(props.initialSort)
const all = computed(() => props.rows ?? [])
const filtered = computed(() => filterRows(all.value, visibleCols.value, query.value))
const shown = computed(() => sortRows(filtered.value, props.columns, sort.value))
function clickSort(c: ColumnDef<T>) { if (c.sortable !== false) sort.value = nextSort(sort.value, c.key) }
function ariaSort(c: ColumnDef<T>) { return sort.value?.key === c.key ? (sort.value.dir === 'asc' ? 'ascending' : 'descending') : 'none' }

// ---- selection
const sel = computed(() => new Set(props.selected ?? []))
const allShownSelected = computed(() => shown.value.length > 0 && shown.value.every((r) => sel.value.has(keyOf(r))))
const someSelected = computed(() => shown.value.some((r) => sel.value.has(keyOf(r))))
function toggleRow(r: T) {
  const s = new Set(sel.value)
  const k = keyOf(r)
  s.has(k) ? s.delete(k) : s.add(k)
  emit('update:selected', [...s])
}
function toggleAll() {
  if (allShownSelected.value) {
    const drop = new Set(shown.value.map(keyOf))
    emit('update:selected', [...sel.value].filter((k) => !drop.has(k)))
  } else {
    emit('update:selected', [...new Set([...sel.value, ...shown.value.map(keyOf)])])
  }
}
const clearSel = () => emit('update:selected', [])

// ---- virtual window
const scroller = ref<HTMLElement | null>(null)
const scrollTop = ref(0)
const viewport = ref(600)
const virtual = computed(() => shown.value.length > props.virtualAbove)
const win = computed(() => (virtual.value ? virtualWindow(scrollTop.value, viewport.value, props.rowHeight, shown.value.length) : { start: 0, end: shown.value.length, padTop: 0, padBottom: 0 }))
const visibleRows = computed(() => shown.value.slice(win.value.start, win.value.end))
let raf = 0
function onScroll() {
  cancelAnimationFrame(raf)
  raf = requestAnimationFrame(() => { if (scroller.value) { scrollTop.value = scroller.value.scrollTop; viewport.value = scroller.value.clientHeight } })
}
let ro: ResizeObserver | null = null
onMounted(() => {
  if (scroller.value && typeof ResizeObserver !== 'undefined') {
    ro = new ResizeObserver(() => { if (scroller.value) viewport.value = scroller.value.clientHeight })
    ro.observe(scroller.value)
  }
})
onBeforeUnmount(() => ro?.disconnect())
watch(query, () => { if (scroller.value) scroller.value.scrollTop = 0 })
const scrollStyle = computed(() => ({ maxHeight: props.maxHeight ?? (virtual.value ? 'min(70vh, 720px)' : undefined) }))

// ---- export
function exportCSV() {
  downloadText(`${props.csvName ?? props.storageKey ?? 'table'}.csv`, toCSV(shown.value, visibleCols.value), 'text/csv')
}

function display(r: T, c: ColumnDef<T>) {
  if (c.format) return c.format(r)
  const v = cellValue(r, c)
  return v === null || v === undefined || v === '' ? '–' : String(v)
}
function onRowClick(e: MouseEvent, r: T) {
  if (!props.rowClickable) return
  if ((e.target as HTMLElement).closest('a,button,input,label,select')) return
  emit('rowClick', r)
}
const errText = computed(() => (props.error ? (typeof props.error === 'string' ? props.error : props.error.message) : ''))
const colspan = computed(() => visibleCols.value.length + (props.selectable ? 1 : 0))
</script>

<template>
  <div class="dt" :class="{ dense }">
    <div v-if="toolbar" class="bar">
      <label v-if="filterable" class="search">
        <Icon name="search" :size="14" />
        <span class="sr-only">Filter rows</span>
        <input v-model="query" type="search" :placeholder="filterPlaceholder" class="q" data-testid="table-filter" />
      </label>
      <span class="count num" aria-live="polite">
        <template v-if="query">{{ formatNumber(shown.length) }} of {{ formatNumber(all.length) }}</template>
        <template v-else-if="rows">{{ formatNumber(all.length) }} {{ all.length === 1 ? 'row' : 'rows' }}</template>
      </span>
      <template v-if="selectable && sel.size">
        <span class="selinfo">{{ sel.size }} selected <button type="button" class="link" @click="clearSel">Clear</button></span>
        <slot name="bulk" :selected="[...sel]" :clear="clearSel" />
      </template>
      <span class="spacer" />
      <slot name="toolbar" />
      <div ref="chooserEl" class="chooser">
        <button type="button" class="btn btn-sm" :aria-expanded="chooserOpen" aria-haspopup="true" @click="chooserOpen = !chooserOpen"><Icon name="columns-3" :size="14" /><span class="hide-sm">Columns</span></button>
        <div v-if="chooserOpen" class="menu" role="group" aria-label="Visible columns">
          <label v-for="c in columns" :key="c.key" class="opt" :class="{ dis: c.required }">
            <input type="checkbox" :checked="!hidden.has(c.key)" :disabled="c.required" @change="toggleColumn(c.key)" />{{ c.label }}
          </label>
        </div>
      </div>
      <button type="button" class="btn btn-sm" :disabled="!shown.length" title="Download the rows shown as CSV" @click="exportCSV"><Icon name="file-down" :size="14" /><span class="hide-sm">CSV</span></button>
    </div>

    <Callout v-if="error && rows" status="critical" title="Could not refresh this list" :retry="retry" class="stale">{{ errText }} Showing the last data received.</Callout>

    <div ref="scroller" class="scroll" :style="scrollStyle" @scroll.passive="onScroll">
      <table :aria-busy="loading ? 'true' : undefined" :aria-rowcount="shown.length + 1">
        <caption v-if="caption" class="sr-only">{{ caption }}</caption>
        <colgroup>
          <col v-if="selectable" style="width: 36px" />
          <col v-for="c in visibleCols" :key="c.key" :style="c.width ? { width: c.width } : undefined" />
        </colgroup>
        <thead>
          <tr>
            <th v-if="selectable" class="sel" scope="col">
              <input type="checkbox" :checked="allShownSelected" :indeterminate="!allShownSelected && someSelected" aria-label="Select all rows shown" @change="toggleAll" />
            </th>
            <th v-for="c in visibleCols" :key="c.key" scope="col" :class="[`a-${c.align ?? 'left'}`, { sortable: c.sortable !== false }]" :aria-sort="c.sortable !== false ? ariaSort(c) : undefined">
              <button v-if="c.sortable !== false" type="button" class="sortbtn" @click="clickSort(c)">
                <span>{{ c.label }}</span>
                <Icon :name="sort?.key === c.key ? (sort.dir === 'asc' ? 'arrow-up' : 'arrow-down') : 'arrow-up-down'" :size="12" class="si" :class="{ on: sort?.key === c.key }" />
              </button>
              <span v-else>{{ c.label }}</span>
            </th>
          </tr>
        </thead>
        <tbody v-if="loading && !rows">
          <tr v-for="i in 6" :key="i" class="skel"><td v-if="selectable" /><td v-for="c in visibleCols" :key="c.key"><Skeleton :width="`${40 + ((i * 17 + c.key.length * 7) % 50)}%`" /></td></tr>
        </tbody>
        <tbody v-else-if="error && !rows">
          <tr><td :colspan="colspan" class="state"><Callout status="critical" title="Could not load this list" :retry="retry">{{ errText }}</Callout></td></tr>
        </tbody>
        <tbody v-else-if="!shown.length">
          <tr><td :colspan="colspan" class="state">
            <EmptyState v-if="query" title="No rows match the filter" icon="filter" compact>Clear the filter or use fewer terms. <button type="button" class="link" @click="query = ''">Clear filter</button></EmptyState>
            <slot v-else name="empty"><EmptyState :title="emptyTitle" compact /></slot>
          </td></tr>
        </tbody>
        <tbody v-else>
          <tr v-if="win.padTop" class="pad" aria-hidden="true"><td :colspan="colspan" :style="{ height: `${win.padTop}px` }" /></tr>
          <tr v-for="(r, i) in visibleRows" :key="keyOf(r)" :class="{ selected: sel.has(keyOf(r)), clickable: rowClickable }" :aria-rowindex="win.start + i + 2" :style="virtual ? { height: `${rowHeight}px` } : undefined" @click="onRowClick($event, r)">
            <td v-if="selectable" class="sel"><input type="checkbox" :checked="sel.has(keyOf(r))" :aria-label="`Select ${keyOf(r)}`" @change="toggleRow(r)" /></td>
            <td v-for="c in visibleCols" :key="c.key" :class="[`a-${c.align ?? 'left'}`, { mono: c.mono }]">
              <slot :name="`cell-${c.key}`" :row="r" :value="cellValue(r, c)">{{ display(r, c) }}</slot>
            </td>
          </tr>
          <tr v-if="win.padBottom" class="pad" aria-hidden="true"><td :colspan="colspan" :style="{ height: `${win.padBottom}px` }" /></tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<style scoped>
.dt { display: flex; flex-direction: column; min-width: 0; background: var(--surface); border: 1px solid var(--border); border-radius: var(--r-lg); }
.bar { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; padding: 8px 10px; border-bottom: 1px solid var(--border); }
.search { display: flex; align-items: center; gap: 6px; height: var(--control-h-sm); padding: 0 8px; border: 1px solid var(--border-strong); border-radius: var(--r); background: var(--surface); color: var(--text-muted); width: 240px; max-width: 100%; }
.search:focus-within { border-color: var(--focus); box-shadow: 0 0 0 3px color-mix(in srgb, var(--focus) 22%, transparent); }
.q { flex: 1; min-width: 0; border: 0; outline: 0; background: transparent; font-size: var(--fs-sm); color: var(--text); }
.count { font-size: var(--fs-sm); color: var(--text-muted); }
.selinfo { font-size: var(--fs-sm); color: var(--text-2); font-weight: var(--fw-medium); }
.link { border: 0; background: none; padding: 0; color: var(--accent-text); cursor: pointer; font: inherit; }
.link:hover { text-decoration: underline; }
.chooser { position: relative; }
.menu { position: absolute; right: 0; top: calc(100% + 4px); z-index: 20; min-width: 200px; max-height: 320px; overflow: auto; padding: 6px; background: var(--surface); border: 1px solid var(--border-strong); border-radius: var(--r); box-shadow: var(--shadow-pop); }
.opt { display: flex; align-items: center; gap: 8px; padding: 5px 8px; border-radius: var(--r-sm); font-size: var(--fs-sm); cursor: pointer; }
.opt:hover { background: var(--surface-3); }
.opt.dis { color: var(--text-muted); cursor: default; }
.stale { margin: 8px 10px 0; }
.scroll { overflow: auto; min-width: 0; border-radius: 0 0 var(--r-lg) var(--r-lg); }
table { width: 100%; border-collapse: separate; border-spacing: 0; font-size: var(--fs-md); }
thead th { position: sticky; top: 0; z-index: 2; background: var(--surface-2); border-bottom: 1px solid var(--border); height: 34px; padding: 0 12px; font-size: var(--fs-sm); font-weight: var(--fw-medium); color: var(--text-muted); text-align: left; white-space: nowrap; }
th.sortable { padding: 0 4px; }
.sortbtn { display: inline-flex; align-items: center; gap: 4px; height: 26px; padding: 0 8px; border: 0; background: none; color: inherit; font: inherit; cursor: pointer; border-radius: var(--r-sm); }
.sortbtn:hover { color: var(--text); background: var(--surface-3); }
.si { opacity: .35; }
.si.on { opacity: 1; color: var(--accent-text); }
th.a-right .sortbtn { flex-direction: row-reverse; }
td { height: var(--row-h); padding: 0 12px; border-bottom: 1px solid var(--border); vertical-align: middle; color: var(--text); }
.dense td { height: 30px; }
tbody tr:last-child td { border-bottom: 0; }
tbody tr:not(.pad):not(.skel):hover td { background: var(--surface-2); }
tr.selected td { background: var(--accent-soft) !important; }
tr.clickable { cursor: pointer; }
.a-right { text-align: right; }
th.a-right { text-align: right; }
.a-center { text-align: center; }
td.mono { font-family: var(--font-mono); font-size: 12.5px; }
.sel { width: 36px; padding: 0 0 0 12px; }
.sel input { margin: 0; vertical-align: middle; accent-color: var(--accent); }
.state { padding: 8px 12px; height: auto; }
.pad td { padding: 0; border: 0; }
@media (max-width: 600px) { .hide-sm { display: none; } .search { width: 100%; } }
</style>
