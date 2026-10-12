import { describe, expect, it } from 'vitest'
import { svgInner } from './svgInner'

describe('svgInner', () => {
  it('returns what is between the svg tags and leaves the licence comment out', () => {
    const raw = '<!-- @license lucide-static v1 - ISC -->\n<svg\n  class="lucide"\n  width="24"\n>\n  <path d="M0 0h24v24H0z" />\n</svg>\n'
    expect(svgInner(raw)).toBe('<path d="M0 0h24v24H0z" />')
  })
  it('never lets a comment marker survive from before the tag', () => {
    expect(svgInner('<!-- <!-- x --> --><svg><g/></svg>')).toBe('<g/>')
  })
  it('gives an empty string for something that is not an svg', () => {
    expect(svgInner('<div>x</div>')).toBe('')
    expect(svgInner('<svg>')).toBe('')
    expect(svgInner('')).toBe('')
  })
})
