<script setup lang="ts">
// Danger zone: every action on this repository that stops service or deletes data, in one place.
// Detach (forget it here, keep the data) and destroy (delete every backup in it) are done here; both
// need the repository offline and every enrolled host named, and destroy a typed confirmation. The
// destroy runs as a server task followed live.
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import Modal from '@/components/Modal.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import FormField from '@/components/form/FormField.vue'
import TaskPanel from '@/components/ops/TaskPanel.vue'
import type { RepoApi } from '@/api/sections/storage'
import { opsApi, type DetachResult, type TaskAccepted, type TaskDetail } from '@/api/sections/ops'
import { useResource, invalidate, clearCache } from '@/api/cache'
import { list } from '@/api/endpoints'
import { hasRole } from '@/stores/session'
import { loadRepos } from '@/stores/repos'
import { toast } from '@/stores/toast'
import type { RepoView } from '@/api/types'

const props = defineProps<{ repo: RepoView; api: RepoApi }>()
const emit = defineEmits<{ go: [string] }>()
const router = useRouter()

const admin = computed(() => hasRole('admin'))
const offline = computed(() => props.repo.mode === 'offline')
const hostsRes = useResource(() => `hosts:${props.repo.name}`, (s) => props.api.hosts(s), { enabled: () => admin.value })
const identities = computed(() => [...new Set(list(hostsRes.data.value).map((h) => h.name))].sort())

const dlg = ref<'' | 'detach' | 'destroy'>('')
const ack = ref(false) // "these hosts lose their enrolment"
const clearOwner = ref(true)
const typed = ref('')
const busy = ref(false)
const err = ref('')
const taskId = ref('')
function open(k: 'detach' | 'destroy') {
  dlg.value = k
  ack.value = false
  typed.value = ''
  err.value = ''
  taskId.value = ''
  void hostsRes.refresh()
}
const dlgOpen = computed({ get: () => !!dlg.value, set: (x: boolean) => { if (!x && !busy.value) dlg.value = '' } })
const ready = computed(() => offline.value && (identities.value.length === 0 || ack.value) && (dlg.value === 'detach' || typed.value === props.repo.name))

async function gone(msg: string, detail: string) {
  toast.success(msg, detail)
  clearCache()
  await loadRepos()
  invalidate('')
  await router.push({ name: 'repositories' })
}

