<script setup lang="ts">
// Policy assignments as a tree: each backup host's default, then per project, then per object, with
// the policy that really governs each object (GET /policies/effective, the server's own resolution:
// object > project > host default > the policy pinned at the object's first backup). Changing a select
// saves at once (PUT /assignments; "inherit" deletes the assignment).
import { computed, reactive, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import StatusPill from '@/components/StatusPill.vue'
import Callout from '@/components/Callout.vue'
import EmptyState from '@/components/EmptyState.vue'
import Skeleton from '@/components/Skeleton.vue'
import Spinner from '@/components/Spinner.vue'
import { api, list } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import { storageApi } from '@/api/sections/storage'
import { repos } from '@/stores/repos'
import { hasRole } from '@/stores/session'
import { toast } from '@/stores/toast'
import { assignmentTree, sourceLabel, type HostNode, type OwnerNode, type ProjectNode } from '@/lib/policy'
import { plural } from '@/lib/format'

const props = defineProps<{ policies: string[]; focus?: string }>()
const emit = defineEmits<{ changed: [] }>()
const key = () => repos.current
const hosts = useResource(() => `hosts:${key()}`, (s) => api.hosts.list(s), { refreshOn: ['host'] })
const eff = useResource(() => `effective:${key()}`, (s) => storageApi.effectivePolicies(s), { refreshOn: ['run', 'host'] })
const asg = useResource(() => `assignments:${key()}`, (s) => api.assignments.list(s))
const editable = computed(() => hasRole('operator'))
const q = ref('')
const open = reactive<Record<string, boolean>>({})
const busy = ref('')

const tree = computed<HostNode[]>(() => {
  const hs = list(hosts.data.value).filter((h) => h.scope === 'backup').map((h) => h.name)
  return assignmentTree(hs, list(eff.data.value), list(asg.data.value))
})
watch(tree, (t) => { for (const h of t) if (open[h.host] === undefined) open[h.host] = t.length <= 3 }, { immediate: true })

const match = (s: string) => !q.value || s.toLowerCase().includes(q.value.toLowerCase())
const shownProjects = (h: HostNode) => h.projects
  .map((p) => ({ ...p, owners: p.owners.filter((o) => match(o.owner) || match(p.project) || match(h.host) || match(o.policy)) }))
  .filter((p) => p.owners.length || match(p.project) || match(h.host))
const shownHosts = computed(() => tree.value.filter((h) => match(h.host) || shownProjects(h).length))
const summary = (h: HostNode) => {
  const c: Record<string, number> = {}
  for (const p of h.projects) for (const o of p.owners) for (const _ of o.entries) c[o.policy] = (c[o.policy] ?? 0) + 1
  return Object.entries(c).sort((a, b) => b[1] - a[1])
}
const inheritProject = (h: HostNode) => (h.assigned ? `host default: ${h.assigned}` : 'pinned per object')
const inheritOwner = (h: HostNode, p: ProjectNode) => (p.assigned ? `project: ${p.assigned}` : h.assigned ? `host default: ${h.assigned}` : 'pinned at first backup')

async function set(host: string, project: string, object: string, policy: string, before: string) {
  if (policy === before) return
  const where = [host, project || '(all projects)', object || (project ? '(whole project)' : '')].filter(Boolean).join(' / ')
  busy.value = `${host}/${project}/${object}`
  try {
    if (policy) await api.assignments.put({ host, project, object, policy })
    else await api.assignments.remove({ host, project, object, policy: '' })
    toast.success(policy ? 'Assignment saved' : 'Assignment removed', policy ? `${where} → ${policy}` : `${where} inherits again`)
    invalidate(`assignments:${key()}`)
    invalidate(`effective:${key()}`)
    await Promise.all([asg.refresh(), eff.refresh()])
    emit('changed')
  } catch (e) {
    toast.error(e, 'Could not change the assignment')
  } finally { busy.value = '' }
}
const kinds = (o: OwnerNode) => [...new Set(o.entries.map((e) => e.kind))].join(', ')
const snaps = (o: OwnerNode) => o.entries.reduce((a, e) => a + e.snapshots, 0)
const isFocus = (p: string) => !!props.focus && p === props.focus
</script>

<template>
  <div class="am" data-testid="assignments">
    <div class="bar">
      <label class="search"><Icon name="search" :size="14" /><span class="sr-only">Filter</span><input v-model="q" type="search" placeholder="Filter hosts, projects, objects, policies" /></label>
      <span class="muted small">Precedence: object › project › host default › the policy pinned at the object's first backup. Unknown or no policy keeps everything.</span>
    </div>
    <Skeleton v-if="(hosts.loading.value || eff.loading.value) && !eff.data.value" :lines="5" height="16px" />
    <Callout v-else-if="eff.error.value && !eff.data.value" status="critical" title="Could not load the assignments" :retry="eff.refresh">{{ eff.error.value.message }}</Callout>
    <EmptyState v-else-if="!tree.length" title="No backup host yet" icon="server">Assignments are per host: enrol a host first.
      <template v-if="hasRole('admin')" #actions><RouterLink :to="{ name: 'host-onboard' }" class="btn btn-sm btn-primary">Add a host</RouterLink></template>
    </EmptyState>
    <div v-else class="tree" role="tree" aria-label="Policy assignments">
      <div v-for="h in shownHosts" :key="h.host" class="host" role="treeitem" :aria-expanded="!!open[h.host]">
        <div class="row lvl0">
          <span class="lead"><button type="button" class="tog" :aria-label="open[h.host] ? `Collapse ${h.host}` : `Expand ${h.host}`" @click="open[h.host] = !open[h.host]"><Icon :name="open[h.host] ? 'chevron-down' : 'chevron-right'" :size="14" /></button>
            <Icon name="server" :size="14" class="mi" /><span class="nm">{{ h.host }}</span></span>
          <span class="what muted small">host default</span>
          <select class="select sm" :value="h.assigned" :disabled="!editable || !!busy" :aria-label="`Default policy of ${h.host}`" :data-testid="`asg-${h.host}`" :class="{ foc: isFocus(h.assigned) }"
            @change="set(h.host, '', '', ($event.target as HTMLSelectElement).value, h.assigned)">
            <option value="">— none (pinned per object) —</option>
            <option v-for="p in policies" :key="p" :value="p">{{ p }}</option>
          </select>
          <span class="eff">
            <span v-for="[p, n] in summary(h)" :key="p" class="cnt" :class="{ foc: isFocus(p) }">{{ p === '' ? 'no policy' : p }} <b class="num">{{ n }}</b></span>
            <span v-if="!h.objects" class="muted small">no objects yet</span>
          </span>
          <span class="sp"><Spinner v-if="busy.startsWith(h.host + '/')" :size="12" /></span>
        </div>
        <template v-if="open[h.host]">
          <template v-for="p in shownProjects(h)" :key="p.project">
            <div class="row lvl1" role="group">
              <span class="lead"><Icon name="folder" :size="14" class="mi" /><span class="nm">{{ p.project }}</span></span>
              <span class="what muted small">project</span>
              <select class="select sm" :value="p.assigned" :disabled="!editable || !!busy" :aria-label="`Policy of project ${p.project} on ${h.host}`" :class="{ foc: isFocus(p.assigned) }"
                @change="set(h.host, p.project, '', ($event.target as HTMLSelectElement).value, p.assigned)">
                <option value="">inherit ({{ inheritProject(h) }})</option>
                <option v-for="x in policies" :key="x" :value="x">{{ x }}</option>
              </select>
              <span class="eff muted small">{{ plural(p.owners.length, 'object') }}</span>
              <span class="sp" />
            </div>
            <div v-for="o in p.owners" :key="o.owner" class="row lvl2" :class="{ hl: isFocus(o.policy) }" :data-owner="`${h.host}/${p.project}/${o.owner}`">
              <span class="lead"><Icon name="box" :size="14" class="mi" /><span class="nm">{{ o.owner }}</span></span>
              <span class="what muted small">{{ kinds(o) || 'not backed up yet' }}</span>
              <select class="select sm" :value="o.assigned" :disabled="!editable || !!busy" :aria-label="`Policy of ${o.owner} in ${p.project} on ${h.host}`" :class="{ foc: isFocus(o.assigned) }"
                @change="set(h.host, p.project, o.owner, ($event.target as HTMLSelectElement).value, o.assigned)">
                <option value="">inherit ({{ inheritOwner(h, p) }})</option>
                <option v-for="x in policies" :key="x" :value="x">{{ x }}</option>
              </select>
              <span class="eff">
                <StatusPill v-if="o.entries.length" :status="o.policy && o.entries[0].policy_known ? (isFocus(o.policy) ? 'info' : 'ok') : 'warn'" :label="o.policy ? o.policy : 'keeps everything'" size="sm" />
                <span v-if="o.entries.length" class="muted small">{{ sourceLabel[o.source] }}{{ o.policy && !o.entries[0].policy_known ? ' · unknown policy: keeps everything' : '' }} · {{ plural(snaps(o), 'snapshot') }}</span>
              </span>
              <span class="sp" />
            </div>
          </template>
          <div v-if="!shownProjects(h).length" class="row lvl1 empty muted small">No objects reported by {{ h.host }} yet.</div>
        </template>
      </div>
    </div>
    <p v-if="!editable" class="muted small"><Icon name="lock" :size="13" /> Changing assignments needs the operator role.</p>
  </div>
</template>

<style scoped>
.am { display: flex; flex-direction: column; gap: 10px; }
.bar { display: flex; gap: 12px; align-items: center; flex-wrap: wrap; }
.search { display: flex; align-items: center; gap: 6px; height: var(--control-h-sm); padding: 0 8px; border: 1px solid var(--border-strong); border-radius: var(--r); background: var(--surface); color: var(--text-muted); width: 320px; max-width: 100%; }
.search input { flex: 1; min-width: 0; border: 0; outline: 0; background: transparent; font-size: var(--fs-sm); color: var(--text); }
.tree { border: 1px solid var(--border); border-radius: var(--r-lg); overflow: hidden; background: var(--surface); }
.host + .host { border-top: 1px solid var(--border-strong); }
.row { display: grid; grid-template-columns: minmax(180px, 1.2fr) minmax(90px, .7fr) minmax(200px, 260px) minmax(0, 2fr) 16px; align-items: center; gap: 10px; min-height: 38px; padding: 3px 12px; border-bottom: 1px solid var(--border); }
.lead { display: flex; align-items: center; gap: 6px; min-width: 0; }
.lvl1 .lead { padding-left: 30px; }
.lvl2 .lead { padding-left: 54px; }
.row.empty { display: block; padding-left: 54px; line-height: 32px; }
.row:last-child { border-bottom: 0; }
.lvl0 { background: var(--surface-2); }
.row.hl { background: color-mix(in srgb, var(--info-bg) 60%, var(--surface)); }
.tog { display: grid; place-items: center; width: 22px; height: 22px; border: 0; background: none; color: var(--text-muted); cursor: pointer; border-radius: var(--r-sm); }
.tog:hover { background: var(--surface-3); }
.mi { color: var(--text-muted); }
.nm { font-weight: var(--fw-medium); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.lvl0 .nm { font-weight: var(--fw-semibold); }
.what { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.select.sm { height: var(--control-h-sm); font-size: var(--fs-sm); padding-top: 0; padding-bottom: 0; }
.select.foc { border-color: var(--info); }
.eff { display: flex; flex-wrap: wrap; gap: 4px 10px; align-items: center; min-width: 0; }
.cnt { font-size: var(--fs-xs); padding: 1px 7px; border-radius: var(--r-pill); background: var(--surface-3); color: var(--text-2); }
.cnt.foc { background: var(--info-bg); color: var(--info-text); }
@media (max-width: 900px) {
  .row { grid-template-columns: 1fr; row-gap: 4px; padding: 6px 12px; }
  .row .what, .row .sp { display: none; }
  .lvl1 .lead, .lvl1 .select, .lvl1 .eff { margin-left: 16px; } .lvl2 .lead, .lvl2 .select, .lvl2 .eff { margin-left: 30px; }
  .lvl1 .lead, .lvl2 .lead { padding-left: 0; }
  .row .select { width: auto; }
}
</style>
