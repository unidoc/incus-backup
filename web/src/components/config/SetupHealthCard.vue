<script setup lang="ts">
// "Is everything set up?": the setup checklist of GET /setup-checklist as a card. On the overview
// (`overview`) it shows only while something is missing, and only to administrators (they fix it).
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import Skeleton from '@/components/Skeleton.vue'
import { useResource } from '@/api/cache'
import { cfg, canCfg } from '@/api/sections/config'
import { session } from '@/stores/session'
import { canOpen } from '@/router'

const props = defineProps<{ overview?: boolean }>()
const allowed = computed(() => canCfg(session.role, 'GET /setup-checklist'))
const res = useResource(() => 'cfg:checklist', (s) => cfg.checklist(s), { enabled: () => allowed.value, refreshOn: ['host', 'alert'], ttl: 30_000 })
const c = computed(() => res.data.value)
const show = computed(() => allowed.value && (!props.overview || (c.value && !c.value.complete)))
const items = computed(() => (c.value?.items ?? []).filter((i) => !props.overview || !i.ok))
const pct = computed(() => (c.value ? Math.round((c.value.done / c.value.total) * 100) : 0))
</script>

<template>
  <section v-if="show" class="panel shc" :class="{ ov: overview }" data-testid="setup-checklist">
    <div class="panel-head">
      <h2>{{ overview ? 'Finish setting up' : 'Is everything set up?' }}</h2>
      <span v-if="c" class="sub">{{ c.done }} of {{ c.total }} done</span>
      <RouterLink v-if="overview" :to="{ name: 'settings' }" class="more">Server settings</RouterLink>
    </div>
    <div class="panel-body">
      <Skeleton v-if="!c" :lines="4" />
      <template v-else>
        <div class="bar" role="progressbar" :aria-valuenow="pct" aria-valuemin="0" aria-valuemax="100" :aria-label="`${c.done} of ${c.total} setup steps done`"><span :style="{ width: pct + '%' }" /></div>
        <ul class="items">
          <li v-for="i in items" :key="i.id" :class="{ ok: i.ok }" :data-item="i.id" :data-ok="i.ok">
            <span class="ic" :class="i.ok ? 'ok' : 'todo'"><Icon :name="i.ok ? 'circle-check' : 'circle-dashed'" :size="16" /></span>
            <span class="tx"><span class="ti">{{ i.title }}</span><span class="de">{{ i.detail }}</span></span>
            <RouterLink v-if="!i.ok && canOpen(i.route)" :to="{ name: i.route }" class="btn btn-sm go">Fix<Icon name="chevron-right" :size="13" /></RouterLink>
          </li>
        </ul>
        <p v-if="c.complete && !overview" class="done">Everything is set up. Alerts reach people, backups have policies, and restores were proven.</p>
      </template>
    </div>
  </section>
</template>

<style scoped>
.shc.ov { border-left: 3px solid var(--info); }
.ov .items { grid-template-columns: repeat(auto-fill, minmax(min(100%, 420px), 1fr)); column-gap: 28px; }
.ov .items li { border-top: 0; border-bottom: 1px solid var(--border); }
.more { margin-left: auto; font-size: var(--fs-sm); }
.bar { height: 6px; border-radius: 3px; background: var(--surface-3); overflow: hidden; margin-bottom: 12px; }
.bar span { display: block; height: 100%; background: var(--ok); border-radius: 3px; transition: width var(--t) var(--ease); }
.items { list-style: none; margin: 0; padding: 0; display: grid; gap: 2px; }
.items li { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; gap: 10px; align-items: center; padding: 7px 0; border-top: 1px solid var(--border); }
.items li:first-child { border-top: 0; }
.ic.ok { color: var(--ok); }
.ic.todo { color: var(--warn); }
.tx { display: flex; flex-direction: column; min-width: 0; }
.ti { font-weight: var(--fw-medium); }
li.ok .ti { color: var(--text-2); font-weight: var(--fw-regular, 400); }
.de { font-size: var(--fs-sm); color: var(--text-muted); overflow-wrap: anywhere; }
.go { white-space: nowrap; }
.done { margin-top: 10px; color: var(--ok-text); font-size: var(--fs-sm); }
</style>
