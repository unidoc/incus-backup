// Promise-based confirmation rendered by the one <ConfirmHost> in the app shell:
//   if (!(await confirm({ title: 'Delete policy daily?', danger: true, typeToConfirm: 'daily', confirmLabel: 'Delete' }))) return
import { reactive } from 'vue'

export interface ConfirmRequest { title: string; message?: string; confirmLabel?: string; danger?: boolean; typeToConfirm?: string }
export const confirmState = reactive({ open: false, req: null as ConfirmRequest | null, resolve: null as ((v: boolean) => void) | null })

export function confirm(req: ConfirmRequest): Promise<boolean> {
  confirmState.resolve?.(false)
  return new Promise((resolve) => {
    confirmState.req = req
    confirmState.resolve = resolve
    confirmState.open = true
  })
}

export function settleConfirm(v: boolean) {
  confirmState.open = false
  confirmState.resolve?.(v)
  confirmState.resolve = null
}
