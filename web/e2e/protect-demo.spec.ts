// "Protection & recovery" against the seeded demo server: the zero-knowledge file browser end to end
// (recovery key -> browse -> download a file and a ZIP, compared byte for byte with the source tree the
// demo backed up), the key checks (another repository's key, a typo), that no request ever carries the
// key, the sealed host-side extraction (fs_extract: key pair in the page, unsealed download compared
// with the source), restore tests, verification, jobs and snapshot pages, screenshots in both themes.
//
// The demo writes its facts (recovery key, browsable snapshot, a file and its SHA-256) to demo.json.
// The host-job parts need a demo binary whose hosts run the job executor (IB_DEMO_BIN); see
// docs/web-protect.md. Tamper detection cannot be staged against a real server here: it is covered by
// web-crypto's own tests (test/security.test.ts, test/reader.test.ts).
import { test, expect, type Page, type Request } from '@playwright/test'
import { createHash } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { existsSync, mkdtempSync, readFileSync, readdirSync, statSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { DEMO_PASSWORD, STATE } from './backend'
import { shot, watchConsole } from './helpers'

test.describe.configure({ mode: 'serial' })

const DEMO_DIR = join(STATE, 'demo')
interface Facts { fs_object: { group: string; snapshot: string }; fs_file: string; fs_file_sha256: string; failed_verify: { group: string; snapshot: string }; protected: { group: string; snapshot: string } }
const state = () => JSON.parse(readFileSync(join(DEMO_DIR, 'demo.json'), 'utf8')) as { recovery_key: string; domain_id: string; facts: Facts }
const TREE = () => join(DEMO_DIR, 'hosts', 'benfica', 'volumes', 'nextcloud-data')
// a VALID recovery key of another key domain (format.EncodeRecoveryKey of sha256("another repository (e2e, public)"))
const OTHER_KEY = 'IBR1-TRWJS-CI6SQ-CUTI7-I6R3O-UBIVB-5KHEV-D7D73-PLTZK-6AYG5-WDUXS-ZVRJD-RRI'
const OTHER_DOMAIN = '6ac89a4dabc3b5c7'
const sha = (b: Buffer) => createHash('sha256').update(b).digest('hex')

async function login(page: Page, name = 'admin') {
  await page.goto('/login')
  await page.getByLabel('User name').fill(name)
  await page.locator('input[name=password]').fill(DEMO_PASSWORD)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByTestId('session-role')).not.toHaveText('')
}

/** Records every request the page sends (URL, headers, body) to prove the key never leaves it. */
function recordRequests(page: Page) {
  const seen: string[] = []
  const add = (r: Request) => seen.push(`${r.method()} ${r.url()}\n${JSON.stringify(r.headers())}\n${r.postData() ?? ''}`)
  page.on('request', add)
  page.context().on('request', add) // worker requests
  return {
    assertNoKey(key: string) {
      const compact = key.replace(/-/g, '')
      const needles = [key, compact, compact.toLowerCase()]
      for (let i = 4; i + 10 <= compact.length; i += 3) needles.push(compact.slice(i, i + 10))
      for (const s of seen) for (const n of needles) expect(s.includes(n), `request carries key material (${n}):\n${s.slice(0, 300)}`).toBe(false)
      expect(seen.length).toBeGreaterThan(5)
      // the decryption fetches only ciphertext through plain GETs
      for (const s of seen.filter((x) => /\/api\/v1\/(chunks|snapshots\/[0-9a-f]{32}\/[0-9-a-f]+\/(index|manifest))/.test(x))) expect(s.startsWith('GET ')).toBe(true)
    },
  }
}

async function jobDone(page: Page) {
  const panel = page.getByTestId('job-panel')
  await expect(panel).toHaveAttribute('data-state', /^(succeeded|failed|cancelled|expired)$/, { timeout: 120_000 })
  const msg = (await page.getByTestId('job-message').allTextContents()).join(' ')
  expect(await panel.getAttribute('data-state'), `job did not succeed: ${msg}`).toBe('succeeded')
}

async function openBrowser(page: Page, key: string) {
  const f = state().facts.fs_object
  await page.goto(`/snapshots/${f.group}/${f.snapshot}/browse?repository=custody`)
  await expect(page.getByTestId('key-gate')).toBeVisible()
  await page.getByTestId('key-input').fill(key)
  await page.getByTestId('key-unlock').click()
}

