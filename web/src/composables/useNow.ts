// One shared clock for relative times ("3 min ago"), ticking every 30 s while anything uses it.
import { ref, onBeforeUnmount, getCurrentInstance } from 'vue'

const now = ref(Date.now())
let users = 0
let timer: ReturnType<typeof setInterval> | null = null

export function useNow() {
  users++
  if (!timer) timer = setInterval(() => { now.value = Date.now() }, 30_000)
  if (getCurrentInstance()) {
    onBeforeUnmount(() => {
      users--
      if (users === 0 && timer) { clearInterval(timer); timer = null }
    })
  }
  return now
}
