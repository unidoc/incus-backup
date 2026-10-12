<script setup lang="ts">
// One repository: identity and health, maintenance mode, schedules, tasks (run now + history),
// integrity, hosts, credentials, escrow and the dangerous actions.
//
// Every call on this page targets THIS repository by name (repoApi(name)), never the repository
// chosen in the top bar: running GC on the wrong repository is not a possible mistake here.
// The tab is in the URL (?tab=tasks) so a link can open it directly.
import { computed, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import Tabs from '@/components/Tabs.vue'
import StatusPill from '@/components/StatusPill.vue'
import Callout from '@/components/Callout.vue'
import EmptyState from '@/components/EmptyState.vue'
import Skeleton from '@/components/Skeleton.vue'
import SummaryTab from './tabs/SummaryTab.vue'
import ModeTab from './tabs/ModeTab.vue'
import SchedulesTab from './tabs/SchedulesTab.vue'
import TasksTab from './tabs/TasksTab.vue'
import IntegrityTab from './tabs/IntegrityTab.vue'
import HostsTab from './tabs/HostsTab.vue'
import CredentialsTab from './tabs/CredentialsTab.vue'
import EscrowTab from './tabs/EscrowTab.vue'
import DangerTab from './tabs/DangerTab.vue'
import { api } from '@/api/endpoints'
import { useResource } from '@/api/cache'
import { repoApi } from '@/api/sections/storage'
import { hasRole } from '@/stores/session'
import { repoHealth } from '@/lib/storage'

const props = defineProps<{ name: string }>()
const route = useRoute()
const router = useRouter()
const r = computed(() => repoApi(props.name))

const repos = useResource(() => 'repositories', (s) => api.repositories.list(s), { refreshOn: ['repository'] })
const ov = useResource(() => 'overview', (s) => api.overview(s), { refreshOn: ['run', 'repository'], poll: 60_000 })
const repo = computed(() => repos.data.value?.find((x) => x.name === props.name))
const o = computed(() => ov.data.value?.repositories.find((x) => x.name === props.name))
const health = computed(() => (repo.value ? repoHealth(o.value, repo.value.mode) : null))
const corrupt = useResource(() => `corrupt:${props.name}`, (s) => r.value.corrupt(false, s), { refreshOn: ['run'] })

const tabs = computed(() => [
  { id: 'summary', label: 'Summary' },
  { id: 'mode', label: 'Maintenance mode' },
  { id: 'schedules', label: 'Schedules' },
  { id: 'tasks', label: 'Tasks' },
  { id: 'integrity', label: 'Integrity', count: (corrupt.data.value?.length ?? 0) + (o.value?.failed_verify_snapshots ?? 0) || undefined },
  { id: 'hosts', label: 'Hosts', count: o.value?.hosts },
  ...(hasRole('admin') ? [{ id: 'credentials', label: 'Credentials' }, { id: 'escrow', label: 'Escrow' }] : []),
  { id: 'danger', label: 'Danger zone' },
])
const tab = computed({
  get: () => {
    const t = String(route.query.tab ?? 'summary')
    return tabs.value.some((x) => x.id === t) ? t : 'summary'
  },
  set: (t: string) => { void router.replace({ query: { ...route.query, tab: t === 'summary' ? undefined : t } }) },
})
watch(() => props.name, () => { if (route.query.tab) void router.replace({ query: {} }) })
const modeLabel = (m: string) => (m === 'online' ? 'Online' : m === 'read-only' ? 'Read-only' : 'Offline')
</script>

<template>
  <div class="detail">
    <div v-if="repos.loading.value && !repos.data.value" class="panel pad"><Skeleton :lines="4" height="16px" /></div>
    <Callout v-else-if="repos.error.value && !repos.data.value" status="critical" title="Could not load the repository" :retry="repos.refresh">{{ repos.error.value.message }}</Callout>
    <EmptyState v-else-if="!repo" title="No such repository" icon="database">
      There is no repository called “{{ name }}” on this server.
      <template #actions><RouterLink :to="{ name: 'repositories' }" class="btn btn-sm">All repositories</RouterLink></template>
    </EmptyState>
    <template v-else>
      <header class="page-head">
        <div>
          <h1 class="h1"><Icon :name="repo.backend === 's3' ? 'cloud' : 'hard-drive'" :size="20" />{{ repo.name }}
            <span class="badge" :class="repo.backend">{{ repo.backend === 's3' ? 'S3' : 'Filesystem' }}</span></h1>
          <p class="lede mono loc" :title="repo.location">{{ repo.location }}</p>
        </div>
        <div class="actions">
          <StatusPill :state="repo.mode" :label="modeLabel(repo.mode)" data-testid="detail-mode" />
          <StatusPill v-if="health" :status="health.status" :label="health.label" />
        </div>
      </header>
      <Callout v-if="repo.mode !== 'online'" :status="repo.mode === 'offline' ? 'critical' : 'warn'" :title="`This repository is ${repo.mode}`" class="mb">
        {{ repo.mode === 'offline' ? 'Backups, restores, sync and maintenance are refused.' : 'Restores and browsing work; backups, sync into it and maintenance that writes are refused.' }}
        <template v-if="repo.mode_reason"> Reason: “{{ repo.mode_reason }}”.</template>
        <template v-if="hasRole('admin')"> Change it on the <a href="#" @click.prevent="tab = 'mode'">Maintenance mode</a> tab.</template>
      </Callout>

      <Tabs v-model="tab" :tabs="tabs" label="Repository sections" />

      <SummaryTab v-if="tab === 'summary'" :repo="repo" :api="r" :overview="o" :health="health" @go="tab = $event" />
      <ModeTab v-else-if="tab === 'mode'" :repo="repo" :api="r" />
      <SchedulesTab v-else-if="tab === 'schedules'" :repo="repo" :api="r" />
      <TasksTab v-else-if="tab === 'tasks'" :repo="repo" :api="r" />
      <IntegrityTab v-else-if="tab === 'integrity'" :repo="repo" :api="r" />
      <HostsTab v-else-if="tab === 'hosts'" :repo="repo" :api="r" />
      <CredentialsTab v-else-if="tab === 'credentials'" :repo="repo" />
      <EscrowTab v-else-if="tab === 'escrow'" :repo="repo" :api="r" />
      <DangerTab v-else-if="tab === 'danger'" :repo="repo" :api="r" @go="tab = $event" />
    </template>
  </div>
</template>

<style scoped>
.pad { padding: var(--s-4); }
.mb { margin-bottom: var(--s-4); }
.h1 { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.h1 .icon { color: var(--text-muted); }
.loc { font-size: var(--fs-sm); overflow-wrap: anywhere; }
.badge { display: inline-flex; align-items: center; height: 20px; padding: 0 7px; border-radius: var(--r-sm); font-size: var(--fs-xs); font-weight: var(--fw-semibold); border: 1px solid var(--border-strong); color: var(--text-2); background: var(--surface-2); }
.badge.s3 { border-color: color-mix(in srgb, var(--series-1) 45%, transparent); color: var(--series-1); background: color-mix(in srgb, var(--series-1) 9%, var(--surface)); }
.actions { align-items: center; }
</style>
