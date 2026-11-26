#!/usr/bin/env node
/**
 * End-to-end browser verification for the NFR Web Console.
 *
 * Covers:
 * - True element-level zero horizontal overflow across 360 / 375 / 390 / 1440px
 *   on Login, Dashboard, Create Tunnel, Tunnels, all 3 Settings tabs, More
 *   dropdown, Config modal, Detail modal, and Edit drawer.
 * - Plain-HTTP insecure-context (isSecureContext = false) modal clipboard copy
 *   verification via real button click (no DOM text shortcut).
 * - Client download selector (GitHub / Mirror auto-speed-test / Local archives).
 * - Create tunnel inputs start empty (no pre-filled local host/port defaults).
 * - Standalone tunnel create -> per-tunnel config -> nfrc online -> probe -> force offline.
 * - Dark mode persistence across page reload, zero console errors, zero unsolicited external requests.
 */

import { createRequire } from 'node:module'
import { spawn } from 'node:child_process'
import http from 'node:http'
import net from 'node:net'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

const require = createRequire(import.meta.url)

function loadPlaywright() {
  const candidates = [
    'playwright',
    process.env.PLAYWRIGHT_PATH,
  ]
  for (const mod of candidates.filter(Boolean)) {
    try {
      return require(mod)
    } catch {
      // try next candidate
    }
  }
  throw new Error('playwright module not found; install playwright or set NODE_PATH')
}

const { chromium } = loadPlaywright()
const ROOT = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')
const NFRS = process.env.NFRS_BIN || path.join(ROOT, 'bin', 'nfrs')
const NFRC = process.env.NFRC_BIN || path.join(ROOT, 'bin', 'nfrc')

function freePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer()
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address()
      const port = typeof addr === 'object' && addr ? addr.port : 0
      srv.close((err) => (err ? reject(err) : resolve(port)))
    })
    srv.on('error', reject)
  })
}

async function waitPort(port, timeoutMs = 15000) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    const ok = await new Promise((resolve) => {
      const sock = net.createConnection({ host: '127.0.0.1', port })
      sock.once('connect', () => { sock.destroy(); resolve(true) })
      sock.once('error', () => { sock.destroy(); resolve(false) })
    })
    if (ok) return true
    await new Promise((r) => setTimeout(r, 100))
  }
  return false
}

const failures = []
function check(name, ok, detail = '') {
  if (ok) {
    console.log(`  ok   ${name}`)
  } else {
    console.log(`  FAIL ${name}${detail ? `  (${detail})` : ''}`)
    failures.push(`${name}${detail ? ` (${detail})` : ''}`)
  }
}

