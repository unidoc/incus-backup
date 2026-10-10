<script setup lang="ts">
// Zero-knowledge file browser of one filesystem snapshot. The server hands out ciphertext only; this
// page decrypts in the browser with the recovery key the operator pastes (KeyGate). Flow:
//   key -> open snapshot (manifest + index + catalog, all authenticated) -> browse / search / preview ->
//   download one file (streamed, SHA-256 verified) or a ZIP (several entries, folders).
// Disk (block) and instance snapshots are not file trees here: the page says so and offers the
// host-side job instead. Everything key-related is wiped on lock, on leaving the page and on pagehide.
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, shallowRef, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import Callout from '@/components/Callout.vue'
import EmptyState from '@/components/EmptyState.vue'
import Spinner from '@/components/Spinner.vue'
import ByteSize from '@/components/ByteSize.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import Skeleton from '@/components/Skeleton.vue'
import PIcon from '@/components/protect/PIcon.vue'
import KeyGate from '@/components/protect/KeyGate.vue'
import FileInfo from '@/components/protect/FileInfo.vue'
import { api, list } from '@/api/endpoints'
import { useResource } from '@/api/cache'
import { repos, currentRepo } from '@/stores/repos'
import { toast } from '@/stores/toast'
import { ApiFetcher, explainCryptoError, loadCrypto, wipeCrypto, type CatalogSnapshot, type EntryInfo, type ExplainedError, type FetchProgress, type KeyHandle, type OpenedSnapshot } from '@/lib/protect/crypto'
import { crumbs, entryIcon, fileType, globToRegExp, nsToDate, parentOf, searchPattern, sortEntries, zipName, type SortKey } from '@/lib/protect/files'
import { canStreamToDisk, pickSaveFile, saveStream, STREAM_TO_DISK_ABOVE } from '@/lib/protect/save'
import { formatBytes, formatNumber } from '@/lib/format'
import { noun } from '@/lib/protect/text'
import { kindLabel } from '@/lib/protect/snapshots'

const props = defineProps<{ group: string; snap: string }>()
const router = useRouter()

const refs = useResource(() => `snapshots:${repos.current}:${props.group}`, (s) => api.snapshots.list(props.group, s), { enabled: () => !!repos.current })
const ref0 = computed(() => list(refs.data.value).find((r) => r.snapshot_id === props.snap) ?? null)
// once opened, the name comes from the decrypted manifest (authenticated), not from the server's listing
const objLabel = computed(() => {
  const m = (opened.value as { manifest?: { project?: string; object?: { name?: string } } } | null)?.manifest
  if (m?.object?.name) return `${m.project}/${m.object.name}`
  return ref0.value ? `${ref0.value.project}/${ref0.value.object}` : 'this snapshot'
})
const kind = computed(() => ref0.value?.kind ?? '')
const notFiles = computed(() => kind.value === 'block' || kind.value === 'instance')

// ---- key, reader, snapshot (component memory only)
const key = shallowRef<KeyHandle | null>(null)
const opened = shallowRef<OpenedSnapshot | null>(null)
const cat = computed(() => (opened.value && opened.value.browsable ? (opened.value as CatalogSnapshot) : null))
const openErr = ref<ExplainedError | null>(null)
const opening = ref(false)
const stats = reactive<FetchProgress>({ chunksFetched: 0, bytesFetched: 0, cacheHits: 0, inFlight: 0 })
let reader: { clearCache(): void; openSnapshot(g: string, s: string, o?: { signal?: AbortSignal; expect?: { project: string; kind: string; name: string } }): Promise<OpenedSnapshot> } | null = null
let openCtl: AbortController | null = null

async function onUnlocked(k: KeyHandle) {
  key.value = k
  await openSnap()
}

async function openSnap() {
  const k = key.value
  if (!k) return
  openErr.value = null
  opening.value = true
  openCtl = new AbortController()
  try {
    const lib = await loadCrypto()
    let pending = false
    reader = new lib.RepoReader(k, new ApiFetcher(), {
      concurrency: 6, cacheBytes: 64 << 20,
      onProgress: (p) => { // at most one reactive update per frame
        if (pending) return
        pending = true
        requestAnimationFrame(() => { pending = false; Object.assign(stats, p) })
      },
    })
    // the module refuses the snapshot unless this group IS the object the page names (the listing is the server's word)
    const r0 = ref0.value
    opened.value = await reader.openSnapshot(props.group, props.snap, { signal: openCtl.signal, expect: r0?.project && r0.kind && r0.object ? { project: r0.project, kind: r0.kind, name: r0.object } : undefined })
    if (cat.value) await go('')
  } catch (e) {
    const same = currentRepo.value?.domain_id ? k.domainId === currentRepo.value.domain_id : null
    openErr.value = explainCryptoError(e, same)
  } finally {
    opening.value = false
  }
}

