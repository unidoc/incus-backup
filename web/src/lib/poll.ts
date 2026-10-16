// Timed polling that pauses while the tab is hidden.
//
// Every authenticated request renews the session (docs/authentication.md, "What counts as activity"),
// so a timer that keeps fetching in a hidden tab would keep it signed in forever. Every timer in the
// console that issues an API request goes through these two helpers instead of setInterval/setTimeout.

const visible = () => typeof document === 'undefined' || document.visibilityState === 'visible'

/**
 * Calls fn every `ms` while the tab is visible. A tick that falls into a hidden period is skipped, and
 * fn runs exactly once more when the tab becomes visible again (the catch-up). Returns stop().
 */
export function usePoll(fn: () => void, ms: number): () => void {
  let missed = false
  const id = setInterval(() => {
    if (visible()) fn()
    else missed = true
  }, ms)
  const onVis = () => {
    if (!missed || !visible()) return
    missed = false
    fn()
  }
  if (typeof document !== 'undefined') document.addEventListener('visibilitychange', onVis)
  return () => {
    clearInterval(id)
    if (typeof document !== 'undefined') document.removeEventListener('visibilitychange', onVis)
  }
}

/**
 * setTimeout for self-rescheduling poll chains: when the delay is over and the tab is hidden, fn waits
 * and runs once when the tab becomes visible. Returns cancel().
 */
export function visibleTimeout(fn: () => void, ms: number): () => void {
  let cancelled = false
  const onVis = () => {
    if (cancelled || !visible()) return
    document.removeEventListener('visibilitychange', onVis)
    fn()
  }
  const id = setTimeout(() => {
    if (cancelled) return
    if (visible()) fn()
    else document.addEventListener('visibilitychange', onVis)
  }, ms)
  return () => {
    cancelled = true
    clearTimeout(id)
    if (typeof document !== 'undefined') document.removeEventListener('visibilitychange', onVis)
  }
}
