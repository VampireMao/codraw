#!/usr/bin/env node
/**
 * 端到端复现用户流程（真实浏览器，走真实前端逻辑）：
 *  1. A 建房画两笔
 *  2. B 进房
 *  3. A 关闭页面，等房主转移给 B
 *  4. A 重新打开同一房间 → 应看到历史内容，且权限为 viewer
 */
import { createRequire } from 'node:module'
const require = createRequire('/mnt/d/Work/AI-TMP/codraw/web/')
const { chromium } = require('playwright')

const BASE = process.argv[2]
const LEAVE_WAIT = Number(process.argv[3] || 4000)

let pass = 0
let fail = 0
const ok = (m, d = '') => (pass++, console.log(`  \x1b[32mPASS\x1b[0m ${m}${d ? `  \x1b[90m${d}\x1b[0m` : ''}`))
const bad = (m, d = '') => (fail++, console.log(`  \x1b[31mFAIL\x1b[0m ${m}${d ? `  \x1b[31m${d}\x1b[0m` : ''}`))
const section = (t) => console.log(`\n\x1b[1m${t}\x1b[0m`)

const browser = await chromium.launch()

// A 用一个持久化的 context（保留 IndexedDB，模拟同一浏览器重新打开）
const ctxA = await browser.newContext({ viewport: { width: 1200, height: 800 } })
const ctxB = await browser.newContext({ viewport: { width: 1200, height: 800 } })

const readState = (page) =>
  page.evaluate(() => {
    const app = document.querySelector('#app')?.__vue_app__
    const pinia = app?.config?.globalProperties?.$pinia
    const b = pinia?.state?.value?.board
    return {
      role: b?.role,
      owner: b?.ownerClientId,
      shapes: (b?.shapes ?? []).length,
      canEdit: b?.canEdit,
      appliedSeq: b?.appliedSeq,
    }
  })

async function enterRoom(page, code, key) {
  await page.goto(`${BASE}/r/${code}?k=${key}`, { waitUntil: 'networkidle' })
  await page.waitForTimeout(2200)
  if (await page.locator('#codraw-name').isVisible().catch(() => false)) {
    await page.locator('#codraw-name').fill('用户A')
    await page.locator('button[type="submit"]').click()
    await page.waitForTimeout(2200)
  }
}

// 建房
const res = await fetch(`${BASE}/api/rooms`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ title: 'e2e' }),
})
const body = await res.json()
const code = body.room.code
const key = body.keys.editorKey
console.log(`\x1b[1m端到端复现\x1b[0m  房间 ${code}`)

section('1. A 建房并画两笔')
const pageA = await ctxA.newPage()
await enterRoom(pageA, code, key)
let st = await readState(pageA)
console.log(`   A: role=${st.role} shapes=${st.shapes}`)
st.role === 'owner' ? ok('A 是房主') : bad('A 不是房主', st.role)

// 画两笔
await pageA.keyboard.press('p')
await pageA.waitForTimeout(300)
const box = await pageA.locator('canvas').first().boundingBox()
const cx = box.x + box.width / 2
const cy = box.y + box.height / 2
for (const dy of [-100, 60]) {
  await pageA.mouse.move(cx - 150, cy + dy)
  await pageA.mouse.down()
  for (let i = 0; i <= 18; i += 1) {
    await pageA.mouse.move(cx - 150 + i * 9, cy + dy + i * 3)
    await pageA.waitForTimeout(20)
  }
  await pageA.mouse.up()
  await pageA.waitForTimeout(800)
}
st = await readState(pageA)
console.log(`   画完后 shapes=${st.shapes} appliedSeq=${st.appliedSeq}`)
st.shapes === 2 ? ok('A 画了 2 个图形') : bad('图形数不对', String(st.shapes))

section('2. B 进入')
const pageB = await ctxB.newPage()
await pageB.goto(`${BASE}/r/${code}`, { waitUntil: 'networkidle' })
await pageB.waitForTimeout(2200)
if (await pageB.locator('#codraw-name').isVisible().catch(() => false)) {
  await pageB.locator('#codraw-name').fill('用户B')
  await pageB.locator('button[type="submit"]').click()
  await pageB.waitForTimeout(2200)
}
const stB = await readState(pageB)
console.log(`   B: role=${stB.role} shapes=${stB.shapes}`)
stB.role === 'viewer' ? ok('B 默认只读') : bad('B 权限异常', stB.role)
stB.shapes === 2 ? ok('B 能看到 2 个图形') : bad('B 看不到内容', String(stB.shapes))

section('3. A 关闭页面，等待房主转移')
await pageA.close()
console.log(`   等待 ${LEAVE_WAIT}ms 让 grace window 过期...`)
await pageB.waitForTimeout(LEAVE_WAIT)

const stB2 = await readState(pageB)
console.log(`   B: role=${stB2.role} owner=${stB2.owner}`)
stB2.role === 'owner' ? ok('B 已成为房主') : bad('B 未接任房主', stB2.role)

section('4. A 重新打开同一房间（关键）')
const pageA2 = await ctxA.newPage()
await enterRoom(pageA2, code, key)
const stA2 = await readState(pageA2)
console.log(`   A: role=${stA2.role} shapes=${stA2.shapes} appliedSeq=${stA2.appliedSeq}`)
console.log(`   owner=${stA2.owner}`)

stA2.role === 'viewer' ? ok('A 重进后是 viewer（不再是 editor）', stA2.role) : bad('A 重进后权限错误', stA2.role)
stA2.shapes === 2
  ? ok('A 重进后能看到历史内容（2 个图形）', `shapes=${stA2.shapes}`)
  : bad('A 重进后看不到历史内容', `shapes=${stA2.shapes}`)

// canEdit 是 computed，不在 pinia state 里；改从 UI 判断：viewer 不应看到工具栏
const toolbarVisible = await pageA2.locator('.toolbar').isVisible().catch(() => false)
const readonlyBanner = await pageA2
  .locator('body')
  .innerText()
  .then((t) => t.includes('只读'))
  .catch(() => false)
console.log(`   工具栏可见=${toolbarVisible}  只读提示=${readonlyBanner}`)
!toolbarVisible ? ok('viewer 看不到工具栏（无法编辑）') : bad('viewer 仍看到工具栏')
readonlyBanner ? ok('显示只读提示') : bad('缺少只读提示')

section('5. 对照：A 刷新页面（同一标签页）')
await pageA2.reload({ waitUntil: 'networkidle' })
await pageA2.waitForTimeout(2800)
if (await pageA2.locator('#codraw-name').isVisible().catch(() => false)) {
  await pageA2.locator('#codraw-name').fill('用户A')
  await pageA2.locator('button[type="submit"]').click()
  await pageA2.waitForTimeout(2200)
}
const stA3 = await readState(pageA2)
console.log(`   刷新后: role=${stA3.role} shapes=${stA3.shapes}`)
stA3.shapes === 2 ? ok('刷新后仍能看到内容') : bad('刷新后内容丢失', String(stA3.shapes))

console.log(`\n\x1b[1m结果\x1b[0m  通过 \x1b[32m${pass}\x1b[0m  失败 \x1b[31m${fail}\x1b[0m`)
await browser.close()
process.exit(fail === 0 ? 0 : 1)
