<script setup lang="ts">
// Details of one catalog entry, and an in-page preview of small text files and images. A preview reads
// the WHOLE file through readFile(), so the catalog's SHA-256 is checked before anything is shown.
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import KeyValue from '@/components/KeyValue.vue'
import ByteSize from '@/components/ByteSize.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import Callout from '@/components/Callout.vue'
import CopyField from '@/components/CopyField.vue'
import Spinner from '@/components/Spinner.vue'
import PIcon from './PIcon.vue'
import type { CatalogSnapshot, EntryInfo } from '@/lib/protect/crypto'
import { explainCryptoError, type ExplainedError } from '@/lib/protect/crypto'
import { decodeText, entryIcon, fileType, nsToDate, previewKind, toBase64 } from '@/lib/protect/files'
import { collect, concat } from '@/lib/protect/save'
import { formatNumber } from '@/lib/format'

const props = defineProps<{ entry: EntryInfo; snap: CatalogSnapshot }>()
const emit = defineEmits<{ download: [EntryInfo]; open: [string]; close: [] }>()

const mtime = computed(() => nsToDate(props.entry.mtimeNs))
const ctime = computed(() => nsToDate(props.entry.ctimeNs))
const pk = computed(() => previewKind(props.entry))
const items = computed(() => {
  const e = props.entry
  const it: { key: string; label: string; value?: string | number | null; mono?: boolean }[] = [
    { key: 'type', label: 'Type', value: fileType(e) },
    { key: 'path', label: 'Path', value: '/' + e.path, mono: true },
  ]
  if (e.kind === 'file' || e.kind === 'hardlink') it.push({ key: 'size', label: 'Size', value: e.size })
  if (e.kind === 'dir') it.push({ key: 'children', label: 'Entries', value: formatNumber(e.children ?? 0) })
  if (e.link) it.push({ key: 'link', label: e.kind === 'hardlink' ? 'Same content as' : 'Points to', value: e.link, mono: true })
  if (e.kind === 'char' || e.kind === 'block') it.push({ key: 'dev', label: 'Device', value: `${e.major ?? 0}, ${e.minor ?? 0}` })
  it.push(
    { key: 'mode', label: 'Mode', value: `${e.modeString}  (${(e.mode & 0o7777).toString(8).padStart(4, '0')})`, mono: true },
    { key: 'owner', label: 'Owner', value: `uid ${e.uid}, gid ${e.gid}`, mono: true },
    { key: 'mtime', label: 'Modified' },
    { key: 'ctime', label: 'Changed' },
    { key: 'inode', label: 'Inode', value: e.inode, mono: true },
  )
  return it
})

// ---- preview
const busy = ref(false)
const err = ref<ExplainedError | null>(null)
const text = ref<string | null>(null)
const binary = ref(false)
const image = ref<string | null>(null)
let ctl: AbortController | null = null
function reset() {
  ctl?.abort(); ctl = null
  busy.value = false; err.value = null; text.value = null; binary.value = false; image.value = null
}
watch(() => props.entry.path, reset)
onBeforeUnmount(reset)

