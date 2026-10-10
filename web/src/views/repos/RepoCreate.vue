<script setup lang="ts">
// Add a repository: create a new one or attach an existing one (disaster recovery, moving servers),
// on a filesystem directory of THIS server or in an S3 bucket.
//
//   1 Kind    create or attach; filesystem or S3; name
//   2 Where   path, or endpoint/bucket/prefix/TLS + credential (stored, or a new access/secret key)
//   3 Test    POST /repositories/check: every check as pass/warn/fail with advice; for attach the
//             owner of the repository and, when another server owns it, the adopt decision
//   4 Add     POST /repositories or /repositories/attach
//
// The secret key lives only in this component: a password field, sent with the test and the create
// request, cleared after a successful add and when leaving the page. The server never returns it.
import { computed, reactive, ref, watch, onBeforeUnmount } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import Stepper from '@/components/Stepper.vue'
import FormField from '@/components/form/FormField.vue'
import PasswordInput from '@/components/form/PasswordInput.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import KeyValue from '@/components/KeyValue.vue'
import CheckList from '@/components/storage/CheckList.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import { api, list } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import { storageApi, type RepoCheckReport, type RepoCheckRequest } from '@/api/sections/storage'
import { loadRepos } from '@/stores/repos'
import { toast, errorText } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import { formatBytes } from '@/lib/format'
import type { CreateRepoRequest } from '@/api/types'

const router = useRouter()
const step = ref(0)
const steps = ['Kind', 'Location', 'Test', 'Add']

const f = reactive({
  purpose: 'create' as 'create' | 'attach',
  kind: 'filesystem' as 'filesystem' | 's3',
  name: '',
  path: '',
  endpoint: '',
  region: '',
  bucket: '',
  prefix: 'incus-backup',
  https: true,
  path_style: true,
  ca_pem: '',
  insecure: false,
  credMode: 'new' as 'new' | 'existing',
  credential: '',
  access_key: '',
  adopt: false,
})
// the secret is kept apart from the reactive form (never in a watcher's logged state, cleared explicitly)
const secret = ref('')
onBeforeUnmount(() => { secret.value = '' })

const creds = useResource(() => 'credentials', (s) => api.credentials.list(s)) // admin page: always readable
const credOptions = computed(() => list(creds.data.value).filter((c) => c.kind === 's3').map((c) => ({ value: c.name, label: `${c.name} (${c.access_key})${c.used_by.length ? ` · used by ${c.used_by.join(', ')}` : ''}` })))
watch(credOptions, (o) => { if (f.credMode === 'existing' && !f.credential && o.length) f.credential = o[0].value })
watch(() => f.credMode, (m) => { if (m === 'existing') { secret.value = ''; f.access_key = ''; if (!f.credential) f.credential = credOptions.value[0]?.value ?? '' } })

const nameRe = /^[A-Za-z0-9][A-Za-z0-9._-]*$/
const nameError = computed(() => (!f.name ? '' : nameRe.test(f.name) ? '' : 'Letters, digits, dot, dash and underscore; start with a letter or digit.'))
const pathError = computed(() => (f.kind !== 'filesystem' || !f.path ? '' : f.path.startsWith('/') ? '' : 'Use an absolute path on the server, starting with /.'))

function request(): RepoCheckRequest {
  const b: RepoCheckRequest = { name: f.name.trim(), kind: f.kind, attach: f.purpose === 'attach' }
  if (f.purpose === 'attach' && f.adopt) b.adopt = true
  if (f.kind === 'filesystem') b.path = f.path.trim()
  else {
    b.s3 = { endpoint: f.endpoint.trim(), bucket: f.bucket.trim(), path_style: f.path_style }
    if (f.region.trim()) b.s3.region = f.region.trim()
    if (f.prefix.trim()) b.s3.prefix = f.prefix.trim()
    if (!f.https) b.s3.no_tls = true
    if (f.https && f.ca_pem.trim()) b.s3.ca_pem = f.ca_pem.trim()
    if (f.https && f.insecure) b.s3.insecure_skip_verify = true
    if (f.credMode === 'existing') b.credential = f.credential
    else { b.access_key = f.access_key.trim(); b.secret_key = secret.value }
  }
  return b
}

