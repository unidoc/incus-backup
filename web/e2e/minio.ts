// A private MinIO for the storage e2e tests (the S3 repository flow): started from the binary in
// INCUS_BACKUP_TEST_MINIO, one bucket created with a signed S3 request (AWS SigV4, no SDK). Fails
// closed when the binary is missing.
import { spawn, type ChildProcess } from 'node:child_process'
import { createHash, createHmac } from 'node:crypto'
import { existsSync, mkdtempSync, rmSync } from 'node:fs'
import { createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

export interface Minio { endpoint: string; access: string; secret: string; stop: () => void }

function freePort(): Promise<number> {
  return new Promise((ok, fail) => {
    const s = createServer()
    s.once('error', fail)
    s.listen(0, '127.0.0.1', () => { const p = (s.address() as { port: number }).port; s.close(() => ok(p)) })
  })
}

export async function startMinio(): Promise<Minio> {
  const bin = process.env.INCUS_BACKUP_TEST_MINIO ?? ''
  if (!bin || !existsSync(bin)) throw new Error('INCUS_BACKUP_TEST_MINIO must point at a MinIO server binary (the S3 repository e2e test needs it)')
  const dir = mkdtempSync(join(tmpdir(), 'ib-e2e-minio-'))
  const port = await freePort()
  const cport = await freePort()
  const access = 'e2eaccess', secret = 'e2esecret-123456'
  const p: ChildProcess = spawn(bin, ['server', dir, '--address', `127.0.0.1:${port}`, '--console-address', `127.0.0.1:${cport}`, '--quiet'],
    { env: { ...process.env, MINIO_ROOT_USER: access, MINIO_ROOT_PASSWORD: secret, MINIO_BROWSER: 'off' }, stdio: 'ignore' })
  const stop = () => { try { p.kill('SIGTERM') } catch { /* gone */ } rmSync(dir, { recursive: true, force: true }) }
  const endpoint = `127.0.0.1:${port}`
  for (let i = 0; i < 100; i++) {
    try { if ((await fetch(`http://${endpoint}/minio/health/live`)).ok) return { endpoint, access, secret, stop } } catch { /* starting */ }
    await new Promise((r) => setTimeout(r, 150))
  }
  stop()
  throw new Error(`MinIO did not start on ${endpoint}`)
}

const sha256 = (s: string) => createHash('sha256').update(s).digest('hex')
const hmac = (k: Buffer | string, s: string) => createHmac('sha256', k).update(s).digest()

/** Create a bucket with a SigV4-signed PUT (region us-east-1). */
export async function makeBucket(m: Minio, bucket: string): Promise<void> {
  const now = new Date()
  const amzDate = now.toISOString().replace(/[-:]/g, '').replace(/\.\d{3}/, '')
  const day = amzDate.slice(0, 8)
  const payload = sha256('')
  const headers = `host:${m.endpoint}\nx-amz-content-sha256:${payload}\nx-amz-date:${amzDate}\n`
  const signed = 'host;x-amz-content-sha256;x-amz-date'
  const canonical = ['PUT', `/${bucket}`, '', headers, signed, payload].join('\n')
  const scope = `${day}/us-east-1/s3/aws4_request`
  const toSign = ['AWS4-HMAC-SHA256', amzDate, scope, sha256(canonical)].join('\n')
  const key = hmac(hmac(hmac(hmac(`AWS4${m.secret}`, day), 'us-east-1'), 's3'), 'aws4_request')
  const sig = createHmac('sha256', key).update(toSign).digest('hex')
  const r = await fetch(`http://${m.endpoint}/${bucket}`, {
    method: 'PUT',
    headers: { 'x-amz-date': amzDate, 'x-amz-content-sha256': payload, Authorization: `AWS4-HMAC-SHA256 Credential=${m.access}/${scope}, SignedHeaders=${signed}, Signature=${sig}` },
  })
  if (!r.ok) throw new Error(`make bucket ${bucket}: ${r.status} ${await r.text()}`)
}