async function submit() {
  busy.value = true
  err.value = ''
  try {
    if (dlg.value === 'detach') {
      const r = (await opsApi.removeRepository(props.repo.name, { mode: 'detach', force_hosts: identities.value, clear_owner: clearOwner.value })) as DetachResult
      dlg.value = ''
      await gone(`Repository ${props.repo.name} detached`, `Its datastore at ${r.location} is untouched${r.owner_cleared ? ' and released' : ''}. Attach it again on Repositories → Add.`)
    } else {
      const r = (await opsApi.removeRepository(props.repo.name, { mode: 'destroy', confirm: typed.value, force_hosts: identities.value })) as TaskAccepted
      taskId.value = r.task.id
    }
  } catch (e) {
    err.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
async function destroyed(d: TaskDetail) {
  if (d.state !== 'succeeded') return
  dlg.value = ''
  await gone(`Repository ${props.repo.name} destroyed`, 'Every backup in it was deleted.')
}
</script>

<template>
  <section class="panel danger">
    <div class="panel-head"><Icon name="triangle-alert" :size="16" /><h2>Danger zone</h2><span class="sub">stops service or deletes data</span></div>
    <ul class="list">
      <li>
        <div><b>Take the repository offline</b><p>Refuses every access to the datastore: backups, restores, sync and maintenance stop until it is online again. Hosts defer their backups instead of failing.</p></div>
        <button type="button" class="btn btn-sm" :disabled="!admin" @click="emit('go', 'mode')">Maintenance mode<Icon name="chevron-right" :size="12" /></button>
      </li>
      <li>
        <div><b>Apply retention now</b><p>Removes every snapshot the objects' policies no longer keep. Preview the exact plan first.</p></div>
        <button type="button" class="btn btn-sm" :disabled="!admin" @click="emit('go', 'tasks')">Tasks<Icon name="chevron-right" :size="12" /></button>
      </li>
      <li>
        <div><b>Run garbage collection</b><p>Deletes unreferenced chunks older than the grace period. This is what frees disk or bucket space.</p></div>
        <button type="button" class="btn btn-sm" :disabled="!admin" @click="emit('go', 'tasks')">Tasks<Icon name="chevron-right" :size="12" /></button>
      </li>
      <li v-if="admin">
        <div><b>Clear the key escrow</b><p>Deletes the wrapped master key of this key domain; recovery with the offline escrow key is impossible until a host uploads a new one.</p></div>
        <button type="button" class="btn btn-sm" @click="emit('go', 'escrow')">Escrow<Icon name="chevron-right" :size="12" /></button>
      </li>
      <li>
        <div><b>Detach the repository</b><p>Forgets {{ repo.name }} on this server and leaves its {{ repo.backend === 's3' ? 'bucket prefix' : 'directory' }} untouched: attach it again later, here or on another server. Its hosts lose their enrolment. Needs the repository offline.</p></div>
        <button type="button" class="btn btn-sm" :disabled="!admin || !offline" :title="!admin ? 'Needs the admin role' : !offline ? 'Take the repository offline first' : ''" data-testid="repo-detach" @click="open('detach')"><Icon name="log-out" :size="12" />Detach</button>
      </li>
      <li>
        <div><b>Destroy the repository</b><p>Deletes every backup in it: all objects of the {{ repo.backend === 's3' ? 'bucket prefix' : 'directory' }} {{ repo.location }}, then its records. This cannot be undone. Needs the repository offline and its name typed.</p></div>
        <button type="button" class="btn btn-sm btn-danger" :disabled="!admin || !offline" :title="!admin ? 'Needs the admin role' : !offline ? 'Take the repository offline first' : ''" data-testid="repo-destroy" @click="open('destroy')"><Icon name="trash-2" :size="12" />Destroy</button>
      </li>
    </ul>
    <p v-if="admin && !offline" class="hint small"><Icon name="info" :size="13" /> Detach and destroy are available once the repository is <button type="button" class="link" @click="emit('go', 'mode')">offline</button>.</p>
  </section>

  <Modal v-model:open="dlgOpen" :title="dlg === 'detach' ? `Detach ${repo.name}?` : `Destroy ${repo.name}?`" :description="repo.location" :dismissible="!busy" width="600px">
    <div class="form-grid">
      <template v-if="!taskId">
        <Callout v-if="dlg === 'destroy'" status="critical" title="Every backup in this repository is deleted">
          All {{ repo.backend === 's3' ? 'objects under the bucket prefix' : 'files in the directory' }} {{ repo.location }} are removed, then the repository's records. Copies elsewhere (sync targets) are not touched. There is no undo.
        </Callout>
        <p v-else class="small">The repository disappears from this server; its data stays where it is. To use it again, add it with “Attach an existing repository” (and run reconcile).</p>
        <div v-if="hostsRes.loading.value" class="small muted"><Spinner :size="12" /> Loading the enrolled hosts…</div>
        <template v-else-if="identities.length">
          <p class="small"><b>{{ identities.length }} enrolled {{ identities.length === 1 ? 'identity' : 'identities' }}</b> lose their enrolment and must enrol again elsewhere:</p>
          <ul class="ids"><li v-for="n in identities" :key="n"><code class="mono">{{ n }}</code></li></ul>
          <SwitchInput v-model="ack" :label="`I understand ${identities.length === 1 ? 'this host loses its' : 'these hosts lose their'} enrolment`" data-testid="ack-hosts" />
        </template>
        <SwitchInput v-if="dlg === 'detach'" v-model="clearOwner" label="Release the repository (another server can attach it without --adopt)" />
        <FormField v-if="dlg === 'destroy'" v-slot="f" :label="`Type ${repo.name} to confirm`">
          <input :id="f.id" v-model="typed" class="input" autocomplete="off" spellcheck="false" data-testid="destroy-confirm" />
        </FormField>
        <Callout v-if="!offline" status="warn" title="The repository is not offline">Take it offline on the Maintenance tab first.</Callout>
        <Callout v-if="err" status="critical" :title="dlg === 'detach' ? 'Could not detach' : 'Could not start the destroy'">{{ err }}</Callout>
        <div class="form-actions">
          <button type="button" class="btn" :class="dlg === 'destroy' ? 'btn-danger' : 'btn-primary'" :disabled="!ready || busy" data-testid="remove-submit" @click="submit"><Spinner v-if="busy" :size="13" />{{ dlg === 'detach' ? 'Detach' : 'Destroy every backup' }}</button>
          <button type="button" class="btn" :disabled="busy" @click="dlg = ''">Cancel</button>
        </div>
      </template>
      <template v-else>
        <p class="small">The datastore is being emptied on the server. You can close this; the task goes on and stays on the Tasks page.</p>
        <TaskPanel :id="taskId" log-height="220px" @done="destroyed" />
      </template>
    </div>
  </Modal>
</template>

<style scoped>
.danger { border-color: var(--critical-border); }
.danger .panel-head { color: var(--critical-text); }
.list { list-style: none; margin: 0; padding: 0; }
.list li { display: flex; gap: 16px; align-items: center; justify-content: space-between; padding: 12px 16px; border-bottom: 1px solid var(--border); }
.list p { color: var(--text-2); font-size: var(--fs-sm); margin-top: 2px; max-width: 80ch; overflow-wrap: anywhere; }
.list .btn { flex: none; }
.hint { display: flex; align-items: center; gap: 6px; padding: 10px 16px; color: var(--text-muted); border-top: 1px solid var(--border); }
.link { border: 0; background: none; padding: 0; color: var(--accent-text); font: inherit; cursor: pointer; text-decoration: underline; }
.ids { margin: 0; padding-left: 18px; display: flex; flex-wrap: wrap; gap: 4px 16px; font-size: var(--fs-sm); }
.small { font-size: var(--fs-sm); }
.muted { color: var(--text-muted); }
@media (max-width: 600px) { .list li { flex-direction: column; align-items: flex-start; gap: 8px; } }
</style>