const step0Ok = computed(() => !!f.name && !nameError.value)
const step1Ok = computed(() => (f.kind === 'filesystem'
  ? !!f.path && !pathError.value
  : !!f.endpoint.trim() && !!f.bucket.trim() && (f.credMode === 'existing' ? !!f.credential : !!f.access_key.trim() && !!secret.value)))

// ---- test
const report = ref<RepoCheckReport | null>(null)
const testing = ref(false)
const testError = ref('')
const stale = ref(false)
watch(() => [JSON.stringify({ ...f, adopt: undefined }), secret.value], () => { if (report.value) stale.value = true })
async function test() {
  testing.value = true
  testError.value = ''
  try {
    report.value = await storageApi.checkSettings(request())
    stale.value = false
  } catch (e) {
    report.value = null
    testError.value = errorText(e)
  } finally { testing.value = false }
}
async function toTest() { step.value = 2; await test() }
watch(() => f.adopt, () => { if (step.value === 2) void test() })

const owner = computed(() => report.value?.datastore.owner)
const canAdd = computed(() => !!report.value?.ready && !stale.value && !testing.value)

// ---- add
const adding = ref(false)
const addError = ref('')
async function add() {
  if (!canAdd.value) return
  if (f.purpose === 'attach' && f.adopt) {
    const ok = await confirm({ title: `Take over repository ${f.name}?`, danger: true, typeToConfirm: f.name, confirmLabel: 'Adopt and attach',
      message: `It belongs to server ${owner.value?.server_id ?? 'unknown'}${owner.value?.host ? ` on host ${owner.value.host}` : ''}. From now on only THIS server may run GC and retention on it. Make sure the old server is switched off for good.` })
    if (!ok) return
  }
  adding.value = true
  addError.value = ''
  try {
    const b = request() as CreateRepoRequest & { attach?: boolean }
    delete b.attach
    if (f.purpose === 'attach') await api.repositories.attach(b)
    else await api.repositories.create(b)
    secret.value = ''
    toast.success(f.purpose === 'attach' ? `Repository ${f.name} attached` : `Repository ${f.name} created`,
      f.purpose === 'attach' ? 'Run reconcile on its Tasks tab to rebuild the snapshot records from the datastore.' : 'Set its maintenance schedules, then enrol hosts.')
    invalidate('repositories')
    invalidate('credentials')
    await loadRepos()
    void router.push({ name: 'repository', params: { name: f.name }, query: { tab: f.purpose === 'attach' ? 'tasks' : 'schedules' } })
  } catch (e) {
    addError.value = errorText(e)
  } finally { adding.value = false }
}
const summary = computed(() => {
  const r = report.value
  const items = [
    { key: 'name', label: 'Name', value: f.name },
    { key: 'action', label: 'Action', value: f.purpose === 'attach' ? (f.adopt ? 'Attach and adopt (take over from another server)' : 'Attach an existing repository') : 'Create a new repository' },
    { key: 'backend', label: 'Backend', value: f.kind === 's3' ? 'S3' : 'Filesystem' },
    { key: 'location', label: 'Location', value: r?.location ?? '', mono: true },
  ]
  if (f.kind === 's3') items.push({ key: 'cred', label: 'Credential', value: f.credMode === 'existing' ? f.credential : `new: ${f.name}-s3 (access key ${f.access_key})` })
  if (r?.datastore.id) items.push({ key: 'id', label: 'Datastore id', value: r.datastore.id, mono: true })
  if (r?.free_bytes !== undefined) items.push({ key: 'free', label: 'Free space', value: `${formatBytes(r.free_bytes)} of ${formatBytes(r.total_bytes)}` })
  return items
})
</script>

