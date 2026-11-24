#!/usr/bin/env node
/**
 * CoDraw · 跨实例同步验收（--scale app=2）
 *
 * 原理：
 *   1. 经 Caddy（edge:8080）建房间，拿到 code 与 editorKey。
 *   2. 连续建立 N 个连接，记录每个连接实际落到了哪个实例（读 joined.payload.instanceId）。
 *   3. 断言：确实命中了 ≥2 个不同实例（否则测试无意义，必须报错而不是"通过"）。
 *   4. 在实例 X 上画一笔，断言实例 Y 上的连接收到广播，seq 一致。
 *   5. 双向各测一次（X→Y 与 Y→X），确保不是单向。
 *
 * 用法：
 *   node scripts/e2e-multi-instance.mjs [--base http://localhost:8080] [--conns 8]
 *
 * 前置：
 *   docker compose up -d --build --scale app=2
 *   BROKER=redis STORE=postgres
 *   服务端 ALLOW_EMPTY_ORIGIN=true（Node WebSocket 无 Origin）
 */

const args = process.argv.slice(2)
const getArg = (name, def) => {
  const i = args.indexOf(`--${name}`)
  return i > -1 ? args[i + 1] : def
}
const BASE = getArg('base', 'http://localhost:8080')
const WS_BASE = BASE.replace(/^http/, 'ws')
const CONNS = Number(getArg('conns', '8'))

let passed = 0
let failed = 0
const failures = []