async function main() {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'nfr-web-e2e-'))
  const clientsDir = path.join(tmp, 'clients')
  fs.mkdirSync(clientsDir, { recursive: true })
  fs.writeFileSync(path.join(clientsDir, 'nfrc_linux_amd64.tar.gz'), 'dummy-tgz')
  fs.writeFileSync(path.join(clientsDir, 'nfrc_windows_amd64.zip'), 'dummy-zip')

  const procs = []
  let localSrv = null
  let browser = null

  try {
    const ctlPort = await freePort()
    const httpPort = await freePort()
    const localPort = await freePort()
    const dataDir = path.join(tmp, 'data')
    const cfgPath = path.join(tmp, 'server.toml')

    fs.writeFileSync(
      cfgPath,
      `control_listen = "127.0.0.1:${ctlPort}"\n` +
      `http_listen = "127.0.0.1:${httpPort}"\n` +
      `https_listen = "off"\n` +
      `data_dir = "${dataDir}"\n` +
      `port_range = [20000, 29999]\n` +
      `log_level = "info"\n` +
      `[panel]\nhttps = false\n`,
    )

    localSrv = http.createServer((_req, res) => {
      res.writeHead(200, { 'Content-Type': 'text/plain' })
      res.end('ok local\n')
    })
    await new Promise((r) => localSrv.listen(localPort, '127.0.0.1', r))

    const logPath = path.join(tmp, 'server.log')
    const logFd = fs.openSync(logPath, 'w')
    const nfrsProc = spawn(NFRS, ['--foreground', '-c', cfgPath], {
      stdio: ['ignore', logFd, logFd],
      env: { ...process.env, NFR_CLIENTS_DIR: clientsDir },
    })
    procs.push(nfrsProc)

    if (!(await waitPort(ctlPort))) throw new Error('nfrs did not listen')
    await new Promise((r) => setTimeout(r, 400))
    const logText = fs.readFileSync(logPath, 'utf8')
    const pwMatch = logText.match(/密码\s+(\S+)/)
    if (!pwMatch) throw new Error('password not found')
    const password = pwMatch[1]
    const baseUrl = `http://127.0.0.1:${ctlPort}`

    browser = await chromium.launch({ headless: true })
    const consoleErrors = []
    const pageErrors = []
    const externalRequests = []

    const context = await browser.newContext({ viewport: { width: 375, height: 812 } })
    const page = await context.newPage()
    // Simulate plain-HTTP public IP (isSecureContext = false) so clipboard fallback is always exercised.
    await page.addInitScript(() => {
      Object.defineProperty(window, 'isSecureContext', { value: false })
      window.__copiedText = ''
      document.addEventListener('copy', () => {
        const active = document.activeElement
        window.__copiedText =
          window.getSelection()?.toString() ||
          (active && 'value' in active ? String(active.value) : '') ||
          ''
      })
    })
    page.on('console', (msg) => {
      if (msg.type() === 'error') consoleErrors.push(msg.text())
    })
    page.on('pageerror', (err) => pageErrors.push(String(err)))
    page.on('request', (req) => {
      const u = req.url()
      if (!u.startsWith(baseUrl) && !u.startsWith('data:') && !u.startsWith('blob:')) {
        externalRequests.push(u)
      }
    })

    async function assertTrueZeroOverflow(label) {
      await page.waitForTimeout(250)
      const audit = await page.evaluate(() => {
        const vw = window.innerWidth
        const docOverflow = document.documentElement.scrollWidth > vw + 1
        const offenders = []
        for (const el of document.querySelectorAll('body *')) {
          if (el.closest('.nfr-code-box') || el.closest('.n-scrollbar-rail') || el.closest('.n-base-select-menu')) continue
          const st = window.getComputedStyle(el)
          if (st.display === 'none' || st.visibility === 'hidden' || st.opacity === '0') continue
          const rect = el.getBoundingClientRect()
          if (rect.width === 0 || rect.height === 0) continue
          if (rect.right > vw + 2 || rect.left < -2) {
            let clipped = false
            let cur = el.parentElement
            while (cur && cur !== document.body) {
              const pst = window.getComputedStyle(cur)
              if (pst.overflowX === 'hidden' || pst.overflowX === 'auto' || pst.overflowX === 'scroll') {
                const prect = cur.getBoundingClientRect()
                if (prect.right <= vw + 2 && prect.left >= -2) {
                  clipped = true
                  break
                }
              }
              cur = cur.parentElement
            }
            if (!clipped) {
              offenders.push({
                tag: el.tagName.toLowerCase(),
                cls: String(el.className || '').slice(0, 50),
                left: Math.round(rect.left),
                right: Math.round(rect.right),
                vw,
              })
            }
          }
        }
        return {
          docOverflow,
          scrollWidth: document.documentElement.scrollWidth,
          vw,
          offenders: offenders.slice(0, 5),
        }
      })
      check(
        `true-zero-overflow ${label}`,
        !audit.docOverflow && audit.offenders.length === 0,
        `scrollW=${audit.scrollWidth}/${audit.vw} offenders=${JSON.stringify(audit.offenders)}`,
      )
    }

    // 1. Login page & default light mode + custom logo image
    for (const [w, h] of [[360, 800], [375, 812], [390, 844], [1440, 900]]) {
      await page.setViewportSize({ width: w, height: h })
      await page.goto(`${baseUrl}/nfr/login`, { waitUntil: 'networkidle' })
      await assertTrueZeroOverflow(`/nfr/login @${w}px`)
    }
    check('default theme is light mode', !(await page.evaluate(() => document.documentElement.classList.contains('dark'))))
    const loginLogoOk = await page.evaluate(() => {
      const img = document.querySelector('.login-logo-img')
      return Boolean(img && img.complete && img.naturalWidth > 0)
    })
    check('login page renders custom NFR logo', loginLogoOk)

    // 2. Sign in with "记住密码" checked -> verify remember persistence & Dashboard GitHub/Mirror download links
    await page.setViewportSize({ width: 375, height: 812 })
    await page.goto(`${baseUrl}/nfr/login`, { waitUntil: 'networkidle' })
    await page.fill('input[type="password"]', password)
    await page.click('.login-options .n-checkbox')
    await page.click('button[type="submit"]')
    await page.waitForURL('**/nfr/', { timeout: 10000 })
    await page.waitForSelector('.brand-logo-img', { timeout: 10000 })
    await page.waitForFunction(() => {
      const img = document.querySelector('.brand-logo-img')
      return Boolean(img && img.complete && img.naturalWidth > 0)
    }, { timeout: 5000 })
    check('main layout header renders custom NFR logo', true)
    await page.waitForSelector('.dl-controls', { timeout: 10000 })
    const dlHrefGitHub = await page.locator('a:has-text("下载客户端")').getAttribute('href')
    check(
      'dashboard download defaults to GitHub archive URL',
      Boolean(dlHrefGitHub && /^https:\/\/github\.com\/Solitudeit\/nfr\/releases\/download\/[^/]+\/nfrc_/.test(dlHrefGitHub)),
      String(dlHrefGitHub),
    )
    check(
      'zero external requests before user activates mirror speed-test',
      externalRequests.length === 0,
      externalRequests.join(', '),
    )

    // Mock mirror probe latency in-browser: make ghfast.top fastest (15ms) while others are slower
    await page.route(/^https:\/\/(edgeone\.gh-proxy\.com|gh-proxy\.org|gh-proxy\.com|ghfast\.top|git\.yylx\.win)\//, async (route) => {
      const u = route.request().url()
      const delay = u.startsWith('https://ghfast.top/') ? 15 : 450
      await new Promise((r) => setTimeout(r, delay))
      await route.fulfill({ status: 200, contentType: 'text/plain', body: 'ok' })
    })

    // Switch download source to 镜像下载 and verify browser auto-selects fastest mirror
    await page.locator('.dl-controls .n-select').first().click()
    const hasGiteeOpt = (await page.locator('.n-base-select-option:has-text("Gitee")').count()) > 0
    check('Gitee option is completely removed', !hasGiteeOpt)
    await page.click('.n-base-select-option:has-text("镜像下载")')
    await page.waitForSelector('.dl-mirror-status:has-text("ghfast.top")', { timeout: 5000 })
    const dlHrefMirror = await page.locator('a:has-text("下载客户端")').getAttribute('href')
    check(
      'switching to 镜像下载 races mirrors in-browser and picks fastest mirror',
      Boolean(dlHrefMirror && /^https:\/\/ghfast\.top\/https:\/\/github\.com\/Solitudeit\/nfr\/releases\/download\/[^/]+\/nfrc_/.test(dlHrefMirror)),
      String(dlHrefMirror),
    )
    // Switch back to GitHub so subsequent dashboard visits stay zero external network
    await page.locator('.dl-controls .n-select').first().click()
    await page.click('.n-base-select-option:has-text("GitHub")')
    await page.waitForTimeout(150)
    await page.unroute(/^https:\/\/(edgeone\.gh-proxy\.com|gh-proxy\.org|gh-proxy\.com|ghfast\.top|git\.yylx\.win)\//)
    externalRequests.length = 0

    // 3. Create tunnel -> verify empty inputs -> open config modal -> click REAL "复制配置" button under plain HTTP
    await page.click('.mobile-bottom-nav >> text=创建隧道')
    await page.waitForURL('**/nfr/create', { timeout: 10000 })
    const initLocalHost = await page.locator('[data-field="local_host"] input').inputValue()
    const initLocalPort = await page.locator('[data-field="local_port"] input').inputValue()
    check(
      'create tunnel local_host and local_port start empty without pre-filled defaults',
      initLocalHost === '' && initLocalPort === '',
      `host=${JSON.stringify(initLocalHost)} port=${JSON.stringify(initLocalPort)}`,
    )
    await page.fill('[data-field="name"] input', 'JiaoShi')
    await page.fill('[data-field="local_host"] input', '127.0.0.1')
    const portIn = page.locator('[data-field="local_port"] input')
    await portIn.fill(String(localPort))
    await portIn.press('Enter')
    await page.click('button:has-text("获取空闲端口")')
    await page.waitForTimeout(400)
    await page.click('button[type="submit"]')
    await page.waitForSelector('text=隧道创建完成', { timeout: 10000 })

    await page.click('button:has-text("生成启动配置")')
    await page.waitForSelector('.cfg-code', { timeout: 10000 })
    await assertTrueZeroOverflow('config modal @375px')
    await page.click('button:has-text("复制配置")')
    await page.waitForTimeout(300)
    const copiedToml = await page.evaluate(() => window.__copiedText)
    check(
      'plain-HTTP modal copy writes actual per-tunnel config to clipboard',
      copiedToml.includes('name = "t-') && copiedToml.includes('name = "JiaoShi"'),
      `len=${copiedToml.length}`,
    )
    await page.click('.n-modal .n-base-close')
    await page.waitForSelector('.n-modal', { state: 'detached', timeout: 5000 })

    // 4. Start real nfrc using the EXACT clipboard text copied from the modal!
    await page.click('.mobile-bottom-nav >> text=隧道管理')
    await page.waitForURL('**/nfr/tunnels', { timeout: 10000 })
    await page.waitForSelector('.tunnel-card .status-pill.offline:has-text("离线")', { timeout: 10000 })

    const agentTomlPath = path.join(tmp, 'jiaoshi.toml')
    fs.writeFileSync(agentTomlPath, copiedToml)
    const agLogFd = fs.openSync(path.join(tmp, 'jiaoshi.log'), 'w')
    const agProc = spawn(NFRC, ['-c', agentTomlPath], { stdio: ['ignore', agLogFd, agLogFd] })
    procs.push(agProc)

    await page.waitForSelector('.tunnel-card .status-pill.online:has-text("在线")', { timeout: 15000 })
    check('nfrc booted from copied config brings JiaoShi online', true)

    // Verify More dropdown, Detail modal + Probe, Edit drawer overflow
    await page.click('.tunnel-card button:has-text("更多")')
    await page.waitForSelector('text=查看详情', { timeout: 5000 })
    await assertTrueZeroOverflow('more dropdown @375px')
    await page.click('text=查看详情')
    await page.waitForSelector('.detail-addr-box', { timeout: 5000 })
    await page.click('button:has-text("一键测通")')
    await page.waitForSelector('text=已连通', { timeout: 10000 })
    await assertTrueZeroOverflow('detail modal @375px')
    await page.click('.n-modal .n-base-close')
    await page.waitForSelector('.n-modal', { state: 'detached', timeout: 5000 })

    await page.click('.tunnel-card button:has-text("更多")')
    await page.click('.n-dropdown-option:has-text("编辑")')
    await page.waitForSelector('text=编辑隧道', { timeout: 5000 })
    await page.waitForTimeout(350)
    await assertTrueZeroOverflow('edit drawer @375px')
    // Edit name, local IP, local port, and remote port directly without pre-blurring,
    // then immediately open 生成启动配置 and verify TOML/YAML & clipboard reflect every edit.
    const editDrawer = page.locator('.n-drawer')
    await editDrawer.locator('input[placeholder*="web"]').fill('JiaoShi2')
    await editDrawer.locator('input[placeholder="127.0.0.1"]').fill('127.0.0.2')
    const editNums = editDrawer.locator('.n-input-number input')
    await editNums.nth(0).fill('19090')
    await editNums.nth(1).fill('24680')
    await editDrawer.locator('button:has-text("保存修改")').click()
    await page.waitForSelector('.n-drawer', { state: 'detached', timeout: 5000 })

    await page.click('.tunnel-card button:has-text("更多")')
    await page.click('.n-dropdown-option:has-text("生成启动配置")')
    await page.waitForSelector('.cfg-code', { timeout: 10000 })
    const editedTitle = await page.locator('.n-modal .n-card-header__main').innerText()
    const editedToml = await page.locator('.cfg-code').innerText()
    check(
      'edited tunnel config immediately reflects new name, local IP, local port, and remote port',
      editedTitle.includes('JiaoShi2') &&
        editedToml.includes('name = "JiaoShi2"') &&
        editedToml.includes('local = "127.0.0.2:19090"') &&
        editedToml.includes('remote_port = 24680') &&
        !editedToml.includes('local_port = 0'),
      `title=${editedTitle} toml=${editedToml}`,
    )
    await page.click('.cfg-bar >> text=YAML')
    await page.waitForTimeout(250)
    const editedYaml = await page.locator('.cfg-code').innerText()
    check(
      'edited tunnel YAML reflects new local IP, local port, and remote port',
      editedYaml.includes('name: JiaoShi2') &&
        editedYaml.includes('local: 127.0.0.2:19090') &&
        editedYaml.includes('remote_port: 24680'),
      editedYaml,
    )
    await page.click('.n-modal .n-base-close')
    await page.waitForSelector('.n-modal', { state: 'detached', timeout: 5000 })

    // 5. All 4 routes across all 4 viewports + 3 settings tabs
    for (const [w, h] of [[360, 800], [375, 812], [390, 844], [1440, 900]]) {
      await page.setViewportSize({ width: w, height: h })
      for (const r of ['/nfr/', '/nfr/create', '/nfr/tunnels', '/nfr/settings']) {
        await page.goto(`${baseUrl}${r}`, { waitUntil: 'networkidle' })
        await assertTrueZeroOverflow(`${r} @${w}px`)
      }
    }
    await page.setViewportSize({ width: 360, height: 800 })
    await page.goto(`${baseUrl}/nfr/settings`, { waitUntil: 'networkidle' })
    for (const tabLabel of ['基础配置', '安全与接口', '外观偏好']) {
      await page.click(`.n-tabs-tab:has-text("${tabLabel}")`)
      await assertTrueZeroOverflow(`settings tab ${tabLabel} @360px`)
    }

    // 6. Dark mode persistence + Force offline kick
    await page.setViewportSize({ width: 375, height: 812 })
    await page.goto(`${baseUrl}/nfr/tunnels`, { waitUntil: 'networkidle' })
    await page.click('button[aria-label="切换为深色模式"]')
    await page.waitForTimeout(250)
    await page.reload({ waitUntil: 'networkidle' })
    check('dark mode persists across reload', await page.evaluate(() => document.documentElement.classList.contains('dark')))

    await page.click('.tunnel-card button:has-text("更多")')
    await page.click('.n-dropdown-option:has-text("强制下线")')
    await page.waitForSelector('.tunnel-card .status-pill.offline:has-text("离线")', { timeout: 10000 })
    check('force offline kicks nfrc and card flips to 离线', true)

    // 7. Verify secure "30 天免登录": zero password in localStorage, HttpOnly persistent cookie, and clean logout revocation
    const lsDump = await page.evaluate(() => JSON.stringify(window.localStorage))
    check(
      'zero password stored in localStorage',
      !lsDump.includes(password) && !lsDump.includes('nfr_login_saved_pw'),
      lsDump,
    )
    const panelCookie = (await context.cookies()).find((c) => c.name === 'nfr_panel')
    const nowSec = Date.now() / 1000
    check(
      'remember-me issues 30-day HttpOnly persistent cookie',
      Boolean(panelCookie && panelCookie.httpOnly && panelCookie.expires > nowSec + 29 * 86400),
      JSON.stringify(panelCookie),
    )

    await page.setViewportSize({ width: 1440, height: 900 })
    await page.click('button[aria-label="退出登录"]')
    await page.waitForURL('**/nfr/login', { timeout: 10000 })
    const afterLogoutPw = await page.locator('input[type="password"]').inputValue()
    check('logout returns to login with empty password input (no JS-stored secret)', afterLogoutPw === '')

    check('zero JS console errors', consoleErrors.length === 0, consoleErrors.join(' | '))
    check('zero uncaught page errors', pageErrors.length === 0, pageErrors.join(' | '))
    check('zero external network requests', externalRequests.length === 0, externalRequests.join(', '))
  } finally {
    if (browser) await browser.close()
    if (localSrv) localSrv.close()
    for (const p of procs) {
      try { p.kill('SIGTERM') } catch {}
    }
    fs.rmSync(tmp, { recursive: true, force: true })
  }

  console.log('')
  if (failures.length > 0) {
    console.error('WEB E2E FAILED:\n - ' + failures.join('\n - '))
    process.exit(1)
  }
  console.log('WEB E2E OK')
}

main().catch((err) => {
  console.error(err)
  process.exit(1)
})