<template>
  <div class="create">
    <header class="page-head">
      <div>
        <h1>Add a repository</h1>
        <p class="lede">Create a new datastore, or attach one that already holds backups (a new server after a disaster, or a repository moved here).</p>
      </div>
      <div class="actions"><RouterLink :to="{ name: 'repositories' }" class="btn">Cancel</RouterLink></div>
    </header>

    <Stepper :steps="steps" :current="step" />

    <!-- 1 kind -->
    <section v-if="step === 0" class="panel" data-testid="step-kind">
      <div class="panel-body form-grid">
        <fieldset class="choice">
          <legend>What do you want to do?</legend>
          <label class="opt" :class="{ on: f.purpose === 'create' }"><input v-model="f.purpose" type="radio" value="create" name="purpose" />
            <span><b>Create a new repository</b><span class="muted">An empty directory or bucket prefix; this server becomes its owner.</span></span></label>
          <label class="opt" :class="{ on: f.purpose === 'attach' }"><input v-model="f.purpose" type="radio" value="attach" name="purpose" data-testid="purpose-attach" />
            <span><b>Attach an existing repository</b><span class="muted">It already holds backups. Afterwards, reconcile rebuilds the snapshot list from it.</span></span></label>
        </fieldset>
        <fieldset class="choice">
          <legend>Where is it stored?</legend>
          <label class="opt" :class="{ on: f.kind === 'filesystem' }"><input v-model="f.kind" type="radio" value="filesystem" name="kind" />
            <span><b><Icon name="hard-drive" :size="14" /> Filesystem</b><span class="muted">A directory on this server (local disk, ZFS dataset, NFS mount).</span></span></label>
          <label class="opt" :class="{ on: f.kind === 's3' }"><input v-model="f.kind" type="radio" value="s3" name="kind" data-testid="kind-s3" />
            <span><b><Icon name="cloud" :size="14" /> S3 object storage</b><span class="muted">Any S3-compatible service: AWS, MinIO, Ceph, Wasabi, Backblaze, R2…</span></span></label>
        </fieldset>
        <FormField v-slot="fl" label="Repository name" :error="nameError" hint="Hosts and the console refer to it by this name. It cannot be changed later." required>
          <input :id="fl.id" v-model="f.name" class="input name" autocomplete="off" spellcheck="false" placeholder="offsite" :aria-invalid="fl.invalid" :aria-describedby="fl.describedby" data-testid="repo-name" />
        </FormField>
        <div class="form-actions"><button type="button" class="btn btn-primary" :disabled="!step0Ok" data-testid="next" @click="step = 1">Next<Icon name="chevron-right" /></button></div>
      </div>
    </section>

    <!-- 2 location -->
    <section v-else-if="step === 1" class="panel" data-testid="step-location">
      <form class="panel-body form-grid" @submit.prevent="step1Ok && toTest()">
        <template v-if="f.kind === 'filesystem'">
          <FormField v-slot="fl" label="Directory on the server" :error="pathError" required
            :hint="f.purpose === 'create' ? 'An empty or missing directory. The service account (incus-backup) must be able to create or write it.' : 'The directory that holds datastore.json.'">
            <input :id="fl.id" v-model="f.path" class="input mono" autocomplete="off" spellcheck="false" placeholder="/srv/incus-backup/offsite" :aria-invalid="fl.invalid" :aria-describedby="fl.describedby" data-testid="repo-path" />
          </FormField>
          <Callout status="info">The path is on the SERVER running incus-backup-server, not on your computer. Use a filesystem with room to grow: free space is checked in the next step.</Callout>
        </template>
        <template v-else>
          <div class="grid2">
            <FormField v-slot="fl" label="Endpoint" required hint="host[:port] or a URL, e.g. s3.eu-central-1.amazonaws.com or minio.lan:9000">
              <input :id="fl.id" v-model="f.endpoint" class="input mono" autocomplete="off" spellcheck="false" placeholder="s3.example.com" data-testid="s3-endpoint" />
            </FormField>
            <FormField v-slot="fl" label="Region" optional hint="Only some services need it (AWS outside us-east-1).">
              <input :id="fl.id" v-model="f.region" class="input mono" autocomplete="off" spellcheck="false" placeholder="eu-central-1" />
            </FormField>
            <FormField v-slot="fl" label="Bucket" required hint="It must exist; the server never creates buckets.">
              <input :id="fl.id" v-model="f.bucket" class="input mono" autocomplete="off" spellcheck="false" placeholder="backups" data-testid="s3-bucket" />
            </FormField>
            <FormField v-slot="fl" label="Prefix" optional hint="A folder inside the bucket; several repositories can share a bucket with different prefixes.">
              <input :id="fl.id" v-model="f.prefix" class="input mono" autocomplete="off" spellcheck="false" placeholder="incus-backup" data-testid="s3-prefix" />
            </FormField>
          </div>
          <div class="switches">
            <SwitchInput v-model="f.https" label="HTTPS" />
            <SwitchInput v-model="f.path_style" label="Path-style addressing (most non-AWS services)" />
          </div>
          <details v-if="f.https" class="adv">
            <summary>TLS options</summary>
            <div class="form-grid">
              <FormField v-slot="fl" label="Private CA certificate (PEM)" optional hint="When the endpoint's certificate comes from your own CA. Public data, stored with the settings.">
                <textarea :id="fl.id" v-model="f.ca_pem" class="textarea mono" rows="4" spellcheck="false" placeholder="-----BEGIN CERTIFICATE-----" />
              </FormField>
              <SwitchInput v-model="f.insecure" label="Do not verify the certificate (last resort)" />
              <Callout v-if="f.insecure" status="warn">Anyone between this server and the endpoint could pose as it. Prefer the CA certificate above.</Callout>
            </div>
          </details>
          <Callout v-if="!f.https" status="warn">Plain HTTP: requests are signed and the data is encrypted, but object names and sizes travel in clear. Use it only on a trusted network.</Callout>

          <fieldset class="choice">
            <legend>Credential</legend>
            <label class="opt" :class="{ on: f.credMode === 'new' }"><input v-model="f.credMode" type="radio" value="new" name="cred" />
              <span><b>Enter an access key</b><span class="muted">Stored sealed on the server as “{{ f.name }}-s3”; the secret is never shown again.</span></span></label>
            <label class="opt" :class="{ on: f.credMode === 'existing', dis: !credOptions.length }"><input v-model="f.credMode" type="radio" value="existing" name="cred" :disabled="!credOptions.length" />
              <span><b>Use a stored credential</b><span class="muted">{{ credOptions.length ? `${credOptions.length} stored` : 'None stored yet' }}</span></span></label>
          </fieldset>
          <div v-if="f.credMode === 'new'" class="grid2">
            <FormField v-slot="fl" label="Access key" required>
              <input :id="fl.id" v-model="f.access_key" class="input mono" autocomplete="off" spellcheck="false" data-testid="s3-access" />
            </FormField>
            <FormField v-slot="fl" label="Secret key" required hint="Kept in this page's memory only until the repository is added.">
              <PasswordInput :id="fl.id" v-model="secret" autocomplete="new-password" name="s3-secret" data-testid="s3-secret" />
            </FormField>
          </div>
          <FormField v-else v-slot="fl" label="Stored credential">
            <SelectInput :id="fl.id" v-model="f.credential" :options="credOptions" />
          </FormField>
        </template>
        <div class="form-actions">
          <button type="button" class="btn" @click="step = 0"><Icon name="chevron-left" />Back</button>
          <button type="submit" class="btn btn-primary" :disabled="!step1Ok" data-testid="test"><Icon name="play" />Test connection</button>
        </div>
      </form>
    </section>

    <!-- 3 test -->
    <section v-else-if="step === 2" class="panel" data-testid="step-test">
      <div class="panel-head"><h2>Connection test</h2><span class="sub">{{ report?.location }}</span><span class="spacer" />
        <button type="button" class="btn btn-sm" :disabled="testing" data-testid="retest" @click="test"><Icon name="refresh-cw" :size="13" />Test again</button></div>
      <div class="panel-body form-grid">
        <div v-if="testing && !report" class="row"><Spinner /> Testing {{ f.kind === 's3' ? 'the bucket (connect, write, conditional write, read, list, delete, bucket settings)' : 'the directory' }}…</div>
        <Callout v-if="testError" status="critical" title="The test could not run" :retry="test">{{ testError }}</Callout>
        <template v-if="report">
          <Callout v-if="stale" status="info">You changed the settings since this test. Test again before adding.</Callout>
          <Callout v-if="report.ready && !stale" status="ok" title="Ready" data-testid="ready">
            {{ f.purpose === 'attach' ? 'This repository can be attached.' : 'The repository can be created here.' }}
            <template v-if="report.checks.some((c) => c.level === 'warn')"> Read the warnings below before you continue.</template>
          </Callout>
          <Callout v-else-if="!stale && !report.needs_adopt" status="critical" title="Not usable yet" data-testid="not-ready">Fix what failed below, then test again.</Callout>

          <div v-if="report.needs_adopt && owner" class="adopt panel" data-testid="adopt">
            <div class="adopt-h"><Icon name="shield-alert" :size="18" /><b>This repository belongs to another server</b></div>
            <KeyValue :items="[
              { key: 'owner', label: 'Owner server', value: owner.server_id, mono: true },
              { key: 'host', label: 'Its host', value: owner.host || 'unknown' },
              { key: 'since', label: 'Owner since', value: owner.since },
              { key: 'me', label: 'This server', value: report.server_id, mono: true },
            ]"><template #value-since><AbsoluteTime :time="owner.since" /></template></KeyValue>
            <p class="txt">Only one server may own a repository: two servers running garbage collection and retention on the same data delete each other's chunks and snapshots. Adopt it only when the other server is gone for good (disaster recovery) or you removed the repository there. The takeover is recorded in owner.json.</p>
            <label class="ack"><input v-model="f.adopt" type="checkbox" data-testid="adopt-check" /> The other server is switched off for good: take over this repository</label>
          </div>

          <CheckList :checks="report.checks" :kind="f.kind" />
          <div class="form-actions">
            <button type="button" class="btn" @click="step = 1"><Icon name="chevron-left" />Back</button>
            <button type="button" class="btn btn-primary" :disabled="!canAdd" data-testid="next" @click="step = 3">Next<Icon name="chevron-right" /></button>
          </div>
        </template>
      </div>
    </section>

    <!-- 4 add -->
    <section v-else class="panel" data-testid="step-add">
      <div class="panel-head"><h2>{{ f.purpose === 'attach' ? 'Attach' : 'Create' }} {{ f.name }}</h2></div>
      <div class="panel-body form-grid">
        <KeyValue :items="summary" />
        <Callout v-if="f.purpose === 'attach'" status="info">After attaching, run <b>Reconcile</b> (Tasks tab) to rebuild the snapshot records from the datastore, including protection, notes and verification state. Hosts re-announce object names with their next backup.</Callout>
        <Callout v-if="addError" status="critical" :title="`Could not ${f.purpose === 'attach' ? 'attach' : 'create'} the repository`">{{ addError }} Go back to the test step to see which check fails now.</Callout>
        <div class="form-actions">
          <button type="button" class="btn" :disabled="adding" @click="step = 2"><Icon name="chevron-left" />Back</button>
          <button type="button" class="btn btn-primary" :disabled="adding || !canAdd" data-testid="add" @click="add"><Spinner v-if="adding" :size="13" />{{ f.purpose === 'attach' ? 'Attach repository' : 'Create repository' }}</button>
        </div>
      </div>
    </section>
  </div>
