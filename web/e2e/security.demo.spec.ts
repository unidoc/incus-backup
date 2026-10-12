// Security follow-ups against the seeded demo server (real server stack, real host job executors):
//  * an authenticated delivery (IBS2): the Restore page asks for the recovery key, verifies the
//    delivery and saves it; a delivery FORGED by a malicious server (sealed to the right public key,
//    the right job id and parameters, but without the repository key) is refused and nothing is
//    saved; and the download is in the audit log;
//  * the live event stream's `signed_out`: the page goes to sign-in and says why;
//  * the 429 after wrong current passwords on the profile page;
//  * a cross-site sign-in refused by the server (403) explained on the sign-in page.
import { test, expect, type Page } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { createHash, randomBytes } from 'node:crypto'
import { existsSync, mkdtempSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { DEMO_PASSWORD, ROOT, STATE } from './backend'
import { shot, watchConsole } from './helpers'

test.describe.configure({ mode: 'serial' })

const DEMO_DIR = join(STATE, 'demo')
interface Facts { fs_object: { group: string; snapshot: string }; fs_file: string; fs_file_sha256: string }
const state = () => JSON.parse(readFileSync(join(DEMO_DIR, 'demo.json'), 'utf8')) as { recovery_key: string; facts: Facts }

async function login(page: Page, name: string, password = DEMO_PASSWORD) {
  await page.goto('/login')
  await page.getByLabel('User name').fill(name)
  await page.locator('input[name=password]').fill(password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByTestId('session-role')).not.toHaveText('')
}

async function csrf(page: Page): Promise<string> {
  const r = await page.request.get('/api/v1/session')
  return ((await r.json()) as { csrf: string }).csrf
}

/** Starts an fs_extract from the Restore page; returns the job id and the parameters the page sent. */
async function startExtract(page: Page, paths: string): Promise<{ id: string; params: Record<string, unknown> }> {
  const f = state().facts.fs_object
  await page.goto(`/restore?repository=custody&action=fs_extract&group=${f.group}&snap=${f.snapshot}`)
  await expect(page.getByTestId('pick-snapshot')).toHaveValue(f.snapshot)
  await page.getByTestId('next-options').click()
  await page.getByTestId('paths').fill(paths)
  await page.getByTestId('next-review').click()
  const created = page.waitForResponse((r) => r.url().includes('/api/v1/jobs') && r.request().method() === 'POST')
  await page.getByTestId('start-job').click()
  const res = await created
  const params = (JSON.parse(res.request().postData() ?? '{}') as { params: Record<string, unknown> }).params
  const id = ((await res.json()) as { id: string }).id
  await expect(page.getByTestId('job-panel')).toHaveAttribute('data-state', 'succeeded', { timeout: 120_000 })
  await expect(page.getByTestId('delivery')).toBeVisible()
  return { id, params }
}

async function unlockDelivery(page: Page, key: string) {
  const gate = page.getByTestId('delivery').getByTestId('key-gate')
  await expect(gate).toBeVisible()
  await gate.getByTestId('key-input').fill(key)
  await gate.getByTestId('key-unlock').click()
  await expect(page.getByTestId('delivery-download')).toBeVisible()
}

test('an authenticated delivery: the page asks for the recovery key, verifies, saves; the tar equals the source; the download is audited', async ({ page }) => {
  test.setTimeout(180_000)
  const con = watchConsole(page)
  const st = state()
  await login(page, 'admin')
  const { id } = await startExtract(page, st.facts.fs_file)
  // the delivery cannot be opened without the repository key: the gate asks for it
  await expect(page.getByTestId('delivery-download')).toHaveCount(0)
  await shot(page, 'security-delivery-keygate-desktop-light')
  // a typo is refused and the field is cleared
  await page.getByTestId('delivery').getByTestId('key-input').fill(st.recovery_key.slice(0, -3) + 'AAA')
  await page.getByTestId('delivery').getByTestId('key-unlock').click()
  await expect(page.getByTestId('key-error')).toBeVisible()
  await expect(page.getByTestId('delivery').getByTestId('key-input')).toHaveValue('')
  await unlockDelivery(page, st.recovery_key)
  const dl = page.waitForEvent('download')
  await page.getByTestId('delivery-download').click()
  const d = await dl
  await expect(page.getByTestId('delivery-done')).toBeVisible()
  await shot(page, 'security-delivery-saved-desktop-light')
  // the demo keeps changing its live tree after the snapshot: compare with the SHA-256 the demo
  // recorded for this file in this snapshot
  const out = mkdtempSync(join(tmpdir(), 'ibtar-'))
  execFileSync('tar', ['-xf', (await d.path())!, '-C', out])
  const got = [join(out, st.facts.fs_file), join(out, 'out', st.facts.fs_file)].find((p) => existsSync(p))
  expect(got, 'the file is in the delivery').toBeTruthy()
  expect(createHash('sha256').update(readFileSync(got!)).digest('hex')).toBe(st.facts.fs_file_sha256)
  // the audit log names the download, the job and the bytes
  const audit = await page.request.get('/api/v1/audit?action=download%20delivery')
  expect(audit.ok()).toBe(true)
  const rows = (await audit.json()) as { actor: string; detail: { job: string; bytes: number }; ok: boolean }[]
  const row = rows.find((r) => r.detail.job === id)
  expect(row, JSON.stringify(rows).slice(0, 500)).toBeTruthy()
  expect(row!.actor).toBe('admin')
  expect(row!.ok).toBe(true)
  expect(row!.detail.bytes).toBeGreaterThan(1000)
  con.assertClean()
})

test('a delivery forged by the server (right public key, job id and parameters, no repository key) is refused; nothing is saved', async ({ page }) => {
  test.setTimeout(180_000)
  const st = state()
  await login(page, 'admin')
  const { id, params } = await startExtract(page, st.facts.fs_file)
  const pub = Buffer.from(String(params.recipient_public_key), 'base64').toString('hex')
  // the malicious server: everything it knows, sealed with a key that is not the repository's
  const forged = execFileSync('go', ['run', './internal/webfixture/sealer', '-pub', pub, '-master', randomBytes(32).toString('hex'), '-job', id, '-type', 'fs_extract', '-params', JSON.stringify(params)], {
    cwd: ROOT,
    input: Buffer.from('forged restore: run me\n'),
    maxBuffer: 16 << 20,
  })
  await page.route('**/api/v1/jobs/*/delivery', (route) => route.fulfill({ status: 200, body: forged, headers: { 'Content-Type': 'application/octet-stream', 'Content-Length': String(forged.length) } }))
  await unlockDelivery(page, st.recovery_key)
  let downloads = 0
  page.on('download', () => { downloads++ })
  await page.getByTestId('delivery-download').click()
  const err = page.getByTestId('delivery').locator('.callout, [role=alert]').filter({ hasText: 'did not verify' })
  await expect(err).toBeVisible({ timeout: 30_000 })
  await expect(err).toContainText('nothing was saved')
  await expect(page.getByTestId('delivery-done')).toHaveCount(0)
  await page.waitForTimeout(1000)
  expect(downloads).toBe(0)
  await shot(page, 'security-delivery-forged-desktop-light')
  await page.unroute('**/api/v1/jobs/*/delivery')
})

test('the live event stream ends with signed_out: the page goes to sign-in and says why', async ({ page, browser }) => {
  test.setTimeout(90_000)
  // a user of its own (other specs keep using the seeded users)
  const admin = await browser.newContext()
  const ap = await admin.newPage()
  await login(ap, 'admin')
  const name = `e2e-so-${Date.now() % 100000}`
  const c = await csrf(ap)
  const mk = await ap.request.post('/api/v1/users', { data: { name, password: DEMO_PASSWORD, role: 'readonly' }, headers: { 'X-CSRF-Token': c } })
  expect(mk.status(), await mk.text()).toBeLessThan(300)
  await login(page, name)
  const stream = page.waitForRequest(/\/api\/v1\/events/)
  await page.goto('/_styleguide') // a quiet page: no polling that would meet the 401 first
  await stream
  // an administrator disables the account: the next stream re-check (20 s) ends it with signed_out
  const dis = await ap.request.put(`/api/v1/users/${name}`, { data: { disabled: true }, headers: { 'X-CSRF-Token': c } })
  expect(dis.status(), await dis.text()).toBeLessThan(300)
  await expect(page).toHaveURL(/\/login\?.*reason=signed_out/, { timeout: 45_000 })
  await expect(page.getByTestId('signed-out')).toBeVisible()
  await shot(page, 'security-signed-out-desktop-light')
  await admin.close()
})

test('profile: wrong current passwords lock password checks; the 429 is explained', async ({ page, browser }) => {
  const admin = await browser.newContext()
  const ap = await admin.newPage()
  await login(ap, 'admin')
  const name = `e2e-lk-${Date.now() % 100000}`
  const mk = await ap.request.post('/api/v1/users', { data: { name, password: DEMO_PASSWORD, role: 'readonly' }, headers: { 'X-CSRF-Token': await csrf(ap) } })
  expect(mk.status(), await mk.text()).toBeLessThan(300)
  await admin.close()
  await login(page, name)
  await page.goto('/profile')
  const form = page.getByTestId('profile-password')
  for (let i = 0; i < 6; i++) {
    await form.locator('input[name=current]').fill(`wrong-password-${i}`)
    await form.locator('input[name=new]').fill('a-much-better-passphrase-77')
    await form.locator('input[name=confirm]').fill('a-much-better-passphrase-77')
    await form.getByRole('button', { name: /Change password/ }).click()
    await expect(form.getByRole('alert')).toBeVisible()
  }
  await expect(form.getByRole('alert')).toContainText('Too many wrong passwords')
  await expect(form.getByRole('alert')).toContainText('stay signed in')
  await shot(page, 'security-profile-locked-desktop-light')
  // still signed in
  await expect(page.getByTestId('session-role')).not.toHaveText('')
})

test('a cross-site sign-in refused by the server is explained', async ({ page }) => {
  // Chromium does not let a page (or an interception) forge its own Origin header. The request is
  // therefore sent to the REAL server from outside the page with a foreign Origin, exactly as a
  // cross-site form would arrive, and the server's real answer is what the page receives.
  await page.route('**/api/v1/session', async (route) => {
    if (route.request().method() !== 'POST') return route.continue()
    const real = await page.request.post('/api/v1/session', { headers: { Origin: 'https://evil.example', 'Content-Type': 'application/json' }, data: route.request().postData() ?? '' })
    expect(real.status()).toBe(403)
    return route.fulfill({ response: real })
  })
  await page.goto('/login')
  await page.getByLabel('User name').fill('viewer')
  await page.locator('input[name=password]').fill(DEMO_PASSWORD)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByTestId('login-error')).toContainText('cross-site request')
  await expect(page.getByTestId('login-error')).toContainText("server's own address")
  await shot(page, 'security-login-crosssite-desktop-light')
})

