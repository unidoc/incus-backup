<script setup lang="ts">
// Wrapper every chart uses: title, legend (always for >= 2 series), and the data table, which is the
// accessible fallback (screen readers always get it; sighted users toggle it with "Table").
import { ref } from 'vue'
import Icon from '@/components/icons/Icon.vue'

export interface LegendItem { label: string; color: string; value?: string }
defineProps<{ title?: string; subtitle?: string; legend?: LegendItem[]; columns: string[]; rows: Array<Array<string>> }>()
const showTable = ref(false)
</script>

<template>
  <figure class="cf">
    <figcaption v-if="title || legend?.length" class="head">
      <div class="tt">
        <span v-if="title" class="title">{{ title }}</span>
        <span v-if="subtitle" class="sub">{{ subtitle }}</span>
      </div>
      <ul v-if="legend && legend.length > 1" class="legend">
        <li v-for="l in legend" :key="l.label"><span class="sw" :style="{ background: l.color }" />{{ l.label }}<b v-if="l.value" class="num">{{ l.value }}</b></li>
      </ul>
      <button type="button" class="btn btn-ghost btn-sm tbl" :aria-pressed="showTable" @click="showTable = !showTable"><Icon name="columns-3" :size="13" />{{ showTable ? 'Chart' : 'Table' }}</button>
    </figcaption>
    <div v-show="!showTable"><slot /></div>
    <div :class="showTable ? 'table-wrap' : 'sr-only'">
      <table class="data">
        <thead><tr><th v-for="c in columns" :key="c" scope="col">{{ c }}</th></tr></thead>
        <tbody><tr v-for="(r, i) in rows" :key="i"><td v-for="(v, j) in r" :key="j" :class="{ num: j > 0 }">{{ v }}</td></tr></tbody>
      </table>
    </div>
  </figure>
</template>

<style scoped>
.cf { margin: 0; min-width: 0; }
.head { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; margin-bottom: 8px; }
.tt { display: flex; flex-direction: column; min-width: 0; }
.title { font-weight: var(--fw-semibold); font-size: var(--fs-md); color: var(--text); }
.sub { font-size: var(--fs-xs); color: var(--text-muted); }
.legend { display: flex; flex-wrap: wrap; gap: 4px 14px; list-style: none; margin: 0; padding: 0; font-size: var(--fs-sm); color: var(--text-2); }
.legend li { display: inline-flex; align-items: center; gap: 6px; }
.legend b { font-weight: var(--fw-medium); color: var(--text); }
.sw { width: 10px; height: 10px; border-radius: 2px; }
.tbl { margin-left: auto; color: var(--text-muted); }
.table-wrap { max-height: 280px; overflow: auto; border: 1px solid var(--border); border-radius: var(--r); }
.data { width: 100%; border-collapse: collapse; font-size: var(--fs-sm); }
.data th, .data td { padding: 5px 10px; border-bottom: 1px solid var(--border); text-align: left; white-space: nowrap; }
.data th { position: sticky; top: 0; background: var(--surface-2); color: var(--text-muted); font-weight: var(--fw-medium); }
.data td.num { text-align: right; }
</style>
