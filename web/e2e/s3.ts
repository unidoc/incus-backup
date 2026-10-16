// A private S3 service for the storage e2e tests (the S3 repository flow): the in-process S3 test
// server of internal/s3test, served by cmd/ibs3. Hermetic: no external binary, no network. The ibs3
// binary comes from IB_S3_BIN, else it is built from this tree with `go build` (cached per run).
import { spawn, execFileSync, type ChildProcess } from 'node:child_process'
import { existsSync, mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { ROOT } from './backend'

export interface S3 { endpoint: string; access: string; secret: string; stop: () => void }

let built = ''

function ibs3Binary(): string {
  const given = process.env.IB_S3_BIN ?? ''
  if (given) {
    if (!existsSync(given)) throw new Error(`IB_S3_BIN=${given} does not exist`)
    return given
  }
  if (!built) {
    const out = join(mkdtempSync(join(tmpdir(), 'ib-e2e-ibs3-bin-')), 'ibs3')
    execFileSync('go', ['build', '-o', out, './cmd/ibs3'], { cwd: ROOT, env: { ...process.env, CGO_ENABLED: '0' }, stdio: 'inherit' })
    built = out
  }
  return built
}

/** Starts the S3 test server with one empty bucket and resolves once it listens. */
export async function startS3(bucket: string): Promise<S3> {
  const access = 'e2eaccess', secret = 'e2esecret-123456'
  const p: ChildProcess = spawn(ibs3Binary(), ['-addr', '127.0.0.1:0', '-access', access, '-secret', secret, '-bucket', bucket], { stdio: ['ignore', 'pipe', 'inherit'] })
  const stop = () => { try { p.kill('SIGTERM') } catch { /* gone */ } }
  const endpoint = await new Promise<string>((ok, fail) => {
    let buf = ''
    const timer = setTimeout(() => { stop(); fail(new Error('ibs3 did not report its address in 20 s')) }, 20_000)
    p.once('error', (e) => { clearTimeout(timer); fail(e) })
    p.once('exit', (code) => { clearTimeout(timer); fail(new Error(`ibs3 exited early (${code})`)) })
    p.stdout!.on('data', (d: Buffer) => {
      buf += d.toString()
      const m = /listening on (\S+)/.exec(buf)
      if (m) { clearTimeout(timer); p.removeAllListeners('exit'); ok(m[1]) }
    })
  })
  return { endpoint, access, secret, stop }
}