function filesUnder(dir: string, rel = ''): string[] {
  const out: string[] = []
  for (const n of readdirSync(join(dir, rel))) {
    const r = rel ? `${rel}/${n}` : n
    const st = statSync(join(dir, r), { throwIfNoEntry: false })
    if (!st) continue
    if (st.isDirectory()) out.push(...filesUnder(dir, r))
    else if (st.isFile()) out.push(r)
  }
  return out
}

test('browser: key entry, browse, single-file download equals the source, ZIP equals the source, no request carries the key', async ({ page }) => {
  test.setTimeout(180_000)
  const con = watchConsole(page)
  const req = recordRequests(page)
  const st = state()
  await login(page)
  await openBrowser(page, st.recovery_key)
  await expect(page.getByTestId('unlocked')).toBeVisible()
  await expect(page.getByTestId('file-table')).toBeVisible({ timeout: 30_000 })
  // the key field is gone (the string was handed to the module and dropped)
  await expect(page.getByTestId('key-input')).toHaveCount(0)
  await shot(page, 'protect-browser-root-desktop-light')

  // navigate into the folder that holds the unicode file
  const [dir, name] = [st.facts.fs_file.split('/').slice(0, -1).join('/'), st.facts.fs_file.split('/').pop()!]
  for (const part of dir.split('/').filter(Boolean)) await page.locator(`tr[data-path$="${part}"][data-kind="dir"] button.nm`).first().click()
  const row = page.locator(`tr[data-path="${st.facts.fs_file}"]`)
  await expect(row).toBeVisible()
  await row.click()
  await expect(page.getByTestId('file-info')).toHaveAttribute('data-path', st.facts.fs_file)
  await expect(page.getByTestId('file-info')).toContainText(st.facts.fs_file_sha256)
  // preview (whole file read and checked) shows the text
  await page.getByTestId('info-preview').click()
  const source = readFileSync(join(TREE(), st.facts.fs_file))
  expect(sha(source)).toBe(st.facts.fs_file_sha256) // the tree on disk is the backed-up state
  await expect(page.getByTestId('preview-text')).toContainText(source.toString('utf8').slice(0, 40))
  await shot(page, 'protect-browser-file-desktop-light')

  // single-file download: streamed, verified, saved only when complete
  const dl1 = page.waitForEvent('download')
  await page.getByTestId('info-download').click()
  const d1 = await dl1
  expect(d1.suggestedFilename()).toBe(name)
  const got = readFileSync((await d1.path())!)
  expect(sha(got)).toBe(st.facts.fs_file_sha256)
  expect(got.equals(source)).toBe(true)
  await expect(page.locator(`[data-testid=download][data-name="${name}"]`)).toHaveAttribute('data-state', 'done')

  // the folder as ZIP: every regular file inside equals the source
  await page.getByTestId('crumb-root').click()
  const folder = dir.split('/')[0]
  const dl2 = page.waitForEvent('download')
  await page.getByTestId(`dl-${folder}`).click()
  const d2 = await dl2
  expect(d2.suggestedFilename()).toBe(`${folder}.zip`)
  const zipPath = (await d2.path())!
  const out = mkdtempSync(join(tmpdir(), 'ibzip-'))
  execFileSync('python3', ['-c', 'import sys,zipfile; z=zipfile.ZipFile(sys.argv[1]); assert z.testzip() is None; z.extractall(sys.argv[2])', zipPath, out])
  const expected = filesUnder(TREE(), folder)
  expect(expected.length).toBeGreaterThan(3)
  for (const f of expected) expect(readFileSync(join(out, f)).equals(readFileSync(join(TREE(), f))), f).toBe(true)
  await expect(page.locator(`[data-testid=download][data-name="${folder}.zip"]`)).toHaveAttribute('data-state', 'done')

  // multi-select -> one ZIP of two entries
  await page.locator(`tr[data-path="${folder}"] td.ck input`).check()
  const firstFile = page.locator('tr[data-kind="file"]').first()
  const firstPath = await firstFile.getAttribute('data-path')
  await firstFile.locator('td.ck input').check()
  const dl3 = page.waitForEvent('download')
  await page.getByTestId('download-selected').click()
  const d3 = await dl3
  const names = execFileSync('python3', ['-c', 'import sys,zipfile; print("\\n".join(zipfile.ZipFile(sys.argv[1]).namelist()))', (await d3.path())!], { encoding: 'utf8' }).split('\n')
  expect(names).toContain(firstPath)
  expect(names.some((n) => n.startsWith(`${folder}/`))).toBe(true)

  // search by glob across the whole snapshot
  await page.getByTestId('find-input').fill('*.txt')
  await page.getByTestId('find-run').click()
  await expect(page.getByTestId('search-note')).toContainText('*.txt')
  await expect(page.locator(`tr[data-path="${st.facts.fs_file}"]`)).toBeVisible()
  await shot(page, 'protect-browser-search-desktop-light')

  // lock wipes the key; the gate comes back
  await page.getByTestId('lock').click()
  await expect(page.getByTestId('key-gate')).toBeVisible()
  req.assertNoKey(st.recovery_key)
  con.assertClean()
})

