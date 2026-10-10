<script setup lang="ts">
// Policies of the selected repository: when backups run, where they go, what retention keeps, and
// which objects each policy governs; plus the assignment tree (host default / project / object).
import { computed } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import Tabs from '@/components/Tabs.vue'
import Callout from '@/components/Callout.vue'
import EmptyState from '@/components/EmptyState.vue'
import Skeleton from '@/components/Skeleton.vue'
import StatusPill from '@/components/StatusPill.vue'
import AssignmentMatrix from '@/components/storage/AssignmentMatrix.vue'
import { api, list } from '@/api/endpoints'
import { useResource } from '@/api/cache'
import { storageApi } from '@/api/sections/storage'
import { repos, repoNone } from '@/stores/repos'
import { hasRole } from '@/stores/session'
import { describeSchedule, plural } from '@/lib/format'
import { retentionSummary, rulesOf, RULE_ORDER } from '@/lib/retention'
import type { Policy } from '@/api/types'

const route = useRoute()
const router = useRouter()
const enabled = () => !!repos.current && !repoNone.value
const pols = useResource(() => `policies:${repos.current}`, (s) => api.policies.list(s), { enabled })
const asg = useResource(() => `assignments:${repos.current}`, (s) => api.assignments.list(s), { enabled })
const eff = useResource(() => `effective:${repos.current}`, (s) => storageApi.effectivePolicies(s), { enabled, refreshOn: ['run', 'host'] })
const rows = computed(() => list(pols.data.value))
const names = computed(() => rows.value.map((p) => p.name))
const assignCount = (p: Policy) => list(asg.data.value).filter((a) => a.policy === p.name).length
const objects = (p: Policy) => list(eff.data.value).filter((e) => e.policy === p.name)
const unknownPinned = computed(() => list(eff.data.value).filter((e) => e.policy && !e.policy_known))
const noPolicy = computed(() => list(eff.data.value).filter((e) => !e.policy))

const tab = computed({
  get: () => (route.query.tab === 'assignments' ? 'assignments' : 'policies'),
  set: (t: string) => { void router.replace({ query: { ...route.query, tab: t === 'policies' ? undefined : t } }) },
})
const tabs = computed(() => [{ id: 'policies', label: 'Policies', count: rows.value.length }, { id: 'assignments', label: 'Assignments', count: list(asg.data.value).length }])
const ruleBars = (p: Policy) => RULE_ORDER.map((k) => ({ k, n: rulesOf(p)[k] })).filter((x) => x.n > 0)
</script>

