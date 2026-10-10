// Pure helpers of the snapshot file browser: paths, sorting, kinds, previews, globbing, times.
// Unit-tested in files.test.ts.

/** The subset of web-crypto's EntryInfo these helpers need (kept structural so tests need no module). */
export interface Entry {
  path: string
  name: string
  kind: 'dir' | 'file' | 'symlink' | 'hardlink' | 'char' | 'block' | 'fifo' | string
  size: number
  mtimeNs: string
  link?: string
  children?: number
}

/** "a/b/c" -> [{name:'a', path:'a'}, {name:'b', path:'a/b'}, ...]; "" -> []. */
export function crumbs(path: string): { name: string; path: string }[] {
  const out: { name: string; path: string }[] = []
  let cur = ''
  for (const p of path.split('/').filter(Boolean)) {
    cur = cur ? `${cur}/${p}` : p
    out.push({ name: p, path: cur })
  }
  return out
}

export const parentOf = (path: string) => (path.includes('/') ? path.slice(0, path.lastIndexOf('/')) : '')
export const joinPath = (dir: string, name: string) => (dir ? `${dir}/${name}` : name)

/** Normalises a path typed by a person: no leading/trailing slash, no "." parts; ".." is refused. */
export function cleanPath(p: string): string | Error {
  const parts: string[] = []
  for (const x of p.split('/')) {
    if (x === '' || x === '.') continue
    if (x === '..') return new Error('".." is not allowed in a path')
    parts.push(x)
  }
  return parts.join('/')
}

/** Nanoseconds since the epoch (decimal string) -> Date (millisecond precision). */
export function nsToDate(ns: string | undefined | null): Date | null {
  if (!ns || !/^-?\d+$/.test(ns)) return null
  const ms = Number(BigInt(ns) / 1_000_000n)
  return Number.isFinite(ms) ? new Date(ms) : null
}

export type SortKey = 'name' | 'size' | 'mtime' | 'kind'

/** Directories first (like every file manager), then the chosen key; names compare naturally. */
export function sortEntries<T extends Entry>(list: readonly T[], key: SortKey = 'name', dir: 'asc' | 'desc' = 'asc'): T[] {
  const coll = new Intl.Collator('en', { numeric: true, sensitivity: 'base' })
  const mul = dir === 'asc' ? 1 : -1
  const isDir = (e: Entry) => (e.kind === 'dir' ? 0 : 1)
  return list.slice().sort((a, b) => {
    const d = isDir(a) - isDir(b)
    if (d) return d
    let c = 0
    if (key === 'size') c = a.size - b.size
    else if (key === 'mtime') c = (BigInt(a.mtimeNs || '0') < BigInt(b.mtimeNs || '0') ? -1 : BigInt(a.mtimeNs || '0') > BigInt(b.mtimeNs || '0') ? 1 : 0)
    else if (key === 'kind') c = coll.compare(fileType(a), fileType(b))
    return (c * mul) || coll.compare(a.name, b.name) * (key === 'name' ? mul : 1)
  })
}

const TEXT_EXT = new Set(['txt', 'md', 'log', 'conf', 'cfg', 'ini', 'toml', 'yaml', 'yml', 'json', 'xml', 'csv', 'tsv', 'sh', 'bash', 'zsh', 'py', 'go', 'rs', 'js', 'ts',
  'mjs', 'c', 'h', 'cpp', 'hpp', 'java', 'rb', 'php', 'pl', 'lua', 'sql', 'html', 'htm', 'css', 'scss', 'service', 'timer', 'socket', 'env', 'properties',
  'gitignore', 'dockerfile', 'tf', 'hcl', 'nix', 'vim', 'rst', 'tex', 'srt', 'vtt', 'pem', 'crt', 'pub', 'diff', 'patch', 'list', 'rules', 'repo', 'desktop'])