/** Wipes everything: downloads stop, the catalog and the sub-keys inside the module are destroyed. */
async function lock(silent = false) {
  for (const d of downloads) if (d.state === 'running') d.ctl.abort()
  openCtl?.abort()
  const s = cat.value
  opened.value = null
  entries.value = []
  selected.value = new Set()
  info.value = null
  search.value = null
  reader?.clearCache()
  reader = null
  try { await s?.close() } catch { /* already closed */ }
  try { await key.value?.close() } catch { /* already closed */ }
  key.value = null
  await wipeCrypto()
  if (!silent) toast.info('Locked', 'The key and every decrypted catalog were wiped from this page.')
}
const onPageHide = () => { void lock(true) }
onMounted(() => window.addEventListener('pagehide', onPageHide))
onBeforeUnmount(() => { window.removeEventListener('pagehide', onPageHide); void lock(true) })

// ---- listing
const cwd = ref('')
const entries = shallowRef<EntryInfo[]>([])
const listing = ref(false)
const listErr = ref<ExplainedError | null>(null)
const sortKey = ref<SortKey>('name')
const sortDir = ref<'asc' | 'desc'>('asc')
const filter = ref('')
const info = shallowRef<EntryInfo | null>(null)
const selected = ref<Set<string>>(new Set())
const search = ref<{ pattern: string; root: string; results: EntryInfo[]; truncated: boolean } | null>(null)
const SHOW_MAX = 3000
const showAll = ref(false)

async function go(path: string) {
  const s = cat.value
  if (!s) return
  listing.value = true
  listErr.value = null
  search.value = null
  try {
    entries.value = await s.tree.list(path)
    cwd.value = path
    selected.value = new Set()
    filter.value = ''
    showAll.value = false
    info.value = null
  } catch (e) {
    listErr.value = explainCryptoError(e, true)
  } finally {
    listing.value = false
  }
}

function sortBy(k: SortKey) {
  if (sortKey.value === k) sortDir.value = sortDir.value === 'asc' ? 'desc' : 'asc'
  else { sortKey.value = k; sortDir.value = k === 'name' || k === 'kind' ? 'asc' : 'desc' }
}
const filterRe = computed(() => {
  const f = filter.value.trim()
  if (!f) return null
  const r = globToRegExp(searchPattern(f))
  return r instanceof Error ? null : new RegExp(r.source, 'i')
})
const source = computed(() => (search.value ? search.value.results : entries.value))
const shown = computed(() => {
  const re = filterRe.value
  const l = re ? source.value.filter((e) => re.test(e.name)) : source.value
  return sortEntries(l, sortKey.value, sortDir.value)
})
const visible = computed(() => (showAll.value ? shown.value : shown.value.slice(0, SHOW_MAX)))
const ariaSort = (k: SortKey) => (sortKey.value === k ? (sortDir.value === 'asc' ? 'ascending' : 'descending') : 'none')
const sortIcon = computed(() => (sortDir.value === 'asc' ? 'arrow-up' : 'arrow-down'))

function activate(e: EntryInfo) {
  if (e.kind === 'dir') void go(e.path)
  else info.value = e
}
const selectable = (e: EntryInfo) => e.kind !== 'char' && e.kind !== 'block' && e.kind !== 'fifo'
function toggle(p: string) {
  const s = new Set(selected.value)
  s.has(p) ? s.delete(p) : s.add(p)
  selected.value = s
}
const allSelected = computed(() => visible.value.length > 0 && visible.value.filter(selectable).every((e) => selected.value.has(e.path)))
function toggleAll() {
  selected.value = allSelected.value ? new Set() : new Set(visible.value.filter(selectable).map((e) => e.path))
}
const selectedEntries = computed(() => source.value.filter((e) => selected.value.has(e.path)))
const selectedBytes = computed(() => selectedEntries.value.reduce((a, e) => a + (e.kind === 'file' ? e.size : 0), 0))
const selectionHasDirs = computed(() => selectedEntries.value.some((e) => e.kind === 'dir'))
const singleFileSelected = computed(() => selectedEntries.value.length === 1 && selectedEntries.value[0].kind === 'file')

