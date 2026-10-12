// Toasts: short confirmations and errors that do not block. Errors stay until dismissed; others fade.
// Use toast.success('Policy saved'), toast.error(err, 'Could not save the policy').
import { reactive } from 'vue'
import { ApiError } from '@/api/client'

export type ToastKind = 'success' | 'error' | 'warn' | 'info'
export interface Toast { id: number; kind: ToastKind; title: string; message?: string; timeout: number; action?: { label: string; run: () => void } }

export const toasts = reactive<Toast[]>([])
let seq = 0
const timers = new Map<number, ReturnType<typeof setTimeout>>()

export function dismiss(id: number) {
  const i = toasts.findIndex((t) => t.id === id)
  if (i >= 0) toasts.splice(i, 1)
  const tm = timers.get(id)
  if (tm) { clearTimeout(tm); timers.delete(id) }
}

export function push(t: Omit<Toast, 'id' | 'timeout'> & { timeout?: number }): number {
  const id = ++seq
  const timeout = t.timeout ?? (t.kind === 'error' ? 0 : 5000)
  toasts.push({ ...t, id, timeout })
  if (toasts.length > 5) dismiss(toasts[0].id)
  if (timeout > 0) timers.set(id, setTimeout(() => dismiss(id), timeout))
  return id
}

/** The message to show for any thrown value. */
export function errorText(e: unknown): string {
  if (e instanceof ApiError) return e.message
  if (e instanceof Error) return e.message
  return String(e)
}

export const toast = {
  success: (title: string, message?: string) => push({ kind: 'success', title, message }),
  info: (title: string, message?: string) => push({ kind: 'info', title, message }),
  warn: (title: string, message?: string) => push({ kind: 'warn', title, message }),
  /** `title` says what failed ("Could not save the policy"); the server's text explains why. */
  error: (e: unknown, title = 'Something went wrong') => push({ kind: 'error', title, message: errorText(e) }),
}