const TEXT_NAMES = new Set(['readme', 'license', 'makefile', 'dockerfile', 'hosts', 'passwd', 'group', 'fstab', 'crontab', 'hostname', 'resolv.conf', 'motd', 'issue', 'shells', 'profile', 'bashrc', '.bashrc', '.profile'])
const IMAGE_MIME: Record<string, string> = { png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', gif: 'image/gif', webp: 'image/webp', bmp: 'image/bmp', ico: 'image/x-icon', svg: 'image/svg+xml', avif: 'image/avif' }
const ARCHIVE_EXT = new Set(['zip', 'tar', 'gz', 'tgz', 'xz', 'zst', 'bz2', '7z', 'rar', 'deb', 'rpm', 'apk', 'iso', 'qcow2', 'img'])

export const extOf = (name: string) => {
  const i = name.lastIndexOf('.')
  return i > 0 ? name.slice(i + 1).toLowerCase() : ''
}

/** A short human type for the Type column. */
export function fileType(e: Entry): string {
  switch (e.kind) {
    case 'dir': return 'Folder'
    case 'symlink': return 'Symbolic link'
    case 'hardlink': return 'Hard link'
    case 'char': return 'Character device'
    case 'block': return 'Block device'
    case 'fifo': return 'Named pipe'
  }
  const x = extOf(e.name)
  if (IMAGE_MIME[x]) return `${x.toUpperCase()} image`
  if (ARCHIVE_EXT.has(x)) return `${x.toUpperCase()} archive`
  if (TEXT_EXT.has(x) || TEXT_NAMES.has(e.name.toLowerCase())) return x ? `${x.toUpperCase()} text` : 'Text'
  return x ? `${x.toUpperCase()} file` : 'File'
}

export type EntryIcon = 'folder' | 'file' | 'file-text' | 'image' | 'file-archive' | 'link' | 'hard-drive' | 'circle-dashed'

export function entryIcon(e: Entry): EntryIcon {
  if (e.kind === 'dir') return 'folder'
  if (e.kind === 'symlink' || e.kind === 'hardlink') return 'link'
  if (e.kind === 'char' || e.kind === 'block') return 'hard-drive'
  if (e.kind === 'fifo') return 'circle-dashed'
  const x = extOf(e.name)
  if (IMAGE_MIME[x]) return 'image'
  if (ARCHIVE_EXT.has(x)) return 'file-archive'
  if (TEXT_EXT.has(x) || TEXT_NAMES.has(e.name.toLowerCase())) return 'file-text'
  return 'file'
}

/** Limits for in-page previews: the whole file is read (and its SHA-256 checked) before it is shown. */
export const PREVIEW_TEXT_MAX = 512 * 1024
export const PREVIEW_IMAGE_MAX = 8 * 1024 * 1024

export type PreviewKind = { kind: 'text' } | { kind: 'image'; mime: string } | { kind: 'none'; reason: string }

export function previewKind(e: Entry): PreviewKind {
  if (e.kind !== 'file' && e.kind !== 'hardlink') return { kind: 'none', reason: 'Only regular files have a preview.' }
  const x = extOf(e.name)
  if (IMAGE_MIME[x]) {
    if (e.size > PREVIEW_IMAGE_MAX) return { kind: 'none', reason: `Images up to ${PREVIEW_IMAGE_MAX >> 20} MiB are previewed; download this one.` }
    return { kind: 'image', mime: IMAGE_MIME[x] }
  }
  if (e.size > PREVIEW_TEXT_MAX) return { kind: 'none', reason: `Files up to ${PREVIEW_TEXT_MAX >> 10} KiB are previewed; download this one.` }
  return { kind: 'text' }
}

/**
 * Decides whether bytes are text worth showing: valid UTF-8 without NUL bytes and with few control
 * characters. Returns the decoded text or null for binary content.
 */
export function decodeText(b: Uint8Array): string | null {
  if (b.includes(0)) return null
  let text: string
  try { text = new TextDecoder('utf-8', { fatal: true }).decode(b) } catch { return null }
  let ctrl = 0
  for (let i = 0; i < text.length; i++) {
    const c = text.charCodeAt(i)
    if (c < 32 && c !== 9 && c !== 10 && c !== 13 && c !== 12) ctrl++
  }
  return ctrl > Math.max(4, text.length / 100) ? null : text
}

/** base64 of bytes (for data: URLs; the CSP allows img-src data: but not blob:). */
export function toBase64(b: Uint8Array): string {
  let s = ''
  for (let i = 0; i < b.length; i += 0x8000) s += String.fromCharCode(...b.subarray(i, i + 0x8000))
  return btoa(s)
}

/**
 * Glob on base names as Go's path.Match does it (what Tree.find uses): * ? [abc] [a-z] [^x] and \ escapes.
 * Used to check a pattern before sending it to the module and to filter the current listing.
 */
export function globToRegExp(glob: string): RegExp | Error {
  let re = '^'
  for (let i = 0; i < glob.length; i++) {
    const c = glob[i]
    if (c === '*') re += '[^/]*'
    else if (c === '?') re += '[^/]'
    else if (c === '\\') {
      if (i + 1 >= glob.length) return new Error('the pattern ends with a backslash')
      re += escapeRe(glob[++i])
    } else if (c === '[') {
      const end = glob.indexOf(']', i + 2)
      if (end < 0) return new Error('a [ has no closing ]')
      let cls = glob.slice(i + 1, end)
      if (cls.startsWith('^') || cls.startsWith('!')) cls = '^' + cls.slice(1)
      re += `[${cls.replace(/\\/g, '\\\\')}]`
      i = end
    } else re += escapeRe(c)
  }
  if (glob.includes('/')) return new Error('the pattern matches file names: leave out "/"')
  try { return new RegExp(re + '$') } catch (e) { return e as Error }
}
const escapeRe = (c: string) => c.replace(/[.*+?^${}()|[\]\\/]/g, '\\$&')

/** A plain word becomes a substring glob ("report" -> "*report*"); patterns stay as typed. */
export function searchPattern(input: string): string {
  const s = input.trim()
  return /[*?[]/.test(s) ? s : `*${s}*`
}

/** Suggested file name of a download of several paths. */
export function zipName(snapshotName: string, paths: string[]): string {
  if (paths.length === 1) return `${paths[0].split('/').pop() || 'files'}.zip`
  return `${snapshotName}.zip`
}