<template>
  <div class="pol">
    <header class="page-head">
      <div>
        <h1>Policies</h1>
        <p class="lede">A policy says when hosts back up, to which servers, and how long the server keeps the snapshots. Assign policies per host, project or object.</p>
      </div>
      <div class="actions">
        <RouterLink v-if="hasRole('operator')" :to="{ name: 'policy-new' }" class="btn btn-primary" data-testid="policy-new"><Icon name="plus" />New policy</RouterLink>
      </div>
    </header>

    <EmptyState v-if="repoNone" title="No repository yet" icon="database">Policies belong to a repository. Add one first.</EmptyState>
    <template v-else>
      <Tabs v-model="tab" :tabs="tabs" label="Policies and assignments" />

      <template v-if="tab === 'policies'">
        <Callout v-if="pols.error.value && !pols.data.value" status="critical" title="Could not load the policies" :retry="pols.refresh">{{ pols.error.value.message }}</Callout>
        <Callout v-if="unknownPinned.length" status="warn" title="Objects pinned to a policy that does not exist" class="mb">
          {{ plural(unknownPinned.length, 'object') }} ({{ [...new Set(unknownPinned.map((e) => e.policy))].join(', ') }}) keep every snapshot because their policy is unknown. Create the policy, or assign another one on the Assignments tab.
        </Callout>
        <div v-if="pols.loading.value && !pols.data.value" class="cards"><div v-for="i in 3" :key="i" class="card panel"><Skeleton :lines="5" height="14px" /></div></div>
        <EmptyState v-else-if="pols.data.value !== undefined && !rows.length" title="No policy yet" icon="calendar-clock">
          Hosts back up with the policies of their own configuration until the server has some. Create one to control schedule and retention from here.
          <template v-if="hasRole('operator')" #actions><RouterLink :to="{ name: 'policy-new' }" class="btn btn-primary btn-sm"><Icon name="plus" :size="14" />New policy</RouterLink></template>
        </EmptyState>
        <div v-else class="cards" data-testid="policy-cards">
          <RouterLink v-for="p in rows" :key="p.name" :to="{ name: 'policy', params: { name: p.name } }" class="card panel" :data-policy="p.name">
            <div class="ch"><span class="name">{{ p.name }}</span><span class="spacer" /><StatusPill :status="p.schedule === 'manual' ? 'neutral' : 'ok'" :label="p.schedule" size="sm" variant="plain" /></div>
            <p class="sched"><Icon name="calendar-clock" :size="14" />{{ describeSchedule(p.schedule) }}</p>
            <p class="ret">{{ retentionSummary(rulesOf(p)) }}</p>
            <div v-if="ruleBars(p).length" class="bars" aria-hidden="true">
              <span v-for="b in ruleBars(p)" :key="b.k" class="bar" :class="`r-${b.k}`" :style="{ flexGrow: Math.log2(b.n + 1) }">{{ b.n }} {{ b.k }}</span>
            </div>
            <dl class="facts">
              <div><dt>Targets</dt><dd>{{ p.targets?.length ? p.targets.join(', ') : 'every server the host knows' }}<span v-if="(p.targets?.length ?? 0) > 1" class="muted"> · commit {{ p.commit_required }}</span></dd></div>
              <div><dt>Periods in</dt><dd>{{ p.timezone || 'UTC' }}</dd></div>
              <div><dt>Verify</dt><dd>{{ describeSchedule(p.verify_schedule) }}{{ p.reverify_days ? ` · re-verify after ${p.reverify_days} d` : '' }}</dd></div>
              <div><dt>Restore test</dt><dd>{{ describeSchedule(p.restore_test_schedule) }}</dd></div>
            </dl>
            <div class="foot">
              <span><b class="num">{{ assignCount(p) }}</b> {{ assignCount(p) === 1 ? 'assignment' : 'assignments' }}</span>
              <span><b class="num">{{ objects(p).length }}</b> {{ objects(p).length === 1 ? 'object' : 'objects' }} governed</span>
              <span class="spacer" /><span class="open">Open<Icon name="chevron-right" :size="13" /></span>
            </div>
          </RouterLink>
        </div>
        <p v-if="noPolicy.length" class="muted small note"><Icon name="info" :size="13" /> {{ plural(noPolicy.length, 'object') }} have no policy at all and keep every snapshot.</p>
      </template>

      <section v-else class="panel pad">
        <AssignmentMatrix :policies="names" />
      </section>
    </template>
  </div>
</template>

<style scoped>
.mb { margin-bottom: var(--s-4); }
.pad { padding: var(--s-4); }
.cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 360px), 1fr)); gap: var(--s-4); }
.card { display: flex; flex-direction: column; gap: 8px; padding: 14px 16px; color: inherit; text-decoration: none; min-width: 0; }
.card:hover { border-color: var(--border-strong); background: var(--surface-2); text-decoration: none; }
.ch { display: flex; align-items: center; gap: 8px; }
.name { font-size: var(--fs-lg); font-weight: var(--fw-semibold); }
.sched { display: flex; gap: 6px; align-items: center; color: var(--text-2); }
.ret { font-weight: var(--fw-medium); }
.bars { display: flex; gap: 3px; height: 20px; }
.bar { display: flex; align-items: center; justify-content: center; min-width: 52px; border-radius: var(--r-sm); font-size: var(--fs-xs); color: var(--on-accent); white-space: nowrap; overflow: hidden; padding: 0 4px; }
.r-last, .r-hourly { background: var(--series-1); } .r-daily { background: var(--series-2); } .r-weekly { background: var(--series-3); } .r-monthly { background: var(--series-4); } .r-yearly { background: var(--series-other); color: var(--text); }
.facts { margin: 0; display: grid; gap: 3px; font-size: var(--fs-sm); }
.facts div { display: grid; grid-template-columns: 96px 1fr; gap: 8px; }
.facts dt { color: var(--text-muted); }
.facts dd { margin: 0; min-width: 0; overflow-wrap: anywhere; }
.foot { display: flex; gap: 14px; align-items: center; border-top: 1px solid var(--border); padding-top: 8px; margin-top: auto; font-size: var(--fs-sm); color: var(--text-2); }
.open { display: inline-flex; align-items: center; color: var(--accent-text); }
.note { margin-top: var(--s-3); display: flex; gap: 6px; align-items: center; }
</style>
