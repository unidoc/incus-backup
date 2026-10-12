<script setup lang="ts">
// The key step of the zero-knowledge file browser: paste the recovery key or load a text file that
// holds it. The text goes straight into the WebAssembly module (openKey) and the field is cleared;
// the page keeps only the module's key handle. The key's domain id is compared with the repository's
// so a key of another repository is named as such (the cryptography alone cannot tell it from tampering).
import { computed, ref } from 'vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import PIcon from './PIcon.vue'
import { explainCryptoError, findRecoveryKey, loadCrypto, type ExplainedError, type KeyHandle } from '@/lib/protect/crypto'

const props = withDefaults(defineProps<{ repoDomain?: string | null; repoName: string; objectLabel: string; purpose?: 'browse' | 'delivery' }>(), { purpose: 'browse' })
const emit = defineEmits<{ unlocked: [KeyHandle] }>()

const text = ref('')
const show = ref(false)
const busy = ref(false)
const problem = ref<ExplainedError | null>(null)
const mismatch = ref<{ key: string; repo: string } | null>(null)
const fileInput = ref<HTMLInputElement | null>(null)
const ready = computed(() => text.value.trim().length > 0 && !busy.value)

async function unlock() {
  problem.value = null
  mismatch.value = null
  const found = findRecoveryKey(text.value)
  // JavaScript cannot wipe strings; drop every reference the page holds, whatever happens next
  // (also after a failed attempt: a near-miss of the key is still most of the key)
  text.value = ''
  if (!found.key) {
    problem.value = { kind: 'key-format', title: 'No recovery key found', detail: `${found.hint ?? ''} The field was cleared; paste the key again.` }
    return
  }
  busy.value = true
  let key: KeyHandle | null = null
  try {
    const lib = await loadCrypto()
    key = await lib.openKey(found.key)
    if (props.repoDomain && key.domainId !== props.repoDomain) {
      mismatch.value = { key: key.domainId, repo: props.repoDomain }
      await key.close()
      return
    }
    emit('unlocked', key)
  } catch (e) {
    if (key) await key.close().catch(() => undefined)
    const x = explainCryptoError(e)
    problem.value = x.kind === 'key-format' ? { ...x, detail: `${x.detail} The field was cleared; paste the key again.` } : x
  } finally {
    text.value = ''
    busy.value = false
  }
}

async function loadFile(ev: Event) {
  const input = ev.target as HTMLInputElement
  const f = input.files?.[0]
  input.value = ''
  if (!f) return
  if (f.size > 64 * 1024) {
    problem.value = { kind: 'key-format', title: 'That file is too large to be a key file', detail: 'Choose the text file that holds the recovery key (IBR1-...), not a backup or an archive.' }
    return
  }
  text.value = await f.text()
  await unlock()
}
</script>

