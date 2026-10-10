import { describe, expect, it } from 'vitest'
import { cleanPath, crumbs, decodeText, entryIcon, fileType, globToRegExp, joinPath, nsToDate, parentOf, previewKind, searchPattern, sortEntries, toBase64, zipName, type Entry } from './files'

const e = (name: string, kind = 'file', size = 0, mtimeNs = '0'): Entry => ({ path: name, name, kind, size, mtimeNs })

describe('paths', () => {
  it('builds breadcrumbs, parents and joins', () => {
    expect(crumbs('')).toEqual([])
    expect(crumbs('a/b/c')).toEqual([{ name: 'a', path: 'a' }, { name: 'b', path: 'a/b' }, { name: 'c', path: 'a/b/c' }])
    expect(parentOf('a/b/c')).toBe('a/b')
    expect(parentOf('a')).toBe('')
    expect(joinPath('', 'x')).toBe('x')
    expect(joinPath('a', 'x')).toBe('a/x')
  })
  it('cleans typed paths and refuses ..', () => {
    expect(cleanPath('/etc//nginx/./')).toBe('etc/nginx')
    expect(cleanPath('')).toBe('')
    expect(cleanPath('a/../b')).toBeInstanceOf(Error)
  })
  it('converts nanosecond strings that do not fit a number', () => {
    expect(nsToDate('1759924980123456789')?.toISOString()).toBe('2025-10-08T12:03:00.123Z')
    expect(nsToDate('')).toBeNull()
    expect(nsToDate('abc')).toBeNull()
  })
})

describe('sorting', () => {
  const list = [e('b.txt', 'file', 5, '3'), e('Zeta', 'dir'), e('a10.txt', 'file', 1, '9'), e('a2.txt', 'file', 9, '1'), e('alpha', 'dir')]
  it('puts directories first and compares names naturally', () => {
    expect(sortEntries(list).map((x) => x.name)).toEqual(['alpha', 'Zeta', 'a2.txt', 'a10.txt', 'b.txt'])
    expect(sortEntries(list, 'name', 'desc').map((x) => x.name)).toEqual(['Zeta', 'alpha', 'b.txt', 'a10.txt', 'a2.txt'])
  })
  it('sorts by size and by mtime (bigint) keeping folders on top', () => {
    expect(sortEntries(list, 'size', 'desc').map((x) => x.name).slice(2)).toEqual(['a2.txt', 'b.txt', 'a10.txt'])
    expect(sortEntries(list, 'mtime').map((x) => x.name).slice(2)).toEqual(['a2.txt', 'b.txt', 'a10.txt'])
    const big = [e('x', 'file', 0, '1759924980123456789'), e('y', 'file', 0, '1759924980123456788')]
    expect(sortEntries(big, 'mtime').map((x) => x.name)).toEqual(['y', 'x']) // differs only below float precision
  })
})

describe('kinds and previews', () => {
  it('names types and icons', () => {
    expect(fileType(e('photo.JPG'))).toBe('JPG image')
    expect(fileType(e('notes.md'))).toBe('MD text')
    expect(fileType(e('README'))).toBe('Text')
    expect(fileType(e('l', 'symlink'))).toBe('Symbolic link')
    expect(entryIcon(e('a.tar.gz'))).toBe('file-archive')
    expect(entryIcon(e('d', 'dir'))).toBe('folder')
    expect(entryIcon(e('x', 'fifo'))).toBe('circle-dashed')
  })
  it('previews small text and images only, whole files only', () => {
    expect(previewKind(e('a.txt', 'file', 100))).toEqual({ kind: 'text' })
    expect(previewKind(e('a.png', 'file', 100))).toEqual({ kind: 'image', mime: 'image/png' })
    expect(previewKind(e('big.log', 'file', 10 << 20)).kind).toBe('none')
    expect(previewKind(e('d', 'dir')).kind).toBe('none')
  })
  it('tells text from binary', () => {
    expect(decodeText(new TextEncoder().encode('Þórsmörk\nferð\t1'))).toBe('Þórsmörk\nferð\t1')
    expect(decodeText(new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0, 1]))).toBeNull()
    expect(decodeText(new Uint8Array([0xff, 0xfe, 0x41]))).toBeNull()
  })
  it('base64-encodes large buffers', () => {
    const b = new Uint8Array(100_000).map((_, i) => i % 251)
    expect(atob(toBase64(b)).length).toBe(100_000)
    expect(toBase64(new TextEncoder().encode('hi'))).toBe('aGk=')
  })
})

describe('glob', () => {
  const m = (g: string, s: string) => { const r = globToRegExp(g); if (r instanceof Error) throw r; return r.test(s) }
  it('matches like Go path.Match on base names', () => {
    expect(m('*.txt', 'a.txt')).toBe(true)
    expect(m('*.txt', 'a.txt.bak')).toBe(false)
    expect(m('a?c', 'abc')).toBe(true)
    expect(m('[a-c]*', 'beta')).toBe(true)
    expect(m('[^a-c]*', 'beta')).toBe(false)
    expect(m('\\*', '*')).toBe(true)
    expect(m('Þórs*', 'Þórsmörk-ferð.txt')).toBe(true)
    expect(m('a.b', 'axb')).toBe(false)
  })
  it('rejects broken patterns and slashes', () => {
    expect(globToRegExp('[abc')).toBeInstanceOf(Error)
    expect(globToRegExp('a/b')).toBeInstanceOf(Error)
    expect(globToRegExp('x\\')).toBeInstanceOf(Error)
  })
  it('turns words into substring searches', () => {
    expect(searchPattern('report')).toBe('*report*')
    expect(searchPattern(' *.pdf ')).toBe('*.pdf')
  })
  it('names zips', () => {
    expect(zipName('nextcloud-data-2026', ['docs/a'])).toBe('a.zip')
    expect(zipName('snap', ['a', 'b'])).toBe('snap.zip')
  })
})
