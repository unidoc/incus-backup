/**
 * The inner markup of a bundled lucide SVG: everything between the opening <svg ...> tag and the closing
 * </svg>. The files carry a licence comment before the tag, which this leaves out by position instead of by
 * pattern-matching comment markers. The input is the build-time `?raw` import of a package file, never user data.
 */
export function svgInner(svg: string): string {
  const open = svg.indexOf('<svg')
  const start = open < 0 ? -1 : svg.indexOf('>', open)
  const end = svg.lastIndexOf('</svg>')
  if (start < 0 || end < start) return ''
  return svg.slice(start + 1, end).trim()
}
