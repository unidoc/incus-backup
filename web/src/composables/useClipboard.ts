// Copy text with a short "Copied" state. Falls back to a hidden textarea where the Clipboard API is
// unavailable (plain-HTTP origins other than localhost).
import { ref } from 'vue'

export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch { /* fall through */ }
  const ta = document.createElement('textarea')
  ta.value = text
  ta.setAttribute('readonly', '')
  ta.style.position = 'fixed'
  ta.style.opacity = '0'
  document.body.appendChild(ta)
  ta.select()
  let ok = false
  try { ok = document.execCommand('copy') } catch { ok = false }
  ta.remove()
  return ok
}

export function useClipboard(resetMs = 1600) {
  const copied = ref(false)
  const failed = ref(false)
  let t: ReturnType<typeof setTimeout> | null = null
  async function copy(text: string) {
    const ok = await copyText(text)
    copied.value = ok
    failed.value = !ok
    if (t) clearTimeout(t)
    t = setTimeout(() => { copied.value = false; failed.value = false }, resetMs)
    return ok
  }
  return { copy, copied, failed }
}

/** Offer text as a file download (recovery codes, CSV). */
export function downloadText(filename: string, text: string, type = 'text/plain') {
  const url = URL.createObjectURL(new Blob([text], { type: `${type};charset=utf-8` }))
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
