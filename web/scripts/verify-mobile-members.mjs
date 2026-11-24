/**
 * 移动端成员面板验收：底部抽屉 + 安全区 + 触控目标 ≥44px。
 *
 * 这是 `verify-permissions.mjs` 的补充：桌面用 popover、移动端用底部抽屉
 * （PERMISSIONS §6.1 的 UI 要求 / 交付说明 §3.2）。
 *
 * 用法：node scripts/verify-mobile-members.mjs [webBase] [apiBase]
 */
import { chromium } from 'playwright'

const WEB = process.argv[2] ?? 'http://127.0.0.1:5173'
const API = process.argv[3] ?? 'http://localhost:8099'

const frame = (code, type, payload, seq) =>
  JSON.stringify({
    v: 1,
    type,
    roomId: code,
    clientId: 'srv',
    ts: Date.now(),
    ...(seq !== undefined ? { seq } : {}),
    payload,
  })

const LIMITS = {
  maxMessageBytes: 65536,
  maxShapeBytes: 32768,
  maxShapesPerRoom: 20000,
  opsPerSecond: 60,
  opsBurst: 120,
  cursorPerSecond: 20,
  resumeMaxOps: 2000,
  resumeMaxBytes: 4194304,
}

const results = []
function record(id, title, ok, detail = '') {
  results.push({ id, title, ok, detail })
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${id}  ${title}${detail ? `  [${detail}]` : ''}`)
}

async function createRoom(title) {
  // 建房是按 IP 限流的：测试脚本连跑会撞 429，这里退避重试
  for (let attempt = 0; attempt < 6; attempt += 1) {
    const res = await fetch(`${API}/api/rooms`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ title }),
    })
    if (res.ok) return res.json()
    const body = (await res.text()).slice(0, 120)
    if (res.status !== 429) throw new Error(`建房失败 ${res.status}：${body}`)
    await new Promise((r) => setTimeout(r, 4000))
  }
  throw new Error('建房失败：持续被限流（429）')
}

async function main() {
  const code = (await createRoom('移动端验收')).room.code
  console.log(`room: ${code}\n`)

  const browser = await chromium.launch({ channel: 'msedge' })
  // iPhone 13 尺寸 + 安全区（Playwright 不支持 env() 覆盖，用 CSS 变量近似）
  const ctx = await browser.newContext({
    viewport: { width: 390, height: 844 },
    deviceScaleFactor: 3,
    isMobile: true,
    hasTouch: true,
  })
  const page = await ctx.newPage()
  await ctx.addInitScript(() => window.localStorage.setItem('codraw.name', '房主甲'))

  await ctx.routeWebSocket(/\/ws(\?|$)/, (ws) => {
    const url = new URL(ws.url())
    const cid = url.searchParams.get('cid') ?? ''
    const name = url.searchParams.get('name') ?? '房主甲'
    const me = { clientId: cid, userId: `u_${cid}`, name, color: '#3d5afe', role: 'owner', isOwner: true }
    const peer = {
      clientId: 'c_peer',
      userId: 'u_peer',
      name: '访客乙',
      color: '#e8590c',
      role: 'viewer',
      isOwner: false,
    }
    ws.send(
      frame(code, 'joined', {
        you: me,
        room: { code, title: '移动端验收', epoch: 1, seq: 0 },
        online: 2,
        instanceId: 'i_mock',
        limits: LIMITS,
        ownerClientId: cid,
      }),
    )
    ws.send(frame(code, 'presence.join', { users: [me, peer], online: 2 }))
  })

  await page.goto(`${WEB}/r/${code}`, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!document.querySelector('.room__me'), null, { timeout: 20000 })

  // 成员入口在移动端应该仍然可见（房主要能管理权限）
  const onlineVisible = await page
    .locator('.online')
    .first()
    .isVisible()
    .catch(() => false)
  record('M1', '移动端顶栏保留成员入口', onlineVisible)

  const triggerBox = await page.locator('.online').first().boundingBox()
  record('M2', '成员入口触控高度 ≥44px', (triggerBox?.height ?? 0) >= 44, `h=${triggerBox?.height}`)

  await page.locator('.online').first().click()
  await page.waitForSelector('.members', { timeout: 5000 })
  // 等抽屉入场动画结束再量尺寸，否则会量到 translateY 中间态
  await page.waitForTimeout(500)

  const panel = await page.locator('.members').boundingBox()
  const vp = page.viewportSize()
  const isBottomSheet = panel && vp ? Math.abs(panel.y + panel.height - vp.height) < 2 : false
  record(
    'M3',
    '移动端成员面板是底部抽屉（贴底）',
    isBottomSheet,
    `panel.y+h=${panel?.y + panel?.height} vh=${vp?.height}`,
  )
  record(
    'M4',
    '抽屉宽度铺满视口',
    panel ? Math.abs(panel.width - (vp?.width ?? 0)) < 2 : false,
    `w=${panel?.width}`,
  )

  const roleBtn = await page.locator('.members__role').first().boundingBox()
  record('M5', '权限切换按钮触控高度 ≥44px', (roleBtn?.height ?? 0) >= 44, `h=${roleBtn?.height}`)

  const transferBtn = await page.locator('.members__transfer').first().boundingBox()
  record('M6', '「转移房主」按钮触控高度 ≥44px', (transferBtn?.height ?? 0) >= 44, `h=${transferBtn?.height}`)

  // 抽屉在 body 下（Teleport），不受顶栏/画布 overflow 裁剪
  const teleported = await page.evaluate(() => {
    const el = document.querySelector('.members')
    return el ? el.parentElement === document.body : false
  })
  record('M7', '面板 Teleport 到 body（不被容器 overflow 裁剪）', teleported)

  // viewer 视角：无管理按钮
  const ctxB = await browser.newContext({
    viewport: { width: 390, height: 844 },
    isMobile: true,
    hasTouch: true,
  })
  await ctxB.addInitScript(() => window.localStorage.setItem('codraw.name', '访客乙'))
  const B = await ctxB.newPage()
  await ctxB.routeWebSocket(/\/ws(\?|$)/, (ws) => {
    const url = new URL(ws.url())
    const cid = url.searchParams.get('cid') ?? ''
    const me = {
      clientId: cid,
      userId: `u_${cid}`,
      name: '访客乙',
      color: '#e8590c',
      role: 'viewer',
      isOwner: false,
    }
    ws.send(
      frame(code, 'joined', {
        you: me,
        room: { code, title: '移动端验收', epoch: 1, seq: 1 },
        online: 2,
        instanceId: 'i_mock',
        limits: LIMITS,
        ownerClientId: 'c_owner_other',
      }),
    )
    ws.send(frame(code, 'presence.join', { users: [me], online: 2 }))
  })

  await B.goto(`${WEB}/r/${code}`, { waitUntil: 'domcontentloaded' })
  await B.waitForFunction(() => !!document.querySelector('.room__me'), null, { timeout: 20000 })
  await B.locator('.online').first().click()
  await B.waitForSelector('.members', { timeout: 5000 })
  await B.waitForTimeout(500)
  record('M8', 'viewer 打开面板没有任何管理按钮', (await B.locator('.members__role-group').count()) === 0)
  record('M9', 'viewer 面板里仍能看到角色标记', (await B.locator('.badge--viewer').count()) > 0)

  await B.screenshot({ path: 'scripts/mobile-viewer-members.png' })
  await page.screenshot({ path: 'scripts/mobile-owner-members.png' })

  await browser.close()

  const passed = results.filter((r) => r.ok).length
  console.log(`\n========== ${passed}/${results.length} passed ==========`)
  process.exit(passed === results.length ? 0 : 1)
}

main().catch((err) => {
  console.error(err)
  process.exit(2)
})