</template>

<style scoped>
.create { max-width: 880px; }
.choice { border: 0; margin: 0; padding: 0; display: grid; grid-template-columns: 1fr 1fr; gap: 8px; }
.choice legend { font-size: var(--fs-sm); font-weight: var(--fw-medium); color: var(--text-2); margin-bottom: 6px; }
.opt { display: flex; gap: 10px; align-items: flex-start; padding: 10px 12px; border: 1px solid var(--border-strong); border-radius: var(--r); cursor: pointer; background: var(--surface); }
.opt:hover { background: var(--surface-2); }
.opt.on { border-color: var(--accent); box-shadow: 0 0 0 1px var(--accent) inset; background: var(--accent-soft); }
.opt.dis { opacity: .6; cursor: not-allowed; }
.opt input { margin-top: 3px; accent-color: var(--accent); }
.opt > span { display: flex; flex-direction: column; gap: 2px; }
.opt b { display: inline-flex; align-items: center; gap: 6px; font-weight: var(--fw-semibold); }
.opt .muted { font-size: var(--fs-sm); }
.name { max-width: 320px; }
.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: var(--s-4); }
.switches { display: flex; gap: 24px; flex-wrap: wrap; }
.adv summary { cursor: pointer; color: var(--text-2); font-weight: var(--fw-medium); margin-bottom: 8px; }
.adopt { padding: 12px 14px; border-color: var(--critical-border); background: color-mix(in srgb, var(--critical-bg) 50%, var(--surface)); display: flex; flex-direction: column; gap: 10px; }
.adopt-h { display: flex; gap: 8px; align-items: center; color: var(--critical-text); }
.txt { color: var(--text-2); font-size: var(--fs-sm); }
.ack { display: flex; gap: 8px; align-items: center; font-weight: var(--fw-medium); }
.ack input { accent-color: var(--critical); }
@media (max-width: 720px) { .choice, .grid2 { grid-template-columns: 1fr; } }
</style>