// ---- search (whole subtree, inside the module)
const searchText = ref('')
const searching = ref(false)
const searchErr = ref('')
async function runSearch() {
  const s = cat.value
  const q = searchText.value.trim()
  searchErr.value = ''
  if (!s || !q) { search.value = null; return }
  const pat = searchPattern(q)
  const re = globToRegExp(pat)
  if (re instanceof Error) { searchErr.value = `That pattern does not work: ${re.message}.`; return }
  searching.value = true
  try {
    const paths = await s.tree.find(cwd.value, pat)
    const cap = 1000
    const results = await Promise.all(paths.slice(0, cap).map((p) => s.tree.stat(p)))
    search.value = { pattern: pat, root: cwd.value, results, truncated: paths.length > cap }
    selected.value = new Set()
    info.value = null
  } catch (e) {
    searchErr.value = explainCryptoError(e, true).detail
  } finally {
    searching.value = false
  }
}
function clearSearch() { search.value = null; searchText.value = ''; searchErr.value = '' }

// ---- downloads
interface Download { id: number; name: string; what: string; total: number; done: number; state: 'running' | 'done' | 'failed' | 'cancelled'; error?: ExplainedError; ctl: AbortController; started: number; finished?: number; zip: boolean; skipped: number }
const downloads = reactive<Download[]>([])
let dlSeq = 0

function startDownload(name: string, what: string, total: number, zip: boolean, stream: (d: Download) => ReadableStream<Uint8Array>) {
  // the save picker must be asked for synchronously from the click (user activation)
  const wantsDisk = total > STREAM_TO_DISK_ABOVE && canStreamToDisk()
  const handleP = wantsDisk ? pickSaveFile(name) : Promise.resolve(null)
  downloads.unshift({ id: ++dlSeq, name, what, total, done: 0, state: 'running', ctl: new AbortController(), started: Date.now(), zip, skipped: 0 })
  if (downloads.length > 8) downloads.splice(8)
  const d = downloads[0] // the reactive proxy
  void (async () => {
    try {
      const handle = await handleP
      if (wantsDisk && !handle) { d.state = 'cancelled'; return }
      const n = await saveStream(stream(d), name, { handle, signal: d.ctl.signal, type: zip ? 'application/zip' : 'application/octet-stream', onBytes: zip ? undefined : (b) => { d.done = b } })
      if (!zip) d.done = n
      else d.done = Math.max(d.done, d.total)
      d.state = 'done'
      d.finished = Date.now()
      if (d.skipped) toast.info(`${name}: ${d.skipped} ${noun(d.skipped, 'entry', 'entries')} left out`, 'Devices and pipes have no content to put into a ZIP.')
    } catch (e) {
      const x = explainCryptoError(e, true)
      if (x.kind === 'aborted' || d.ctl.signal.aborted) { d.state = 'cancelled'; return }
      d.state = 'failed'
      d.error = x
      toast.error(new Error(x.detail), `Download of ${name} failed`)
    }
  })()
}

function downloadEntry(e: EntryInfo) {
  const s = cat.value
  if (!s) return
  if (e.kind === 'dir') { void downloadZip([e.path]); return }
  startDownload(e.name, '/' + e.path, e.size, false, (d) => s.readFile(e.path, { signal: d.ctl.signal, onProgress: (done) => { d.done = done } }))
}
async function downloadZip(paths: string[]) {
  const s = cat.value
  if (!s) return
  const name = paths.length === 1 ? zipName('', paths) : s.suggestedFilename(paths, true)
  let total = 0
  for (const p of paths.length ? paths : ['']) {
    try { total += (await s.tree.subtree(p)).bytes } catch { /* sized as it streams */ }
  }
  const what = paths.length === 0 ? 'the whole snapshot' : paths.length === 1 ? '/' + paths[0] : `${paths.length} entries`
  startDownload(name, what, total, true, (d) => s.extractZip(paths, { signal: d.ctl.signal, onProgress: (done, t) => { d.done = done; if (t) d.total = t }, onSkip: () => { d.skipped++ } }))
}
function downloadSelected() {
  const sel = selectedEntries.value
  if (sel.length === 1 && sel[0].kind === 'file') { downloadEntry(sel[0]); return }
  void downloadZip(sel.map((e) => e.path))
}
const rate = (d: Download) => {
  const ms = (d.finished ?? Date.now()) - d.started
  return ms > 500 ? (d.done / ms) * 1000 : 0
}
const dlIcon = (d: Download) => (d.state === 'done' ? 'circle-check' : d.state === 'failed' ? 'shield-x' : d.state === 'cancelled' ? 'ban' : d.zip ? 'file-archive' : 'file')

