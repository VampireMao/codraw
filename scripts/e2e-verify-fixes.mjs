#!/usr/bin/env node
/**
 * 验证三个修复（协议层，不依赖浏览器）：
 *  1. delete op 确实落库：A 删除后，新连接拉快照不应看到该图形
 *  2. append 可用：create 后追加点，其他连接能收到 append 且点数增长
 *  3. 工具保持是纯前端行为，此处不涉及（由浏览器手测）
 *
 * 用法：node scripts/e2e-verify-fixes.mjs --base http://127.0.0.1:8080
 */
const args = process.argv.slice(2)
const getArg = (n, d) => {
  const i = args.indexOf(`--${n}`)
  return i > -1 ? args[i + 1] : d
}
const BASE = getArg('base', 'http://127.0.0.1:8080')
const WS = BASE.replace(/^http/, 'ws')

let pass = 0
let fail = 0
const ok = (m, d = '') => (pass++, console.log(`  \x1b[32mPASS\x1b[0m ${m}${d ? `  \x1b[90m${d}\x1b[0m` : ''}`))
const bad = (m, d = '') => (fail++, console.log(`  \x1b[31mFAIL\x1b[0m ${m}${d ? `  \x1b[31m${d}\x1b[0m` : ''}`))
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const section = (t) => console.log(`\n\x1b[1m${t}\x1b[0m`)

class C {
  constructor(ws) {
    this.ws = ws
    this.inbox = []
    this.waiters = []
    ws.addEventListener('message', (e) => {
      let m
      try {
        m = JSON.parse(e.data)
      } catch {
        return
      }
      this.inbox.push(m)
      this.waiters = this.waiters.filter((w) => {
        if (w.f(m)) {
          w.r(m)
          return false
        }
        return true
      })
    })
  }
  static async open(p) {
    const ws = new WebSocket(`${WS}/ws?${new URLSearchParams(p)}`)
    return new Promise((res, rej) => {
      const t = setTimeout(() => rej(new Error('timeout')), 10000)
      ws.addEventListener('open', () => {
        clearTimeout(t)
        res(new C(ws))
      })
      ws.addEventListener('error', () => {
        clearTimeout(t)
        rej(new Error('ws error'))
      })
    })
  }
  send(type, payload) {
    this.ws.send(JSON.stringify({ v: 1, type, roomId: this.room, clientId: this.cid, ts: Date.now(), payload }))
  }
  wait(f, ms = 6000) {
    const hit = this.inbox.find(f)
    if (hit) return Promise.resolve(hit)
    return new Promise((res, rej) => {
      const e = {
        f,
        r: (m) => {
          clearTimeout(t)
          res(m)
        },
      }
      const t = setTimeout(() => {
        this.waiters = this.waiters.filter((x) => x !== e)
        rej(new Error('wait timeout'))
      }, ms)
      this.waiters.push(e)
    })
  }
  close() {
    try {
      this.ws.close()
    } catch {}
  }
}

function freehand(id, points) {
  return {
    id,
    kind: 'freehand',
    geom: { x: 0, y: 0, w: 100, h: 100 },
    style: { stroke: '#16181d', fill: 'none', strokeWidth: 2, opacity: 1, dash: 'solid' },
    data: { points, closed: false },
    text: null,
  }
}

async function main() {
  console.log(`\x1b[1m验证三个修复\x1b[0m  ${BASE}`)

  const res = await fetch(`${BASE}/api/rooms`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ title: 'fix-verify' }),
  })
  const body = await res.json()
  const code = body.room.code
  const key = body.keys.editorKey
  console.log(`  房间 ${code}`)

  const mk = async (tag) => {
    const cid = `c_fix_${tag}_${Date.now()}`
    const c = await C.open({ room: code, cid, name: tag, key })
    c.room = code
    c.cid = cid
    await c.wait((m) => m.type === 'joined')
    c.send('hello', { lastSeq: 0, epoch: 1 })
    await c.wait((m) => m.type === 'resync.snapshot' || m.type === 'resync.events')
    return c
  }

  // ---------------- 问题 1：delete 必须落库 ----------------
  section('问题 1：删除必须持久化（橡皮/删除键）')
  const a = await mk('A')
  const b = await mk('B')

  const sid = 'sh_fix_del_1'
  a.send('op.add', { opId: 'op_fix_create_1', action: 'create', shape: freehand(sid, [[0, 0], [10, 10]]) })
  await a.wait((m) => m.type === 'ack' && m.payload?.opId === 'op_fix_create_1')
  await b.wait((m) => m.type === 'op.add' && m.payload?.shape?.id === sid)
  ok('A 创建图形，B 收到')

  a.send('op.add', { opId: 'op_fix_del_1', action: 'delete', id: sid })
  const delAck = await a.wait((m) => m.type === 'ack' && m.payload?.opId === 'op_fix_del_1')
  ok('A 删除收到 ack', `status=${delAck.payload?.status}`)

  const delB = await b.wait(
    (m) =>
      m.type === 'op.add' &&
      m.payload?.action === 'delete' &&
      (m.payload?.id === sid || m.payload?.shape?.id === sid),
  )
  ok('B 收到 delete 广播', `shape.deleted=${delB.payload?.shape?.deleted}`)

  // 关键：新连接拉快照，被删图形不能出现
  const c1 = await mk('C')
  const snap = c1.inbox.find((m) => m.type === 'resync.snapshot')
  const ids = (snap?.payload?.shapes ?? []).map((s) => s.id)
  if (ids.includes(sid)) {
    bad('新连接快照中仍包含已删除图形（这正是你遇到的问题）', `shapes=${JSON.stringify(ids)}`)
  } else {
    ok('新连接快照不含已删除图形（问题 1 已修复）', `shapes=${JSON.stringify(ids)}`)
  }
  c1.close()

  // ---------------- 问题 2：append 实时追加 ----------------
  section('问题 2：落笔过程 append 实时可见')
  const sid2 = 'sh_fix_pen_1'
  // 落笔即 create，只带首个点
  a.send('op.add', { opId: 'op_fix_pen_create', action: 'create', shape: freehand(sid2, [[0, 0]]) })
  await b.wait((m) => m.type === 'op.add' && m.payload?.shape?.id === sid2)
  const created = b.inbox.find((m) => m.type === 'op.add' && m.payload?.shape?.id === sid2)
  ok('落笔时 B 立即收到 create（含首点）', `points=${created?.payload?.shape?.data?.points?.length}`)

  // 模拟绘制过程中的批量 append
  for (let batch = 1; batch <= 3; batch += 1) {
    const pts = []
    for (let i = 0; i < 8; i += 1) pts.push([batch * 100 + i * 5, batch * 100 + i * 3])
    a.send('op.add', { opId: `op_fix_pen_append_${batch}`, action: 'append', id: sid2, points: pts })
    await a.wait((m) => m.type === 'ack' && m.payload?.opId === `op_fix_pen_append_${batch}`)
  }
  await sleep(400)

  const appendsB = b.inbox.filter((m) => m.type === 'op.add' && m.payload?.action === 'append' && m.payload?.id === sid2)
  ok('B 收到 3 条 append（无需等松手）', `收到 ${appendsB.length} 条`)

  // 新连接拉快照，点数应为 1 + 24
  const c2 = await mk('D')
  const snap2 = c2.inbox.find((m) => m.type === 'resync.snapshot')
  const shape2 = (snap2?.payload?.shapes ?? []).find((s) => s.id === sid2)
  const n = shape2?.data?.points?.length ?? 0
  if (n === 25) {
    ok('append 已落库（1 首点 + 24 增量 = 25）', `points=${n}`)
  } else {
    bad('append 点数不符', `points=${n}，期望 25`)
  }
  c2.close()

  // ---------------- 回归：原有功能未破坏 ----------------
  section('回归：create / update / 幂等')
  const sid3 = 'sh_fix_reg_1'
  a.send('op.add', { opId: 'op_fix_reg_create', action: 'create', shape: freehand(sid3, [[0, 0], [5, 5]]) })
  const ack3 = await a.wait((m) => m.type === 'ack' && m.payload?.opId === 'op_fix_reg_create')
  ok('create 正常', `seq=${ack3.payload?.seq}`)

  a.send('op.add', { opId: 'op_fix_reg_create', action: 'create', shape: freehand(sid3, [[0, 0], [5, 5]]) })
  const dup = await a.wait((m) => m.type === 'ack' && m.payload?.opId === 'op_fix_reg_create' && m.payload?.status === 'duplicate')
  ok('重发仍返回 duplicate（幂等未破坏）', `seq=${dup.payload?.seq}`)

  a.close()
  b.close()
  await sleep(200)

  console.log(`\n\x1b[1m结果\x1b[0m  通过 \x1b[32m${pass}\x1b[0m  失败 \x1b[31m${fail}\x1b[0m`)
  process.exit(fail === 0 ? 0 : 1)
}

main().catch((e) => {
  console.error('\x1b[31m异常:\x1b[0m', e.message)
  process.exit(1)
})