test('browser: another repository\'s key is named as such, a typo is explained', async ({ page }) => {
  const con = watchConsole(page)
  const st = state()
  expect(st.domain_id).not.toBe(OTHER_DOMAIN)
  await login(page)
  await openBrowser(page, OTHER_KEY)
  await expect(page.getByTestId('key-mismatch')).toContainText('This key belongs to another repository')
  await expect(page.getByTestId('key-mismatch')).toContainText(OTHER_DOMAIN)
  await expect(page.getByTestId('unlocked')).toHaveCount(0)
  await shot(page, 'protect-browser-wrong-key-desktop-light')
  // one character changed: the checksum catches it before anything is fetched
  const typo = st.recovery_key.slice(0, 10) + (st.recovery_key[10] === 'A' ? 'B' : 'A') + st.recovery_key.slice(11)
  await page.getByTestId('key-input').fill(typo)
  await page.getByTestId('key-unlock').click()
  await expect(page.getByTestId('key-error')).toContainText('not a valid recovery key')
  // the host's key file is recognised and explained
  await page.getByTestId('key-input').fill('{"version":1,"protection":"passphrase","domain_id":"x"}')
  await page.getByTestId('key-unlock').click()
  await expect(page.getByTestId('key-error')).toContainText('incus-backup key recovery')
  con.assertClean()
})

test('a disk snapshot is not browsable here and offers the host-side recovery', async ({ page }) => {
  const st = state()
  await login(page)
  const fv = st.facts.failed_verify // a VM root disk
  await page.goto(`/snapshots/${fv.group}/${fv.snapshot}/browse?repository=custody`)
  await expect(page.getByTestId('not-browsable')).toContainText('cannot be browsed')
  await page.getByTestId('offer-job').click()
  await expect(page).toHaveURL(/action=guest_files/)
  await expect(page.getByTestId('pick-snapshot')).toHaveValue(fv.snapshot)
})

test('fs_extract: the host extracts, seals to a key made in this tab, the page unseals and saves; the tar equals the source', async ({ page }) => {
  test.setTimeout(180_000)
  const con = watchConsole(page)
  const st = state()
  const f = st.facts.fs_object
  const folder = st.facts.fs_file.split('/')[0]
  await login(page)
  await page.goto(`/restore?repository=custody&action=fs_extract&group=${f.group}&snap=${f.snapshot}`)
  await expect(page.getByTestId('pick-snapshot')).toHaveValue(f.snapshot)
  await page.getByTestId('next-options').click()
  await page.getByTestId('paths').fill(`${folder}\n${st.facts.fs_file}`)
  await page.getByTestId('next-review').click()
  await shot(page, 'protect-restore-review-desktop-light')
  await page.getByTestId('start-job').click()
  await expect(page.getByTestId('job-panel')).toBeVisible()
  await jobDone(page)
  await expect(page.getByTestId('delivery')).toBeVisible()
  await shot(page, 'protect-restore-delivered-desktop-light')
  // IBS2: the delivery opens only with the repository key (it proves a key holder made it)
  await page.getByTestId('delivery').getByTestId('key-input').fill(st.recovery_key)
  await page.getByTestId('delivery').getByTestId('key-unlock').click()
  const dl = page.waitForEvent('download')
  await page.getByTestId('delivery-download').click()
  const d = await dl
  await expect(page.getByTestId('delivery-done')).toBeVisible()
  const out = mkdtempSync(join(tmpdir(), 'ibtar-'))
  execFileSync('tar', ['-xf', (await d.path())!, '-C', out])
  const expected = filesUnder(TREE(), folder)
  let compared = 0
  for (const rel of expected) {
    const cand = [join(out, rel), join(out, 'out', rel)].find((p) => existsSync(p))
    expect(cand, `${rel} missing from the delivery`).toBeTruthy()
    expect(readFileSync(cand!).equals(readFileSync(join(TREE(), rel))), rel).toBe(true)
    compared++
  }
  expect(compared).toBeGreaterThan(3)
  // the job is listed, with its params (the public key shortened) and log
  await page.goto('/jobs?repository=custody')
  await expect(page.getByTestId('job-table')).toContainText('Files from a filesystem')
  await page.getByTestId('job-table').getByText('Files from a filesystem').first().click()
  await expect(page.getByTestId('job-detail')).toContainText('one-time public key')
  await expect(page.getByTestId('job-log')).not.toContainText('No log lines yet')
  await shot(page, 'protect-job-detail-desktop-light')
  con.assertClean()
})