async function preview() {
  reset()
  const kind = pk.value
  if (kind.kind === 'none') return
  busy.value = true
  ctl = new AbortController()
  try {
    const parts = await collect(props.snap.readFile(props.entry.path, { signal: ctl.signal }), undefined, ctl.signal)
    const bytes = concat(parts)
    if (kind.kind === 'image') image.value = `data:${kind.mime};base64,${toBase64(bytes)}`
    else {
      const t = decodeText(bytes)
      if (t === null) binary.value = true
      else text.value = t
    }
  } catch (e) {
    err.value = explainCryptoError(e, true)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <aside class="fi" data-testid="file-info" :data-path="entry.path">
    <header class="head">
      <PIcon :name="entryIcon(entry)" :size="18" />
      <h3 class="truncate" :title="entry.name">{{ entry.name || '/' }}</h3>
      <button type="button" class="btn btn-sm btn-ghost btn-icon" aria-label="Close the details" @click="emit('close')"><PIcon name="x" :size="15" /></button>
    </header>
    <div class="acts">
      <button v-if="entry.kind === 'dir'" type="button" class="btn btn-sm" @click="emit('open', entry.path)"><PIcon name="folder-open" :size="14" />Open</button>
      <button v-if="entry.kind === 'file' || entry.kind === 'hardlink' || entry.kind === 'dir'" type="button" class="btn btn-sm btn-primary" data-testid="info-download" @click="emit('download', entry)">
        <PIcon name="download" :size="14" />{{ entry.kind === 'dir' ? 'Download as ZIP' : 'Download' }}
      </button>
      <button v-if="pk.kind !== 'none'" type="button" class="btn btn-sm" :disabled="busy" data-testid="info-preview" @click="preview"><PIcon name="eye" :size="14" />Preview</button>
    </div>
    <KeyValue :items="items">
      <template #value-size><ByteSize :bytes="entry.size" /></template>
      <template #value-mtime><AbsoluteTime v-if="mtime" :time="mtime" seconds /><span v-else>–</span></template>
      <template #value-ctime><AbsoluteTime v-if="ctime" :time="ctime" seconds /><span v-else>–</span></template>
    </KeyValue>
    <div v-if="entry.sha256" class="sum">
      <CopyField :value="entry.sha256" label="SHA-256 (recorded at backup time)" />
    </div>
    <p v-if="entry.xattrs?.length" class="muted small">Extended attributes: {{ entry.xattrs.map((x) => x.name).join(', ') }}</p>
    <p v-if="entry.kind === 'symlink'" class="muted small">A symbolic link is restored as a link; its target is not followed.</p>
    <p v-if="entry.kind === 'char' || entry.kind === 'block' || entry.kind === 'fifo'" class="muted small">Devices and pipes have no content to download; a ZIP leaves them out and says so.</p>

    <div v-if="pk.kind !== 'none'" class="pv">
      <div v-if="busy" class="row muted"><Spinner /> Decrypting and checking the whole file…</div>
      <Callout v-else-if="err" status="critical" :title="err.title">{{ err.detail }}</Callout>
      <p v-else-if="binary" class="muted small">This file is not text; download it instead.</p>
      <pre v-else-if="text !== null" class="text" data-testid="preview-text">{{ text }}</pre>
      <img v-else-if="image" :src="image" :alt="`Preview of ${entry.name}`" class="img" data-testid="preview-image" />
      <p v-if="text !== null || image" class="ok small"><PIcon name="circle-check" :size="13" /> Content matches the recorded SHA-256.</p>
    </div>
    <p v-else-if="entry.kind === 'file'" class="muted small">{{ pk.reason }}</p>
  </aside>
</template>

<style scoped>
.fi { display: flex; flex-direction: column; gap: var(--s-3); min-width: 0; }
.head { display: flex; align-items: center; gap: var(--s-2); }
.head h3 { flex: 1; font-size: var(--fs-md); min-width: 0; }
.acts { display: flex; gap: var(--s-2); flex-wrap: wrap; }
.sum :deep(.val) { font-size: var(--fs-xs); }
.small { font-size: var(--fs-sm); }
.text { max-height: 360px; overflow: auto; font-family: var(--font-mono); font-size: var(--fs-xs); background: var(--code-bg); border: 1px solid var(--border); border-radius: var(--r); padding: var(--s-3); white-space: pre-wrap; overflow-wrap: anywhere; margin: 0; }
.img { max-width: 100%; max-height: 360px; border: 1px solid var(--border); border-radius: var(--r); background: repeating-conic-gradient(var(--surface-2) 0% 25%, var(--surface) 0% 50%) 50% / 16px 16px; }
.ok { color: var(--ok-text); display: flex; gap: 4px; align-items: center; }
</style>
