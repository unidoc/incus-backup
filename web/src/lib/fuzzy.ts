// Small fuzzy matcher for the command palette: every query character must appear in order; contiguous
// runs, word starts and prefix matches score higher. Returns null for no match.
export function fuzzyScore(query: string, text: string): number | null {
  const q = query.trim().toLowerCase()
  if (!q) return 0
  const t = text.toLowerCase()
  if (t === q) return 1000
  if (t.startsWith(q)) return 800 - t.length
  const idx = t.indexOf(q)
  if (idx >= 0) return 600 - idx - t.length / 10 + (isStart(t, idx) ? 100 : 0)
  let score = 0
  let ti = 0
  let run = 0
  for (const ch of q) {
    const found = t.indexOf(ch, ti)
    if (found < 0) return null
    run = found === ti ? run + 1 : 0
    score += 10 + run * 5 + (isStart(t, found) ? 15 : 0) - Math.min(9, found - ti)
    ti = found + 1
  }
  return score - t.length / 10
}

function isStart(t: string, i: number) {
  return i === 0 || /[\s/._:-]/.test(t[i - 1])
}

export function fuzzyFilter<T>(items: T[], query: string, text: (x: T) => string, limit = 50): T[] {
  if (!query.trim()) return items.slice(0, limit)
  return items
    .map((x) => ({ x, s: fuzzyScore(query, text(x)) }))
    .filter((r): r is { x: T; s: number } => r.s !== null)
    .sort((a, b) => b.s - a.s)
    .slice(0, limit)
    .map((r) => r.x)
}
