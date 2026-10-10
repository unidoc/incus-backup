import { startDemo, startReal, stopAll } from './backend'

export default async function globalSetup() {
  try {
    const real = await startReal()
    process.env.IB_E2E_SETUP_TOKEN = real.setupToken
    const demo = await startDemo()
    if (demo) process.env.IB_E2E_DEMO = demo
  } catch (e) {
    stopAll()
    throw e
  }
  return () => stopAll()
}
