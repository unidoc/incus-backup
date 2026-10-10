import { describe, expect, it } from 'vitest'
import { fuzzyFilter, fuzzyScore } from './fuzzy'

describe('fuzzy', () => {
  it('matches in order and rejects otherwise', () => {
    expect(fuzzyScore('hst', 'Hosts')).not.toBeNull()
    expect(fuzzyScore('xyz', 'Hosts')).toBeNull()
    expect(fuzzyScore('', 'anything')).toBe(0)
  })
  it('ranks exact > prefix > substring > scattered', () => {
    const items = ['Restore tests', 'Hosts', 'Host web01', 'Snapshots', 'Settings']
    expect(fuzzyFilter(items, 'host', (x) => x)).toEqual(['Hosts', 'Host web01'])
    expect(fuzzyFilter(['xtest', 'a test'], 'te', (x) => x)[0]).toBe('a test') // word start beats mid-word
    expect(fuzzyFilter(['abc', 'a-b-c', 'xabcx'], 'abc', (x) => x)[0]).toBe('abc')
  })
})