test('snapshot identity: genuine catalog snapshots open in both repositories (custody and the synced offsite); a mislabelled listing is refused', async ({ page }) => {
  test.setTimeout(120_000)
  const st = state()
  await login(page, 'admin')
  type Ref = { group_id: string; snapshot_id: string; kind?: string; project?: string; object?: string }
  const pick = async (repo: string): Promise<Ref> => {
    const r = await page.request.get(`/api/v1/snapshots?repository=${repo}`)
    expect(r.ok()).toBe(true)
    const l = ((await r.json()) as Ref[] | null) ?? []
    const fs = l.filter((x) => x.kind === 'filesystem' && x.object === 'volume/nextcloud-data')
    expect(fs.length, `${repo}: no synced filesystem snapshot`).toBeGreaterThan(0)
    return fs[fs.length - 1]!
  }
  const open = async (repo: string, s: Ref) => {
    await page.goto(`/snapshots/${s.group_id}/${s.snapshot_id}/browse?repository=${repo}`)
    await page.getByTestId('key-input').fill(st.recovery_key)
    await page.getByTestId('key-unlock').click()
  }
  for (const repo of ['custody', 'offsite']) {
    const s = await pick(repo)
    await open(repo, s)
    await expect(page.getByTestId('file-table'), repo).toBeVisible({ timeout: 60_000 })
    await page.getByTestId('lock').click()
  }
  // the server lists group A under another object's name: the page refuses to show it as that object
  const s = await pick('custody')
  await page.route(`**/api/v1/snapshots?*group=${s.group_id}*`, async (route) => {
    const res = await route.fetch()
    const l = (await res.json()) as Ref[]
    for (const x of l) x.object = 'volume/payroll'
    return route.fulfill({ response: res, json: l })
  })
  await open('custody', s)
  await expect(page.getByTestId('open-error')).toContainText('not the object the server listed', { timeout: 60_000 })
  await expect(page.getByTestId('file-table')).toHaveCount(0)
  await shot(page, 'security-identity-mislabelled-desktop-light')
})
