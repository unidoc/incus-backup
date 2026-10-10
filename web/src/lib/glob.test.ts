import { describe, expect, it } from 'vitest'
import { globMatch, silenceMatches, hasMatcher } from './glob'

describe('globMatch (mirror of notify glob)', () => {
  it('matches everything for "" and "*"', () => {
    expect(globMatch('', 'anything/at all')).toBe(true)
    expect(globMatch('*', '')).toBe(true)
  })
  it('is anchored', () => {
    expect(globMatch('node', 'node1')).toBe(false)
    expect(globMatch('node*', 'node1')).toBe(true)
    expect(globMatch('*1', 'node1')).toBe(true)
    expect(globMatch('ode', 'node')).toBe(false)
  })
  it('lets * cross slashes and ? match one character', () => {
    expect(globMatch('default/*', 'default/web/db')).toBe(true)
    expect(globMatch('node?', 'node12')).toBe(false)
    expect(globMatch('node??', 'node12')).toBe(true)
  })
  it('treats regex metacharacters literally', () => {
    expect(globMatch('a.b', 'axb')).toBe(false)
    expect(globMatch('a.b', 'a.b')).toBe(true)
    expect(globMatch('(x)+[y]', '(x)+[y]')).toBe(true)
    expect(globMatch('backup_missed:custody/*', 'backup_missed:custody/0123')).toBe(true)
  })
  it('matches silences on every field', () => {
    const a = { type: 'host_offline', key: 'host_offline:custody/node1', host: 'node1', repository: 'custody', object: '' }
    expect(silenceMatches({ type_glob: 'host_*', key_glob: '', host_glob: 'node1', repo_glob: '', object_glob: '' }, a)).toBe(true)
    expect(silenceMatches({ type_glob: 'host_*', key_glob: '', host_glob: 'node2', repo_glob: '', object_glob: '' }, a)).toBe(false)
    expect(silenceMatches({ type_glob: '', key_glob: '', host_glob: '', repo_glob: '', object_glob: 'x' }, a)).toBe(false)
    expect(hasMatcher({ type_glob: '', key_glob: '', host_glob: '', repo_glob: '', object_glob: '' })).toBe(false)
  })
})