const ok = (n, d = '') => {
  passed++
  console.log(`  \x1b[32mPASS\x1b[0m ${n}${d ? `  \x1b[90m${d}\x1b[0m` : ''}`)
}
const bad = (n, d = '') => {
  failed++
  failures.push(`${n}${d ? ` — ${d}` : ''}`)
  console.log(`  \x1b[31mFAIL\x1b[0m ${n}${d ? `  \x1b[31m${d}\x1b[0m` : ''}`)
}
const assert = (c, n, d = '') => (c ? ok(n, d) : bad(n, d), c)
const section = (t) => console.log(`\n\x1b[1m${t}\x1b[0m`)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

class Client {
  constructor(ws) {
    this.ws = ws
    this.inbox = []
    this.closed = false
    this.waiters = []
    ws.addEventListener('message', (ev) => {
      let m
      try {
        m = JSON.parse(ev.data)
      } catch {
        return
      }
      this.inbox.push(m)
      this.waiters = this.waiters.filter((w) => {
        if (w.match(m)) {
          w.resolve(m)
          return false
        }
        return true
      })
    })
    ws.addEventListener('close', () => (this.closed = true))
  }
  static connect(p) {
    const ws = new WebSocket(`${WS_BASE}/ws?${new URLSearchParams(p)}`)
    return new Promise((res, rej) => {
      const t = setTimeout(() => rej(new Error('连接超时')), 10000)
      ws.addEventListener('open', () => (clearTimeout(t), res(new Client(ws))))
      ws.addEventListener('error', () => (clearTimeout(t), rej(new Error('连接失败'))))
    })
  }
  send(type, payload) {
    this.ws.send(JSON.stringify({ v: 1, type, roomId: this.room, clientId: this.cid, ts: Date.now(), payload }))
  }
  wait(match, ms = 8000) {
    const hit = this.inbox.find(match)
    if (hit) return Promise.resolve(hit)
    return new Promise((res, rej) => {
      const entry = {
        match,
        resolve: (m) => {
          clearTimeout(timer)
          res(m)
        },
      }
      const timer = setTimeout(() => {
        this.waiters = this.waiters.filter((w) => w !== entry)
        rej(new Error(`等待超时(${ms}ms)`))
      }, ms)
      this.waiters.push(entry)
    })
  }
  all(type) {
    return this.inbox.filter((m) => m.type === type)
  }
  close() {
    try {
      this.ws.close()
    } catch {}
  }
}

function rect(id) {
  return {
    id,
    kind: 'rect',
    geom: { x: 0, y: 0, w: 50, h: 50 },
    style: { stroke: '#16181d', fill: 'none', strokeWidth: 2, opacity: 1, dash: 'solid' },
    data: {},
    text: null,
  }
}

async function main() {
  console.log(`\x1b[1mCoDraw 跨实例同步验收\x1b[0m  目标 ${BASE}  连接数 ${CONNS}`)

  section('1. 建房')
  const res = await fetch(`${BASE}/api/rooms`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ title: '多实例验收' }),
  })
  const body = await res.json()
  const code = body?.room?.code
  const key = body?.keys?.editorKey
  assert(res.status === 201 && !!code, '建房成功', `code=${code}`)

  section('2. 建立连接并识别实例')
  const clients = []
  const byInstance = new Map()

  for (let i = 0; i < CONNS; i++) {
    const cid = `c_mi_${i}_${Date.now()}`
    try {
      const c = await Client.connect({ room: code, cid, name: `节点${i}`, key })
      c.room = code
      c.cid = cid
      const joined = await c.wait((m) => m.type === 'joined')
      const inst = joined.payload?.instanceId || joined.payload?.instance || '(未上报)'
      c.instance = inst
      clients.push(c)
      byInstance.set(inst, (byInstance.get(inst) || []).concat(c))
      c.send('hello', { lastSeq: 0, epoch: 1 })
      await c.wait((m) => m.type === 'resync.snapshot' || m.type === 'resync.events')
    } catch (e) {
      bad(`第 ${i} 个连接建立失败`, e.message)
    }
  }

  const instances = [...byInstance.keys()]
  console.log(`  \x1b[90m命中实例: ${instances.join(', ')}（各 ${[...byInstance.values()].map((v) => v.length).join('/')} 个连接）\x1b[0m`)

  if (!assert(instances.length >= 2, `命中 ≥2 个实例（当前 ${instances.length}）`)) {
    console.log(
      '\n\x1b[33m提示：只命中 1 个实例，跨实例测试无意义。\x1b[0m\n' +
        '  请确认：docker compose up -d --scale app=2，且 edge(Caddy) 在做轮询。\n' +
        '  如果 joined.payload 没有 instanceId 字段，请让 Go 专家补上（验收需要）。',
    )
    for (const c of clients) c.close()
    process.exit(1)
  }

  section('3. 跨实例广播（双向）')
  // 取两个不同实例上的客户端
  const [instX, instY] = instances
  const x = byInstance.get(instX)[0]
  const y = byInstance.get(instY)[0]
  console.log(`  \x1b[90mX=${instX}  Y=${instY}\x1b[0m`)

  // X → Y
  const idXY = `sh_mi_xy_${Date.now()}`
  x.send('op.add', { opId: `op_xy_${Date.now()}`, action: 'create', shape: rect(idXY) })
  const ackX = await x.wait((m) => m.type === 'ack' && m.payload?.opId?.startsWith('op_xy_')).catch(() => null)
  assert(!!ackX && ackX.payload?.status === 'applied', 'X 侧写入被接受', `status=${ackX?.payload?.status}`)

  const seenY = await y.wait((m) => m.type === 'op.add' && m.payload?.shape?.id === idXY, 8000).catch(() => null)
  assert(!!seenY, `Y 实例（${instY}）收到 X 实例（${instX}）的广播`)
  assert(!!seenY && seenY.seq === ackX?.payload?.seq, '跨实例 seq 一致', `ack.seq=${ackX?.payload?.seq} 广播.seq=${seenY?.seq}`)

  // Y → X
  const idYX = `sh_mi_yx_${Date.now()}`
  y.send('op.add', { opId: `op_yx_${Date.now()}`, action: 'create', shape: rect(idYX) })
  const ackY = await y.wait((m) => m.type === 'ack' && m.payload?.opId?.startsWith('op_yx_')).catch(() => null)
  assert(!!ackY && ackY.payload?.status === 'applied', 'Y 侧写入被接受', `status=${ackY?.payload?.status}`)

  const seenX = await x.wait((m) => m.type === 'op.add' && m.payload?.shape?.id === idYX, 8000).catch(() => null)
  assert(!!seenX, `X 实例（${instX}）收到 Y 实例（${instY}）的广播`)

  section('4. 跨实例 seq 单调性')
  const seqs = [ackX?.payload?.seq, ackY?.payload?.seq].filter((s) => typeof s === 'number')
  assert(seqs.length === 2 && seqs[0] < seqs[1], '两个实例分配的 seq 严格递增', `seqs=${seqs.join(' < ')}`)

  section('5. 新连接从另一实例补齐（跨实例状态一致）')
  const cidN = `c_mi_new_${Date.now()}`
  const n = await Client.connect({ room: code, cid: cidN, name: '新客', key })
  n.room = code
  n.cid = cidN
  await n.wait((m) => m.type === 'joined')
  n.send('hello', { lastSeq: 0, epoch: 1 })
  const snap = await n.wait((m) => m.type === 'resync.snapshot' || m.type === 'resync.events', 8000)
  const shapes = snap.payload?.shapes ?? snap.payload?.events?.map((e) => e.shape).filter(Boolean) ?? []
  assert(shapes.length >= 2, '新连接能看到两个实例写入的图形', `count=${shapes.length}`)
  n.close()

  for (const c of clients) c.close()
  await sleep(200)

  console.log(`\n\x1b[1m结果\x1b[0m  通过 \x1b[32m${passed}\x1b[0m  失败 \x1b[31m${failed}\x1b[0m`)
  if (failed > 0) {
    console.log('\n\x1b[31m失败项：\x1b[0m')
    for (const f of failures) console.log(`  - ${f}`)
    process.exit(1)
  }
  console.log('\x1b[32m跨实例同步全部通过\x1b[0m')
}

main().catch((e) => {
  console.error('\n\x1b[31m异常终止：\x1b[0m', e.message)
  process.exit(1)
})