// ---- header facts
const manifest = computed(() => opened.value?.manifest ?? null)
const tstats = computed(() => cat.value?.tree.stats ?? null)
const restoreLink = computed(() => ({ name: 'restore', query: { group: props.group, snap: props.snap, action: kind.value === 'block' ? 'guest_files' : kind.value === 'instance' ? 'restore_instance' : 'fs_extract' } }))

watch(() => [props.group, props.snap], () => { void lock(true) })
// on narrow screens the details panel sits below the list: bring it into view
watch(info, async (e) => {
  if (!e || window.innerWidth > 1100) return
  await nextTick()
  document.querySelector('[data-testid=file-info]')?.scrollIntoView({ behavior: 'smooth', block: 'start' })
})
watch(() => repos.current, (n, o) => { if (o && n !== o) { void lock(true); void router.push({ name: 'snapshots' }) } })
</script>

<template>
  <div class="browser" data-testid="browser">
    <header class="page-head">
      <div>
        <h1>Browse files</h1>
        <p class="lede">
          <RouterLink :to="{ name: 'snapshot', params: { group, snap } }" class="mono">{{ snap }}</RouterLink>
          <template v-if="ref0"> of <strong>{{ objLabel }}</strong> on {{ ref0.host }}, <AbsoluteTime :time="ref0.created" /> (<RelativeTime :time="ref0.created" />)</template>
        </p>
      </div>
      <div class="actions">
        <span v-if="key" class="unlocked" data-testid="unlocked"><PIcon name="lock-open" :size="14" />Unlocked in this tab</span>
        <button v-if="key" type="button" class="btn" data-testid="lock" @click="lock()"><PIcon name="lock" :size="15" />Lock and wipe the key</button>
      </div>
    </header>

    <Skeleton v-if="refs.loading.value && !ref0" height="180px" />
    <Callout v-else-if="refs.error.value && !ref0" status="critical" title="Could not load the snapshot" :retry="refs.refresh">{{ refs.error.value.message }}</Callout>
    <EmptyState v-else-if="!ref0 && refs.data.value !== undefined" title="This snapshot does not exist in this repository" icon="history">
      It may have been pruned or forgotten, or it belongs to another repository (top bar).
      <template #actions><RouterLink class="btn" :to="{ name: 'snapshots' }">All snapshots</RouterLink></template>
    </EmptyState>

    <!-- disks and instance configuration are not file trees here -->
    <section v-else-if="notFiles" class="panel nofiles" data-testid="not-browsable">
      <div class="panel-body">
        <PIcon :name="kind === 'block' ? 'hard-drive' : 'box'" :size="22" />
        <div>
          <h2>{{ kind === 'block' ? 'A disk snapshot cannot be browsed in the browser' : 'An instance snapshot holds configuration only' }}</h2>
          <p v-if="kind === 'block'">This snapshot is a raw disk image ({{ kindLabel(kind) }}). Reading files out of it needs its partition table and filesystem, and guest filesystems are never mounted on the backup host or in the browser. Instead, the host can boot a disposable recovery VM from the backup, copy the paths you name out of it and deliver them encrypted to this browser.</p>
          <p v-else>It records the instance's configuration and which disks belong to it. Restore it as a whole instance, or browse the filesystem snapshot of the instance's root volume if it is a container.</p>
          <div class="row"><RouterLink class="btn btn-primary" :to="restoreLink" data-testid="offer-job"><PIcon :name="kind === 'block' ? 'file-search' : 'archive-restore'" :size="15" />{{ kind === 'block' ? 'Recover files from this disk…' : 'Restore this instance…' }}</RouterLink></div>
        </div>
      </div>
    </section>

    <template v-else-if="ref0">
      <KeyGate v-if="!key" :repo-domain="currentRepo?.domain_id" :repo-name="repos.current" :object-label="objLabel" @unlocked="onUnlocked" />

      <template v-else>
        <div v-if="opening" class="panel"><div class="panel-body row muted" data-testid="opening"><Spinner /> Fetching and checking the manifest, index and file catalog…</div></div>
        <Callout v-else-if="openErr" status="critical" :title="openErr.title" data-testid="open-error">
          {{ openErr.detail }}
          <template #actions>
            <button type="button" class="btn btn-sm" @click="openSnap">Try again</button>
            <button type="button" class="btn btn-sm" @click="lock()">Use another key</button>
          </template>
        </Callout>
        <section v-else-if="opened && !opened.browsable" class="panel nofiles" data-testid="not-browsable">
          <div class="panel-body">
            <PIcon name="file-archive" :size="22" />
            <div>
              <h2>This snapshot cannot be browsed here</h2>
              <p>The library reports: {{ opened.unsupported }}. Snapshots in the older archive (tar) format have no file catalog. The host can extract the paths you name and deliver them encrypted to this browser.</p>
              <div class="row"><RouterLink class="btn btn-primary" :to="restoreLink"><PIcon name="file-search" :size="15" />Extract files on the host…</RouterLink></div>
            </div>
          </div>
        </section>

        <template v-else-if="cat">
          <div class="summary">
            <span><strong class="num">{{ formatNumber(tstats?.Files ?? 0) }}</strong> files</span>
            <span><strong class="num">{{ formatNumber(tstats?.Dirs ?? 0) }}</strong> folders</span>
            <span v-if="tstats?.Symlinks"><strong class="num">{{ formatNumber(tstats.Symlinks) }}</strong> links</span>
            <span><strong class="num">{{ formatBytes(tstats?.Bytes ?? 0) }}</strong> of content</span>
            <span class="sep" />
            <span class="muted" title="Encrypted bytes this page fetched from the server, chunks decoded, and reads answered from the in-memory cache">
              fetched <strong class="num" data-testid="fetched-bytes">{{ formatBytes(stats.bytesFetched) }}</strong> in {{ formatNumber(stats.chunksFetched) }} {{ noun(stats.chunksFetched, 'chunk') }}, {{ formatNumber(stats.cacheHits) }} cache {{ noun(stats.cacheHits, 'hit') }}<template v-if="stats.inFlight">, {{ stats.inFlight }} in flight</template>
            </span>
            <span v-if="manifest?.meta?.['fs.format']" class="muted">catalog format {{ manifest.meta['fs.format'] }}</span>
          </div>

          <div class="layout" :class="{ withinfo: !!info }">
            <section class="panel files">
              <div class="bar">
                <nav class="crumbs" aria-label="Folder">
                  <button type="button" class="crumb root" :class="{ cur: cwd === '' && !search }" data-testid="crumb-root" @click="go('')"><PIcon name="hard-drive" :size="14" />{{ ref0.object }}</button>
                  <template v-for="c in crumbs(cwd)" :key="c.path">
                    <PIcon name="chevron-right" :size="13" class="chev" />
                    <button type="button" class="crumb" :class="{ cur: c.path === cwd && !search }" @click="go(c.path)">{{ c.name }}</button>
                  </template>
                </nav>
                <form class="find" role="search" @submit.prevent="runSearch">
                  <PIcon name="search" :size="14" class="si" />
                  <input v-model="searchText" class="input" :placeholder="cwd ? `Find in /${cwd} (name or *.glob)` : 'Find anywhere (name or *.glob)'" aria-label="Find files by name" data-testid="find-input" />
                  <button type="submit" class="btn btn-sm" :disabled="searching || !searchText.trim()" data-testid="find-run">{{ searching ? 'Searching…' : 'Find' }}</button>
                </form>
              </div>
              <div class="bar2">
                <button type="button" class="btn btn-sm btn-ghost" :disabled="cwd === '' && !search" @click="search ? clearSearch() : go(parentOf(cwd))"><PIcon name="arrow-up" :size="14" />Up</button>
                <input v-model="filter" class="input input-sm" placeholder="Filter this list" aria-label="Filter this list" />
                <span class="count muted">{{ formatNumber(shown.length) }} {{ noun(shown.length, 'entry', 'entries') }}</span>
                <span class="spacer" />
                <template v-if="selected.size">
                  <span class="muted sel">{{ selected.size }} selected<template v-if="selectedBytes">, <ByteSize :bytes="selectedBytes" /><template v-if="selectionHasDirs"> + folders</template></template></span>
                  <button type="button" class="btn btn-sm btn-primary" data-testid="download-selected" @click="downloadSelected"><PIcon name="download" :size="14" />{{ singleFileSelected ? 'Download' : 'Download as ZIP' }}</button>
                  <button type="button" class="btn btn-sm btn-ghost" @click="selected = new Set()">Clear</button>
                </template>
                <button v-else type="button" class="btn btn-sm" data-testid="download-folder" @click="downloadZip(cwd ? [cwd] : [])"><PIcon name="file-archive" :size="14" />{{ cwd ? 'This folder as ZIP' : 'Everything as ZIP' }}</button>
              </div>
              <Callout v-if="searchErr" status="warn">{{ searchErr }}</Callout>
              <div v-if="search" class="searchnote" data-testid="search-note">
                <span><strong>{{ formatNumber(search.results.length) }}</strong> {{ noun(search.results.length, 'match', 'matches') }} for <code>{{ search.pattern }}</code> in {{ search.root ? '/' + search.root : 'the whole snapshot' }}<template v-if="search.truncated"> (first 1000 shown; narrow the pattern)</template></span>
                <button type="button" class="btn btn-sm btn-ghost" @click="clearSearch"><PIcon name="x" :size="13" />Clear search</button>
              </div>
              <Callout v-if="listErr" status="critical" :title="listErr.title">{{ listErr.detail }}</Callout>
              <div class="tablewrap">
                <table class="ft" data-testid="file-table">
                  <thead>
                    <tr>
                      <th class="ck"><input type="checkbox" :checked="allSelected" :indeterminate="!allSelected && selected.size > 0" aria-label="Select all shown" @change="toggleAll" /></th>
                      <th :aria-sort="ariaSort('name')"><button type="button" class="sorth" @click="sortBy('name')">Name<PIcon v-if="sortKey === 'name'" :name="sortIcon" :size="12" /></button></th>
                      <th class="r" :aria-sort="ariaSort('size')"><button type="button" class="sorth" @click="sortBy('size')">Size<PIcon v-if="sortKey === 'size'" :name="sortIcon" :size="12" /></button></th>
                      <th class="hide-s" :aria-sort="ariaSort('mtime')"><button type="button" class="sorth" @click="sortBy('mtime')">Modified<PIcon v-if="sortKey === 'mtime'" :name="sortIcon" :size="12" /></button></th>
                      <th class="hide-m" :aria-sort="ariaSort('kind')"><button type="button" class="sorth" @click="sortBy('kind')">Type<PIcon v-if="sortKey === 'kind'" :name="sortIcon" :size="12" /></button></th>
                      <th class="hide-m">Mode</th>
                      <th class="act"><span class="sr-only">Actions</span></th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-if="listing"><td colspan="7" class="muted pad"><Spinner /> Reading the catalog…</td></tr>
                    <tr v-else-if="!shown.length"><td colspan="7"><EmptyState compact :title="search ? 'Nothing matches' : filter ? 'Nothing matches the filter' : 'This folder is empty'" icon="folder" /></td></tr>
                    <template v-else>
                      <tr v-for="e in visible" :key="e.path" :class="{ sel: selected.has(e.path), active: info?.path === e.path }" :data-path="e.path" :data-kind="e.kind" @click="info = e" @dblclick="activate(e)">
                        <td class="ck" @click.stop><input type="checkbox" :checked="selected.has(e.path)" :disabled="!selectable(e)" :aria-label="`Select ${e.name}`" @change="toggle(e.path)" /></td>
                        <td class="name">
                          <button type="button" class="nm" :class="{ dir: e.kind === 'dir' }" :title="'/' + e.path" @click.stop="activate(e)">
                            <PIcon :name="entryIcon(e)" :size="16" class="fic" :class="`k-${e.kind}`" />
                            <span class="truncate">{{ search ? e.path : e.name }}</span>
                          </button>
                          <span v-if="e.kind === 'symlink'" class="lnk mono" :title="`Points to ${e.link}`">→ {{ e.link }}</span>
                          <span v-else-if="e.kind === 'hardlink'" class="lnk mono" :title="`Same content as /${e.link}`">= /{{ e.link }}</span>
                        </td>
                        <td class="r num"><ByteSize v-if="e.kind === 'file' || e.kind === 'hardlink'" :bytes="e.size" /><span v-else-if="e.kind === 'dir'" class="muted">{{ formatNumber(e.children ?? 0) }} {{ noun(e.children ?? 0, 'item') }}</span></td>
                        <td class="hide-s nowrap"><AbsoluteTime :time="nsToDate(e.mtimeNs)" /></td>
                        <td class="hide-m muted nowrap">{{ fileType(e) }}</td>
                        <td class="hide-m mono muted nowrap">{{ e.modeString }}</td>
                        <td class="act" @click.stop>
                          <button v-if="selectable(e) && e.kind !== 'symlink'" type="button" class="btn btn-sm btn-ghost btn-icon" :aria-label="e.kind === 'dir' ? `Download ${e.name} as ZIP` : `Download ${e.name}`" :data-testid="`dl-${e.name}`" @click="downloadEntry(e)"><PIcon name="download" :size="15" /></button>
                        </td>
                      </tr>
                    </template>
                    <tr v-if="!showAll && shown.length > SHOW_MAX"><td colspan="7" class="pad"><button type="button" class="btn btn-sm" @click="showAll = true">Show all {{ formatNumber(shown.length) }} entries</button></td></tr>
                  </tbody>
                </table>
              </div>
            </section>
            <section v-if="info" class="panel side"><div class="panel-body"><FileInfo :entry="info" :snap="cat" @download="downloadEntry" @open="go" @close="info = null" /></div></section>
          </div>

          <section v-if="downloads.length" class="panel dls" data-testid="downloads">
            <header class="panel-head"><PIcon name="hard-drive-download" /><h2>Downloads</h2><span class="sub">decrypted and verified in this browser; saved only when complete</span></header>
            <ul>
              <li v-for="d in downloads" :key="d.id" :data-state="d.state" :data-name="d.name" data-testid="download">
                <PIcon :name="dlIcon(d)" :size="16" :class="`st-${d.state}`" />
                <div class="dl-main">
                  <div class="dl-top"><strong class="truncate">{{ d.name }}</strong><span class="muted truncate">{{ d.what }}</span></div>
                  <div class="dl-bar"><div :style="{ width: `${d.state === 'done' ? 100 : d.total ? Math.min(100, (d.done / d.total) * 100) : 5}%` }" :class="`st-${d.state}`" /></div>
                  <div class="dl-facts muted">
                    <span><ByteSize :bytes="d.done" /><template v-if="d.total"> of <ByteSize :bytes="d.total" /></template></span>
                    <span v-if="rate(d)">{{ formatBytes(rate(d)) }}/s</span>
                    <span v-if="d.state === 'done'" class="okt">verified, saved</span>
                    <span v-else-if="d.state === 'cancelled'">cancelled, nothing saved</span>
                    <span v-else-if="d.state === 'failed'" class="badt">{{ d.error?.title }}</span>
                  </div>
                  <p v-if="d.state === 'failed' && d.error" class="dl-err">{{ d.error.detail }}</p>
                </div>
                <button v-if="d.state === 'running'" type="button" class="btn btn-sm" @click="d.ctl.abort()">Cancel</button>
              </li>
            </ul>
          </section>
        </template>
      </template>
    </template>
  </div>
