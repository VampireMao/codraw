/**
 * 权限模型前端验收（PERMISSIONS §9 的 P2/P4/P5/P6/P7）。
 *
 * 为什么用 WS mock：
 *   仓库里的后端目前只有协议常量（`protocol.go` 的 TypeRoomGrant / TypeMemberRole），
 *   还没有 room.grant / room.transfer / member.role 的处理路径，也无法自动移交房主。
 *   所以这里按 `docs/PERMISSIONS.md` 的契约实现一个**最小协议对端**，
 *   让真实的前端代码（store / realtime / UI）完整跑一遍。
 *
 *   => 本脚本证明的是「前端行为符合契约」，**不代表后端已实现**。
 *      真实后端联调结论以「待联调」记录。
 *
 * 用法：
 *   node scripts/verify-permissions.mjs [baseUrl]
 * 默认 baseUrl = http://127.0.0.1:5173（vite dev，后端 REST 需可用）。
 */
import { chromium } from 'playwright'

const BASE = process.argv[2] ?? 'http://127.0.0.1:5173'
/** 建房限流是按连接/IP 的，vite 代理和后端分开计数；直连后端更稳 */
const API_BASE = process.env.CODRAW_API ?? 'http://localhost:8099'
const results = []

function record(id, title, ok, detail = '') {
  results.push({ id, title, ok, detail })
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${id}  ${title}${detail ? `  [${detail}]` : ''}`)
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function waitFor(page, selector, { visible = true, timeout = 12000 } = {}) {
  const deadline = Date.now() + timeout
  while (Date.now() < deadline) {
    const locator = page.locator(selector)
    const count = await locator.count()
    if (count > 0 === visible) {
      if (!visible) return true
      if (
        await locator
          .first()
          .isVisible()
          .catch(() => false)
      )
        return true
    }
    await sleep(120)
  }
  return false
}

async function waitForText(page, selector, text, timeout = 8000) {
  const deadline = Date.now() + timeout
  while (Date.now() < deadline) {
    const t = await page
      .locator(selector)
      .first()
      .innerText()
      .catch(() => '')
    if (t.includes(text)) return true
    await sleep(120)
  }
  return false
}

async function badgeText(page) {
  return page
    .locator('.room__me .badge')
    .first()
    .innerText()
    .catch(() => '')
}

async function createRoomViaApi() {
  // 优先直连后端（vite 代理可能和后端限流叠加）；失败再退回 BASE 代理
  const endpoints = [API_BASE, BASE].filter(Boolean)
  let lastError
  for (const endpoint of endpoints) {
    for (let attempt = 0; attempt < 3; attempt += 1) {
      const res = await fetch(`${endpoint}/api/rooms`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title: '权限验收' }),
      })
      if (res.ok) return res.json()
      const body = (await res.text()).slice(0, 120)
      lastError = `${res.status} ${body}`
      if (res.status === 429) {
        // 建房限流：等窗口过去再试
        await sleep(4000)
        continue
      }
      break
    }
  }
  throw new Error(`创建房间失败: ${lastError}`)
}

/** 预置昵称：跳过首次进入的昵称引导，避免 <dialog> 遮罩干扰后续点击。 */
async function seedName(context, name) {
  await context.addInitScript((value) => {
    try {
      window.localStorage.setItem('codraw.name', value)
    } catch {
      /* 隐私模式 */
    }
  }, name)
}

async function joinRoom(page, code) {
  await page.goto(`${BASE}/r/${code}`, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction(() => !!document.querySelector('.room__me'), null, { timeout: 20000 })
  const blocked = !(await waitFor(page, 'dialog[open]', { visible: false, timeout: 4000 }))
  if (blocked) throw new Error('昵称弹窗未关闭，后续交互会被遮罩拦截')
}

/* --------------------------- 最小协议对端（PERMISSIONS） --------------------------- */

const COLORS = ['#3d5afe', '#e8590c', '#12b886', '#7048e8', '#e64980']

function createRoomMock(code) {
  /** cid → { cid, name, role, color, order, ws, alive } */
  const members = new Map()
  let seq = 0
  let ownerCid = null
  let joinCounter = 0
  const opsByCid = new Map()
  /** 断线宽限计时器：同一 cid 短时间重连不触发移交（PERMISSIONS §2.4 / §4.4） */
  const leaveTimers = new Map()

  const frame = (type, payload, envelopeSeq) =>
    JSON.stringify({
      v: 1,
      type,
      roomId: code,
      clientId: 'srv',
      ts: Date.now(),
      ...(envelopeSeq !== undefined ? { seq: envelopeSeq } : {}),
      payload,
    })

  const user = (m) => ({
    clientId: m.cid,
    userId: `u_${m.cid.slice(0, 6)}`,
    name: m.name,
    color: m.color,
    role: m.role,
  })

  const users = () =>
    [...members.values()]
      .filter((m) => m.alive)
      .sort((a, b) => a.order - b.order)
      .map(user)

  function send(member, type, payload, envelopeSeq) {
    if (!member.alive) return
    try {
      member.ws.send(frame(type, payload, envelopeSeq))
    } catch {
      /* 已关闭 */
    }
  }

  function broadcast(type, payload, envelopeSeq) {
    for (const m of members.values()) send(m, type, payload, envelopeSeq)
  }

  function broadcastPresence() {
    const list = users()
    broadcast('presence.join', { users: list, online: list.length })
  }

  /** member.role：带 seq 的持久消息（PERMISSIONS §3.1） */
  function broadcastRole(cid, role, grantedBy) {
    seq += 1
    broadcast('member.role', { clientId: cid, role, ...(grantedBy ? { grantedBy } : {}) }, seq)
  }

  function join({ ws, cid, name }) {
    // 同名 cid 在宽限期内重连：取消待移交，恢复原角色
    const pending = leaveTimers.get(cid)
    if (pending) {
      clearTimeout(pending)
      leaveTimers.delete(cid)
    }
    const existing = members.get(cid)
    if (existing) {
      // 同一 cid 重新加入（改名会重建会话）：恢复原角色（PERMISSIONS §4.4）
      existing.alive = true
      existing.ws = ws
      existing.name = name
    } else {
      joinCounter += 1
      // §4.2：无主房间的第一个连接成为 owner，其余默认 viewer
      const role = ownerCid === null ? 'owner' : 'viewer'
      if (role === 'owner') ownerCid = cid
      members.set(cid, {
        cid,
        name,
        role,
        color: COLORS[(joinCounter - 1) % COLORS.length],
        order: joinCounter,
        ws,
        alive: true,
      })
    }
    const me = members.get(cid)
    if (me.role === 'owner') ownerCid = cid

    send(me, 'joined', {
      you: { ...user(me), isOwner: me.role === 'owner' },
      room: { code, title: '权限验收', epoch: 1, seq },
      online: users().length,
      instanceId: 'i_mock',
      limits: {
        maxMessageBytes: 65536,
        maxShapeBytes: 32768,
        maxShapesPerRoom: 20000,
        opsPerSecond: 60,
        opsBurst: 120,
        cursorPerSecond: 20,
        resumeMaxOps: 2000,
        resumeMaxBytes: 4194304,
      },
      serverTime: Date.now(),
      ownerClientId: ownerCid,
    })
    broadcastPresence()

    ws.onMessage((raw) => {
      let msg
      try {
        msg = JSON.parse(String(raw))
      } catch {
        return
      }
      const sender = members.get(cid)
      if (!sender) return

      if (msg.type === 'op.add') {
        opsByCid.set(cid, (opsByCid.get(cid) ?? 0) + 1)
        return
      }
      if (msg.type === 'room.grant') {
        const { targetClientId, role } = msg.payload ?? {}
        if (sender.role !== 'owner') {
          send(sender, 'error', { code: 'forbidden', message: '只有房主可以授权', fatal: true })
          return
        }
        const target = members.get(targetClientId)
        if (!target?.alive) {
          send(sender, 'error', { code: 'target_offline', message: '目标不在线', fatal: false })
          return
        }
        if (role !== 'editor' && role !== 'viewer') {
          send(sender, 'error', { code: 'invalid_grant', message: '非法角色', fatal: false })
          return
        }
        target.role = role
        broadcastRole(targetClientId, role, cid)
        return
      }
      if (msg.type === 'room.transfer') {
        const { targetClientId } = msg.payload ?? {}
        if (sender.role !== 'owner') {
          send(sender, 'error', { code: 'forbidden', message: '只有房主可以转移', fatal: true })
          return
        }
        const target = members.get(targetClientId)
        if (!target?.alive) {
          send(sender, 'error', { code: 'target_offline', message: '目标不在线', fatal: false })
          return
        }
        sender.role = 'editor'
        target.role = 'owner'
        ownerCid = targetClientId
        // 目标先成 owner，原 owner 再降级，客户端的兜底逻辑也覆盖这个顺序
        broadcastRole(targetClientId, 'owner', cid)
        broadcastRole(cid, 'editor', targetClientId)
        return
      }
    })

    ws.onClose(() => {
      // 宽限期：改名/刷新会短暂断连，不应立刻移交房主（PERMISSIONS §2.4）
      const timer = setTimeout(() => {
        leaveTimers.delete(cid)
        leave(cid)
      }, 600)
      leaveTimers.set(cid, timer)
    })
  }

  function leave(cid) {
    const member = members.get(cid)
    if (!member || !member.alive) return
    member.alive = false
    const wasOwner = member.role === 'owner' || ownerCid === cid
    if (wasOwner) {
      // §2.2：按入场顺序移交给下一个在线成员
      const next = [...members.values()]
        .filter((m) => m.alive && m.cid !== cid)
        .sort((a, b) => a.order - b.order)[0]
      ownerCid = next?.cid ?? null
      if (next) next.role = 'owner'
    }
    broadcastPresence()
    if (wasOwner && ownerCid) broadcastRole(ownerCid, 'owner')
  }

  /** 强制离开（测试脚本关闭页面时调用，跳过宽限期） */
  function forceLeave(cid) {
    const timer = leaveTimers.get(cid)
    if (timer) {
      clearTimeout(timer)
      leaveTimers.delete(cid)
    }
    leave(cid)
  }

  return {
    join,
    leave,
    forceLeave,
    opsOf: (cid) => opsByCid.get(cid) ?? 0,
  }
}

/** 给一个 context 装上 mock WS；返回该 context 里出现过的 cid 列表。 */
async function installMock(context, room) {
  const cids = []
  await context.routeWebSocket(/\/ws(\?|$)/, (ws) => {
    const url = new URL(ws.url())
    const cid = url.searchParams.get('cid') ?? ''
    const name = url.searchParams.get('name') ?? '访客'
    if (cid) cids.push(cid)
    room.join({ ws, cid, name })
  })
  return { cids }
}

async function launchBrowser() {
  for (const attempt of [
    () => chromium.launch(),
    () => chromium.launch({ channel: 'msedge' }),
    () => chromium.launch({ channel: 'chrome' }),
  ]) {
    try {
      return await attempt()
    } catch {
      /* 试下一个 */
    }
  }
  throw new Error('没有可用的浏览器（尝试过 playwright chromium / msedge / chrome）')
}

/* ---------------------------------- 主流程 ---------------------------------- */

async function main() {
  console.log(`base: ${BASE}`)
  console.log('注意：后端尚未实现 room.grant / room.transfer / member.role，')
  console.log('      本脚本用协议 mock 驱动真实前端，结论仅覆盖前端行为。\n')

  const browser = await launchBrowser()
  const ctxA = await browser.newContext({ viewport: { width: 1440, height: 900 } })
  const ctxB = await browser.newContext({ viewport: { width: 1440, height: 900 } })
  const A = await ctxA.newPage()
  const B = await ctxB.newPage()

  const errors = []
  A.on('pageerror', (e) => errors.push(`A: ${e.message}`))
  B.on('pageerror', (e) => errors.push(`B: ${e.message}`))

  const roomInfo = await createRoomViaApi()
  const code = roomInfo.room.code
  const room = createRoomMock(code)
  const mockA = await installMock(ctxA, room)
  const mockB = await installMock(ctxB, room)
  // 预置昵称，跳过首次引导（否则 <dialog> 遮罩会拦截后续所有点击）
  await seedName(ctxA, '房主甲')
  await seedName(ctxB, '访客乙')
  console.log(`room: ${code}\n`)

  // ---------------- P1 前置：创建者成为房主 ----------------
  await joinRoom(A, code)
  await waitFor(A, '.room__me .badge')
  const ownerBadge = await badgeText(A)
  record('P1', '创建者显示「房主」徽标', ownerBadge.includes('房主'), `badge=${ownerBadge.trim()}`)
  const ownerToolbar = (await A.locator('.toolbar').count()) > 0
  record('P1b', '房主拥有完整工具栏', ownerToolbar)

  // ---------------- P2：填房号进入 → viewer ----------------
  await joinRoom(B, code)
  await waitFor(B, '.room__ro-banner')
  const banner = await B.locator('.room__ro-banner')
    .innerText()
    .catch(() => '')
  record(
    'P2a',
    'viewer 显示只读横幅（新文案）',
    banner.includes('只读模式 · 房主可授予你编辑权限'),
    banner.trim(),
  )
  record('P2b', 'viewer 不渲染绘图工具栏', (await B.locator('.toolbar').count()) === 0)
  record(
    'P2c',
    'viewer 顶栏显示「只读」徽标',
    (await badgeText(B)).includes('只读'),
    (await badgeText(B)).trim(),
  )

  // ---------------- P4：房主授权 B 为 editor ----------------
  await A.locator('.online').first().click()
  const panelOpen = await waitFor(A, '.members')
  record('P4a', '房主可打开成员面板', panelOpen)
  const rowCount = await A.locator('.members__row').count()
  record('P4b', '成员面板列出所有在线成员', rowCount >= 2, `rows=${rowCount}`)

  const bRow = A.locator('.members__row', { hasText: '访客乙' }).first()
  const grantBtn = bRow.getByRole('button', { name: '可编辑' })
  const hasGrant = (await grantBtn.count()) > 0
  record('P4c', '房主看到每个成员的角色切换按钮', hasGrant)
  if (hasGrant) {
    await grantBtn.click()
    const toolbarShown = await waitFor(B, '.toolbar', { timeout: 6000 })
    record('P4d', 'B 被授权后工具栏立即出现', toolbarShown)
    record('P4e', 'B 收到「你已获得编辑权限」toast', await waitForText(B, '.co-toast', '你已获得编辑权限'))
    record('P4f', 'B 的只读横幅消失', await waitFor(B, '.room__ro-banner', { visible: false, timeout: 4000 }))
    record(
      'P4g',
      'B 顶栏徽标消失（editor 无徽标）',
      (await badgeText(B)) === '',
      `badge=${(await badgeText(B)).trim()}`,
    )

    // 真的画一笔 → 前端必须真的发出 op.add（证明 canEdit 生效）
    await B.locator('.toolbar button[aria-label="画笔"]').click()
    const canvas = B.locator('canvas.codraw-canvas--base').first()
    const box = await canvas.boundingBox()
    if (box) {
      await B.mouse.move(box.x + 240, box.y + 240)
      await B.mouse.down()
      for (let i = 1; i <= 10; i += 1) {
        await B.mouse.move(box.x + 240 + i * 9, box.y + 240 + i * 5)
        await sleep(18)
      }
      await B.mouse.up()
      await sleep(700)
    }
    // 通过 mock 侧的 op 计数确认（前端确实提交了写操作）
    const bCid = mockB.cids.at(-1)
    record('P4h', 'B 能实际绘制（发出 op.add）', room.opsOf(bCid) > 0, `ops=${room.opsOf(bCid)}`)
    record('P4i', 'B 的空态提示消失', (await B.locator('.board-canvas__hint').count()) === 0)
  }

  // ---------------- P5：撤销 B ----------------
  const revokeBtn = bRow.getByRole('button', { name: '只读' })
  if ((await revokeBtn.count()) > 0) {
    await revokeBtn.click()
    record('P5a', 'B 被撤销后工具栏立即消失', await waitFor(B, '.toolbar', { visible: false, timeout: 6000 }))
    record('P5b', 'B 重新显示只读横幅', await waitFor(B, '.room__ro-banner', { timeout: 4000 }))
    record('P5c', 'B 收到「你已被设为只读」toast', await waitForText(B, '.co-toast', '你已被设为只读'))
    record('P5d', 'B 的连接未断开', (await B.locator('.room').count()) > 0)
  } else {
    record('P5', '房主撤销 B 的编辑权限', false, '未找到「只读」按钮')
  }

  // ---------------- P6：转移房主 ----------------
  const transferBtn = bRow.getByRole('button', { name: '转移房主' })
  if ((await transferBtn.count()) > 0) {
    await transferBtn.click()
    const bOwner = await waitForText(B, '.room__me .badge', '房主', 6000)
    record('P6a', 'B 成为房主（顶栏「房主」徽标）', bOwner, (await badgeText(B)).trim())
    record('P6b', 'B 收到「你已成为房主」toast', await waitForText(B, '.co-toast', '你已成为房主'))

    await B.locator('.online').first().click()
    const bPanel = await waitFor(B, '.members')
    const bManage = bPanel && (await B.locator('.members__role-group').count()) > 0
    record('P6c', 'B 出现成员管理按钮', bManage)

    const aBadge = await badgeText(A)
    record('P6d', '原房主 A 失去「房主」徽标', !aBadge.includes('房主'), `badge=${aBadge.trim()}`)
    record(
      'P6e',
      '原房主 A 的管理入口消失（chevron 不再渲染）',
      (await A.locator('.online__chev').count()) === 0,
    )
    const aToolbar = (await A.locator('.toolbar').count()) > 0
    record('P6f', '原房主 A 仍可编辑（降为 editor）', aToolbar)
    await B.keyboard.press('Escape')
    await A.keyboard.press('Escape')
  } else {
    record('P6', '转移房主', false, '未找到「转移房主」按钮')
  }

  // ---------------- P7：房主关闭页面 → 按入场顺序移交 ----------------
  // 此刻 owner 是 B（若 P6 成功）；关掉当前 owner，检查另一方接任
  const aIsOwner = (await badgeText(A)).includes('房主')
  const bIsOwner = (await badgeText(B)).includes('房主')
  if (aIsOwner || bIsOwner) {
    const closing = aIsOwner ? A : B
    const closingTag = aIsOwner ? 'A' : 'B'
    const survivor = aIsOwner ? B : A
    const survivorTag = aIsOwner ? 'B' : 'A'

    // 关闭前先让 mock 知道谁要走（tab 关闭时 WS 可能来不及 onClose）
    const closingCids = aIsOwner ? mockA.cids : mockB.cids
    const survivorCids = aIsOwner ? mockB.cids : mockA.cids
    await closing.close()
    for (const cid of closingCids) room.forceLeave(cid)

    const gotOwner = await (async () => {
      const deadline = Date.now() + 12000
      while (Date.now() < deadline) {
        if ((await badgeText(survivor).catch(() => '')).includes('房主')) return true
        await sleep(150)
      }
      return false
    })()
    record(
      'P7a',
      `${closingTag} 关闭页面后 ${survivorTag} 自动接任房主`,
      gotOwner,
      `badge=${(await badgeText(survivor).catch(() => '')).trim()}`,
    )
    record(
      'P7b',
      `${survivorTag} 收到「你已成为房主」toast`,
      await waitForText(survivor, '.co-toast', '你已成为房主'),
    )
    record('P7c', `${survivorTag} 出现成员管理入口`, (await survivor.locator('.online__chev').count()) > 0)

    const survivorCid = survivorCids.at(-1)
    record('P7d', '房主身份落在仍在线的成员上', gotOwner && !!survivorCid)
  } else {
    record('P7', '房主关闭后自动移交', false, '前置失败：没有房主')
  }

  if (errors.length) {
    console.log('\n--- page errors ---')
    for (const e of errors) console.log('  ' + e)
  }

  await browser.close()

  const passed = results.filter((r) => r.ok).length
  console.log(`\n========== ${passed}/${results.length} passed ==========`)
  const failed = results.filter((r) => !r.ok)
  if (failed.length) {
    console.log('FAILED:')
    for (const f of failed) console.log(`  - ${f.id} ${f.title} ${f.detail}`)
  }
  process.exit(failed.length === 0 ? 0 : 1)
}

main().catch((err) => {
  console.error(err)
  process.exit(2)
})
