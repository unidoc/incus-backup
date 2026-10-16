// Backends for the end-to-end tests.
//
//  real: a fresh PostgreSQL (the embedded-postgres binaries testdb already caches) and the REAL
//        incus-backup-server binary built from this tree with the UI embedded (go:embed), serving HTTPS
//        with its self-signed certificate: exercises the CSP, cookies, CSRF and the SPA fallback
//        exactly as shipped. Starts with NO user (first-run setup) and two filesystem repositories.
//  demo: /tmp/ibwork/demo-bin/ibdemo (seeded realistic data, users admin/operator/restorer/viewer),
//        serving the Vite build from internal/server/webui/dist. Used for data-rich screenshots and
//        the role-aware navigation checks. Only started when the binary exists.
import { spawn, execFileSync, type ChildProcess } from 'node:child_process'
import { appendFileSync, existsSync, mkdirSync, rmSync, openSync } from 'node:fs'
import { homedir } from 'node:os'
import { createServer } from 'node:net'
import { join, resolve } from 'node:path'
import { fileURLToPath as toPath } from 'node:url'
const HERE = toPath(new URL('.', import.meta.url))

export const WEB = resolve(HERE, '..')
export const ROOT = resolve(WEB, '..')
export const STATE = join(WEB, 'e2e', '.state')
export const DEMO_BIN = process.env.IB_DEMO_BIN ?? '/tmp/ibwork/demo-bin/ibdemo'
export const REAL_PORT = Number(process.env.IB_E2E_PORT ?? 18443)
export const DEMO_PORT = Number(process.env.IB_E2E_DEMO_PORT ?? 18199)
export const REAL_URL = `https://127.0.0.1:${REAL_PORT}`
export const DEMO_URL = `http://127.0.0.1:${DEMO_PORT}`
export const PASSWORD = 'tide-lantern-ochre-47'
export const DEMO_PASSWORD = 'demo-demo-demo-1'

const procs: ChildProcess[] = []
let pgData = ''
let pgBin = ''

function run(cmd: string, args: string[], env: NodeJS.ProcessEnv = {}, cwd = ROOT): string {
  return execFileSync(cmd, args, { cwd, env: { ...process.env, ...env }, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })
}

async function waitFor(url: string, ms: number, what: string) {
  const until = Date.now() + ms
  let last = ''
  while (Date.now() < until) {
    try {
      const r = await fetch(url)
      if (r.ok) return
      last = `HTTP ${r.status}`
    } catch (e) { last = String((e as Error).cause ?? e) }
    await new Promise((r) => setTimeout(r, 300))
  }
  throw new Error(`${what} did not come up at ${url} within ${ms / 1000}s (last: ${last}); see ${STATE}/*.log`)
}

function freePortCheck(port: number): Promise<void> {
  return new Promise((ok, fail) => {
    const srv = createServer()
    srv.once('error', () => fail(new Error(`port ${port} is in use: stop the old e2e server or set IB_E2E_PORT / IB_E2E_DEMO_PORT`)))
    srv.listen(port, '127.0.0.1', () => srv.close(() => ok()))
  })
}