</template>

<style scoped>
.unlocked { display: inline-flex; align-items: center; gap: 6px; color: var(--ok-text); background: var(--ok-bg); border: 1px solid var(--ok-border); border-radius: var(--r-pill); padding: 4px 10px; font-size: var(--fs-sm); font-weight: var(--fw-medium); }
.mono { font-family: var(--font-mono); }
.nofiles .panel-body { display: grid; grid-template-columns: 28px 1fr; gap: var(--s-3); padding: var(--s-6); }
.nofiles h2 { font-size: var(--fs-lg); margin-bottom: var(--s-2); }
.nofiles p { color: var(--text-2); max-width: 75ch; margin-bottom: var(--s-4); }
.summary { display: flex; flex-wrap: wrap; gap: var(--s-2) var(--s-5); align-items: baseline; margin-bottom: var(--s-3); font-size: var(--fs-sm); color: var(--text-2); }
.summary .sep { flex: 1; }
.layout { display: grid; grid-template-columns: minmax(0, 1fr); gap: var(--s-4); align-items: start; }
.layout.withinfo { grid-template-columns: minmax(0, 1fr) 340px; }
@media (max-width: 1100px) { .layout.withinfo { grid-template-columns: minmax(0, 1fr); } }
.side { position: sticky; top: calc(var(--topbar-h) + var(--s-3)); }
.bar { display: flex; gap: var(--s-3); align-items: center; padding: var(--s-2) var(--s-3); border-bottom: 1px solid var(--border); flex-wrap: wrap; }
.crumbs { display: flex; align-items: center; gap: 2px; flex-wrap: wrap; min-width: 0; flex: 1; }
.crumb { border: 0; background: none; color: var(--accent-text); padding: 3px 6px; border-radius: var(--r-sm); cursor: pointer; font: inherit; display: inline-flex; gap: 6px; align-items: center; max-width: 28ch; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.crumb:hover { background: var(--surface-3); }
.crumb.cur { color: var(--text); font-weight: var(--fw-semibold); }
.chev { color: var(--text-faint); }
.find { display: flex; gap: var(--s-1); align-items: center; position: relative; flex: 0 1 360px; min-width: 220px; }
.find .si { position: absolute; left: 9px; color: var(--text-faint); pointer-events: none; }
.find .input { padding-left: 28px; flex: 1; min-width: 0; }
.bar2 { display: flex; gap: var(--s-2); align-items: center; padding: var(--s-2) var(--s-3); border-bottom: 1px solid var(--border); flex-wrap: wrap; }
.bar2 .input-sm { height: var(--control-h-sm); width: 200px; max-width: 40vw; }
.count, .sel { font-size: var(--fs-sm); }
.spacer { flex: 1; }
.searchnote { display: flex; justify-content: space-between; align-items: center; gap: var(--s-2); padding: var(--s-2) var(--s-3); background: var(--info-bg); border-bottom: 1px solid var(--info-border); font-size: var(--fs-sm); flex-wrap: wrap; }
.tablewrap { overflow: auto; max-height: min(68vh, 760px); }
.ft { width: 100%; border-collapse: collapse; font-size: var(--fs-md); }
.ft th { position: sticky; top: 0; background: var(--surface-2); text-align: left; font-weight: var(--fw-medium); color: var(--text-2); font-size: var(--fs-sm); border-bottom: 1px solid var(--border); padding: 0 var(--s-2); height: 34px; z-index: 1; }
.ft td { padding: 0 var(--s-2); height: 36px; border-bottom: 1px solid var(--border); vertical-align: middle; }
.ft tbody tr:hover { background: var(--surface-3); }
.ft tbody tr.sel { background: var(--accent-soft); }
.ft tbody tr.active { box-shadow: inset 3px 0 0 var(--accent); }
.ft .r { text-align: right; }
.ft .ck { width: 32px; text-align: center; }
.ft .act { width: 40px; text-align: right; }
.sorth { border: 0; background: none; font: inherit; color: inherit; cursor: pointer; display: inline-flex; gap: 4px; align-items: center; padding: 0; }
.name { min-width: 0; max-width: 0; width: 50%; }
.nm { border: 0; background: none; font: inherit; color: var(--text); display: inline-flex; align-items: center; gap: 8px; cursor: pointer; padding: 0; max-width: 100%; text-align: left; }
.nm.dir { font-weight: var(--fw-medium); }
.nm:hover .truncate { text-decoration: underline; }
.fic { color: var(--text-muted); }
.fic.k-dir { color: var(--accent); }
.lnk { display: block; color: var(--text-muted); font-size: var(--fs-xs); margin-left: 24px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.pad { padding: var(--s-3) var(--s-2); }
@media (max-width: 900px) { .hide-m { display: none; } }
@media (max-width: 560px) { .hide-s { display: none; } .find { flex-basis: 100%; } }
.dls { margin-top: var(--s-4); }
.dls ul { list-style: none; margin: 0; padding: 0; }
.dls li { display: flex; gap: var(--s-3); align-items: center; padding: var(--s-3) var(--s-4); border-bottom: 1px solid var(--border); }
.dls li:last-child { border-bottom: 0; }
.dl-main { flex: 1; min-width: 0; }
.dl-top { display: flex; gap: var(--s-2); align-items: baseline; min-width: 0; }
.dl-top .muted { font-size: var(--fs-sm); }
.dl-bar { height: 6px; background: var(--surface-3); border-radius: var(--r-pill); overflow: hidden; margin: 6px 0 4px; }
.dl-bar > div { height: 100%; background: var(--accent); transition: width var(--t) var(--ease); }
.dl-bar > .st-done { background: var(--ok); }
.dl-bar > .st-failed { background: var(--critical); }
.dl-bar > .st-cancelled { background: var(--neutral); }
.dl-facts { display: flex; gap: var(--s-3); font-size: var(--fs-sm); flex-wrap: wrap; }
.okt { color: var(--ok-text); }
.badt { color: var(--critical-text); font-weight: var(--fw-medium); }
.dl-err { font-size: var(--fs-sm); color: var(--critical-text); margin-top: 4px; }
.st-done { color: var(--ok); }
.st-failed { color: var(--critical); }
.st-cancelled { color: var(--text-muted); }
</style>