test('restore tests: confidence, run now as a job; verification runs; bulk protect on snapshots', async ({ page }) => {
  test.setTimeout(180_000)
  const con = watchConsole(page)
  await login(page, 'operator')
  await page.goto('/restore/tests?repository=custody')
  await expect(page.getByTestId('confidence')).toContainText('Restore confidence')
  await page.getByTestId('run-default/volume/nextcloud-data').click()
  await page.getByTestId('run-start').click()
  await jobDone(page)
  await shot(page, 'protect-restore-tests-desktop-light')

  await page.goto('/verify?repository=custody')
  await page.getByTestId('verify-run').click()
  await expect(page.getByTestId('verify-ok').or(page.getByTestId('verify-problems'))).toBeVisible({ timeout: 120_000 })
  await expect(page.getByTestId('attention-table')).toContainText('failed')

  await page.goto('/snapshots?repository=custody&verify=failed')
  await expect(page.getByTestId('snapshot-table')).toContainText('db-01')
  await page.goto('/snapshots?repository=custody&object=nextcloud')
  const box = page.locator('[data-testid=snapshot-table] tbody tr td.sel input').first()
  await box.check()
  await page.getByTestId('bulk-protect').click()
  await expect(page.locator('[data-testid=snapshot-table] tbody tr').first().getByLabel('Protected')).toBeVisible()
  // operators cannot forget: the button is disabled with an explanation
  await expect(page.getByTestId('bulk-forget')).toHaveCount(0)
  await box.check()
  await page.getByTestId('bulk-unprotect').click()
  con.assertClean()
})

test('admin bulk forget refuses protected snapshots and says why', async ({ page }) => {
  await login(page)
  const p = state().facts.protected
  await page.goto(`/snapshots?repository=custody&protected=yes`)
  await page.locator('[data-testid=snapshot-table] tbody tr td.sel input').first().check()
  await page.getByTestId('bulk-forget').click()
  await expect(page.getByTestId('forget-refused')).toContainText(p.snapshot)
  await expect(page.getByTestId('forget-continue')).toBeDisabled()
})

for (const theme of ['light', 'dark'] as const) {
  test(`protection screenshots, ${theme}`, async ({ browser }) => {
    test.setTimeout(180_000)
    const st = state()
    for (const vp of [{ name: 'desktop', width: 1440, height: 900 }, { name: 'mobile', width: 390, height: 844 }]) {
      const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, colorScheme: theme, deviceScaleFactor: vp.name === 'mobile' ? 2 : 1 })
      const page = await ctx.newPage()
      const con = watchConsole(page)
      await login(page)
      const pages: [string, string][] = [
        ['snapshots', '/snapshots?repository=custody'],
        ['snapshot', `/snapshots/${st.facts.failed_verify.group}/${st.facts.failed_verify.snapshot}?repository=custody`],
        ['restore', '/restore?repository=custody'],
        ['restore-form', `/restore?repository=custody&action=restore_instance`],
        ['verify', '/verify?repository=custody'],
        ['jobs', '/jobs?repository=custody'],
        ['restore-tests', '/restore/tests?repository=custody'],
      ]
      for (const [n, path] of pages) {
        await page.goto(path)
        await page.waitForLoadState('networkidle')
        await shot(page, `protect-${n}-${vp.name}-${theme}`)
        if (vp.name === 'mobile') expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `${n} scrolls sideways`).toBe(true)
      }
      await openBrowser(page, st.recovery_key)
      await expect(page.getByTestId('file-table')).toBeVisible({ timeout: 30_000 })
      await shot(page, `protect-browser-${vp.name}-${theme}`)
      if (vp.name === 'mobile') expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
      con.assertClean()
      await ctx.close()
    }
  })
}