export async function startReal(): Promise<{ url: string; setupToken: string }> {
  process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0' // the server's self-signed certificate (local test only)
  const dir = join(STATE, 'real')
  rmSync(dir, { recursive: true, force: true })
  mkdirSync(dir, { recursive: true })
  // 1. the server binary, with the freshly built UI embedded
  let bin = process.env.IB_SERVER_BIN ?? ''
  if (!bin) {
    if (!process.env.IB_E2E_SKIP_BUILD) run('npx', ['vite', 'build', '--logLevel', 'error'], {}, WEB)
    bin = join(dir, 'incus-backup-server')
    run('go', ['build', '-o', bin, './cmd/incus-backup-server'], { CGO_ENABLED: '0' })
  }
  // 2. PostgreSQL from the embedded-postgres cache
  const cache = process.env.INCUS_BACKUP_PG_CACHE ?? join(homedir(), '.cache', 'incus-backup-embedded-postgres')
  pgBin = join(cache, 'bin')
  if (!existsSync(join(pgBin, 'postgres'))) throw new Error(`no PostgreSQL binaries in ${pgBin}: run "go test ./internal/server/auth" once (embedded-postgres downloads them) or set INCUS_BACKUP_PG_CACHE`)
  const libEnv = { LD_LIBRARY_PATH: join(cache, 'lib') }
  pgData = join(dir, 'pg')
  const pgPort = REAL_PORT + 1
  run(join(pgBin, 'initdb'), ['-D', pgData, '-U', 'postgres', '--auth=trust', '-E', 'UTF8', '--no-sync'], libEnv)
  run(join(pgBin, 'pg_ctl'), ['-D', pgData, '-w', '-l', join(dir, 'pg.log'), '-o', `-p ${pgPort} -h 127.0.0.1 -k ${dir} -F`, 'start'], libEnv)
  // 3. `init` (CA, server certificate, schema, first repository, config file), then the web listener
  //    settings (bootstrap only; everything else is data in PostgreSQL) and a second repository
  await freePortCheck(REAL_PORT)
  const cfg = join(dir, 'server.yaml')
  run(bin, ['-c', cfg, 'init', '--state-dir', join(dir, 'state'), '--database-url', `postgres://postgres@127.0.0.1:${pgPort}/postgres?sslmode=disable`,
    '--repository', 'primary', '--datastore', join(dir, 'ds-primary'), '--listen', `127.0.0.1:${REAL_PORT + 2}`, '--san', '127.0.0.1'])
  appendFileSync(cfg, `web_listen: 127.0.0.1:${REAL_PORT}\nadmin_socket: "off"\n`)
  run(bin, ['-c', cfg, 'repo', 'create', 'archive', '--datastore', join(dir, 'ds-archive')])
  // 4. the daemon
  const log = openSync(join(dir, 'server.log'), 'a')
  const p = spawn(bin, ['-c', cfg, 'serve'], { cwd: dir, stdio: ['ignore', log, log], detached: false })
  procs.push(p)
  await waitFor(`${REAL_URL}/api/v1/setup`, 30_000, 'incus-backup-server')
  // each setup-token call replaces the previous token (also the one serve logged): fetch it last
  const setupToken = run(bin, ['-c', cfg, 'user', 'setup-token']).trim().split(/\s+/).pop() ?? ''
  if (!/^[A-Za-z0-9_-]{16,}$/.test(setupToken)) throw new Error(`unexpected setup-token output: ${setupToken}`)
  return { url: REAL_URL, setupToken }
}

export async function startDemo(): Promise<string | null> {
  if (!existsSync(DEMO_BIN)) return null
  // `--project real` alone does not need the demo (it seeds for ~2 minutes and needs memory)
  const a = process.argv.join(' ')
  if (/--project[ =]real\b/.test(a) && !/--project[ =]demo\b/.test(a)) return null
  const dir = join(STATE, 'demo')
  await freePortCheck(DEMO_PORT)
  const log = openSync(join(STATE, 'demo.log'), 'a')
  const args = ['-dir', dir, '-reset', '-profile', 'test', '-web', `127.0.0.1:${DEMO_PORT}`, '-ui-dir', join(ROOT, 'internal', 'server', 'webui', 'dist'), '-s3=false']
  const p = spawn(DEMO_BIN, args, { stdio: ['ignore', log, log], env: { ...process.env } })
  procs.push(p)
  await waitFor(`${DEMO_URL}/api/v1/ping`, 240_000, 'ibdemo')
  return DEMO_URL
}

export function stopAll() {
  for (const p of procs.reverse()) { try { p.kill('SIGTERM') } catch { /* gone */ } }
  if (pgData && pgBin) {
    try { run(join(pgBin, 'pg_ctl'), ['-D', pgData, '-m', 'fast', 'stop'], { LD_LIBRARY_PATH: join(pgBin, '..', 'lib') }) } catch { /* already stopped */ }
  }
}