<template>
  <section class="kg panel" data-testid="key-gate">
    <div class="panel-body grid">
      <div class="intro">
        <span class="badge"><PIcon name="key" :size="20" /></span>
        <h2 v-if="purpose === 'delivery'">Unlock the delivery with the recovery key</h2>
        <h2 v-else>Unlock {{ objectLabel }} in this browser</h2>
        <p v-if="purpose === 'delivery'">The files are encrypted to this tab. The recovery key proves they were made by a host that holds this repository's key, for this job and these paths: a server that relays them cannot forge or swap them.</p>
        <p v-else>The server stores only encrypted data. To browse and download files, this page decrypts them here, with the repository's recovery key.</p>
        <ul class="facts">
          <li><PIcon name="shield-check" :size="15" /><span>The key <strong>never leaves this browser tab</strong>: it is not sent to the server, not stored, not put in the address.</span></li>
          <li><PIcon name="lock" :size="15" /><span>It is kept in memory only, inside the decryption module, and <strong>wiped</strong> when you lock, leave this page or close the tab.</span></li>
          <li v-if="purpose === 'delivery'"><PIcon name="circle-check" :size="15" /><span>The delivery is checked before anything is saved; a forged, altered or incomplete delivery is refused.</span></li>
          <li v-else><PIcon name="circle-check" :size="15" /><span>Every chunk and every downloaded file is checked against the backup's checksums; altered data is refused, never shown.</span></li>
        </ul>
        <p class="muted small">Where to find it: on any host that backs up into <strong>{{ repoName }}</strong>, run <code>incus-backup key recovery</code>. It prints a key that starts with <code>IBR1-</code>.</p>
      </div>
      <form class="form" @submit.prevent="unlock">
        <label for="kg-key" class="lbl">Recovery key</label>
        <div class="keybox">
          <textarea v-if="show" id="kg-key" v-model="text" class="textarea mono" rows="3" spellcheck="false" autocomplete="off" autocapitalize="off" placeholder="IBR1-XXXXX-XXXXX-…" data-testid="key-input" />
          <input v-else id="kg-key" v-model="text" class="input mono" type="password" spellcheck="false" autocomplete="off" placeholder="IBR1-XXXXX-XXXXX-…" data-testid="key-input" />
          <button type="button" class="btn btn-ghost btn-icon" :aria-label="show ? 'Hide the key' : 'Show the key'" @click="show = !show"><PIcon :name="show ? 'eye-off' : 'eye'" /></button>
        </div>
        <div class="row acts">
          <button type="submit" class="btn btn-primary" :disabled="!ready" data-testid="key-unlock">
            <Spinner v-if="busy" :size="13" label="Opening" /><PIcon v-else name="lock-open" :size="15" />Unlock
          </button>
          <button type="button" class="btn" :disabled="busy" @click="fileInput?.click()"><PIcon name="upload" :size="15" />Load a key file</button>
          <input ref="fileInput" type="file" accept=".txt,.key,text/plain" class="sr-only" data-testid="key-file" @change="loadFile" />
        </div>
        <Callout v-if="mismatch" status="critical" title="This key belongs to another repository" data-testid="key-mismatch">
          The key's domain is <code>{{ mismatch.key }}</code>; repository {{ repoName }} uses <code>{{ mismatch.repo }}</code>. Use the recovery key of the hosts that back up into {{ repoName }}. The key was discarded.
        </Callout>
        <Callout v-else-if="problem" status="critical" :title="problem.title" data-testid="key-error">{{ problem.detail }}</Callout>
        <p v-if="repoDomain" class="muted small">Repository key domain: <code data-testid="repo-domain">{{ repoDomain }}</code></p>
        <p v-else class="muted small">The server does not record a key domain for this repository yet, so a key of another repository shows up as "does not authenticate".</p>
      </form>
    </div>
  </section>
</template>

<style scoped>
.grid { display: grid; grid-template-columns: minmax(0, 1.1fr) minmax(0, 1fr); gap: var(--s-8); padding: var(--s-6); }
@media (max-width: 900px) { .grid { grid-template-columns: 1fr; gap: var(--s-5); padding: var(--s-4); } }
.badge { display: inline-grid; place-items: center; width: 40px; height: 40px; border-radius: 50%; background: var(--accent-soft); color: var(--accent-text); margin-bottom: var(--s-3); }
h2 { font-size: var(--fs-lg); margin-bottom: var(--s-2); }
.intro > p { color: var(--text-2); max-width: 60ch; }
.facts { list-style: none; padding: 0; margin: var(--s-4) 0; display: grid; gap: var(--s-3); }
.facts li { display: grid; grid-template-columns: 18px 1fr; gap: var(--s-2); align-items: start; color: var(--text-2); }
.facts li :first-child { color: var(--ok); margin-top: 2px; }
.form { display: flex; flex-direction: column; gap: var(--s-3); align-self: center; }
.lbl { font-weight: var(--fw-medium); }
.keybox { display: flex; gap: var(--s-1); align-items: flex-start; }
.keybox .input, .keybox .textarea { flex: 1; min-width: 0; }
.acts { flex-wrap: wrap; }
.small { font-size: var(--fs-sm); }
code { font-family: var(--font-mono); font-size: 0.92em; overflow-wrap: anywhere; }
</style>
