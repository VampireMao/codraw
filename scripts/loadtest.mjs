#!/usr/bin/env node
/**
 * CoDraw · 压测器（PM 独立实现，不复用后端代码）
 *
 * 三个场景：
 *   s1  单房间 N 客户端持续绘制（测端到端广播延迟）
 *   s2  M 个连接跨 K 个房间保活（测连接容量与内存稳定性）
 *   s3  M 个客户端高吞吐（测消息吞吐上限）
 *
 * 延迟定义：本进程内「某客户端发出 op 的时刻」→「另一客户端收到该 op 的时刻」。
 * 因为都在同一进程内，时钟天然一致，不需要 NTP 对齐。这是同机测试的便利，
 * 也必须在报告中声明（公网会明显更差）。
 *
 * 用法：
 *   node scripts/loadtest.mjs --scenario s1 --base http://localhost:8080 --clients 100 --rate 5 --duration 60
 *   node scripts/loadtest.mjs --scenario s2 --base http://localhost:8080 --conns 1000 --rooms 100 --duration 600
 *   node scripts/loadtest.mjs --scenario s3 --base http://localhost:8080 --clients 500 --rate 10 --duration 300
 */

const args = process.argv.slice(2)
const arg = (n, d) => {
  const i = args.indexOf(`--${n}`)
  return i > -1 ? args[i + 1] : d
}

const BASE = arg('base', 'http://localhost:8080')
const WS_BASE = BASE.replace(/^http/, 'ws')
const SCENARIO = arg('scenario', 's1')
const CLIENTS = Number(arg('clients', '100'))
const CONNS = Number(arg('conns', '1000'))
const ROOMS = Number(arg('rooms', '1'))
const RATE = Number(arg('rate', '5'))
const DURATION = Number(arg('duration', '60'))
const RAMP = Number(arg('ramp', '5000')) // 连接建立的总爬坡时间 ms

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const fmt = (n, d = 2) => (Number.isFinite(n) ? n.toFixed(d) : 'n/a')

function quantiles(arr) {
  if (!arr.length) return { p50: NaN, p95: NaN, p99: NaN, max: NaN, count: 0 }
  const s = [...arr].sort((a, b) => a - b)
  const at = (q) => s[Math.min(s.length - 1, Math.max(0, Math.ceil(q * s.length) - 1))]
  return { p50: at(0.5), p95: at(0.95), p99: at(0.99), max: s[s.length - 1], count: s.length }
}

async function http(method, path, body) {
  const res = await fetch(`${BASE}${path}`, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  return { status: res.status, json: await res.json().catch(() => null) }
}

/** 发送时刻表：shapeId -> {t, clientIdx} */
const sentAt = new Map()
const latencies = []
let errors = 0
let acks = 0
let broadcasts = 0
let closedUnexpectedly = 0
let stop = false

function makeRect(id, i) {
  return {
    id,
    kind: 'rect',
    geom: { x: (i % 2000) * 3, y: Math.floor(i / 2000) * 3, w: 20, h: 20 },
    style: { stroke: '#16181d', fill: 'none', strokeWidth: 2, opacity: 1, dash: 'solid' },
    data: {},
    text: null,
  }
}

class LoadClient {
  constructor(idx, code, key, roomTag) {
    this.idx = idx
    this.code = code
    this.key = key
    this.cid = `c_lt_${roomTag}_${idx}_${Math.random().toString(36).slice(2, 8)}`
    this.opSeq = 0
    this.ws = null
    this.ready = false
    this.expectClose = false
  }

  connect() {
    const q = new URLSearchParams({ room: this.code, cid: this.cid, name: `压测${this.idx}`, key: this.key })
    const ws = new WebSocket(`${WS_BASE}/ws?${q}`)
    this.ws = ws

    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('连接超时')), 20000)

      ws.addEventListener('open', () => {
        ws.send(
          JSON.stringify({
            v: 1,
            type: 'hello',
            roomId: this.code,
            clientId: this.cid,
            ts: Date.now(),
            payload: { lastSeq: 0, epoch: 1 },
          }),
        )
      })

      ws.addEventListener('message', (ev) => {
        let m
        try {
          m = JSON.parse(ev.data)
        } catch {
          return
        }
        switch (m.type) {
          case 'joined':
            break
          case 'resync.snapshot':
          case 'resync.events':
            if (!this.ready) {
              this.ready = true
              clearTimeout(timer)
              resolve(this)
            }
            break
          case 'ack':
            acks++
            if (m.payload?.status === 'rejected') errors++
            break
          case 'op.add': {
            broadcasts++
            const sid = m.payload?.shape?.id
            if (sid) {
              const rec = sentAt.get(sid)
              if (rec && rec.clientIdx !== this.idx) {
                latencies.push(Date.now() - rec.t)
                sentAt.delete(sid)
              }
            }
            break
          }
          case 'error':
            errors++
            break
        }
      })

      ws.addEventListener('close', () => {
        if (!this.expectClose) closedUnexpectedly++
      })
      ws.addEventListener('error', () => {
        if (!this.expectClose) errors++
      })
    })
  }

  sendOp() {
    if (!this.ready || this.ws?.readyState !== 1) return
    const n = this.opSeq++
    const sid = `sh_lt_${this.idx}_${n}`
    const opId = `op_lt_${this.idx}_${n}`
    sentAt.set(sid, { t: Date.now(), clientIdx: this.idx })
    this.ws.send(
      JSON.stringify({
        v: 1,
        type: 'op.add',
        roomId: this.code,
        clientId: this.cid,
        ts: Date.now(),
        payload: { opId, action: 'create', shape: makeRect(sid, this.idx * 1000 + n) },
      }),
    )
  }

  close() {
    this.expectClose = true
    try {
      this.ws?.close()
    } catch {}
  }
}

async function createRoom(title) {
  const r = await http('POST', '/api/rooms', { title })
  if (r.status !== 201) throw new Error(`建房失败 status=${r.status}`)
  return { code: r.json.room.code, key: r.json.keys.editorKey }
}

async function main() {
  console.log(`\x1b[1mCoDraw 压测\x1b[0m  场景 ${SCENARIO}  目标 ${BASE}`)

  // 基线指标（用于对比增长）
  const baseline = await http('GET', '/ops/data')
  console.log(`\x1b[90m基线: 连接=${baseline.json?.connections} 堆=${fmt(baseline.json?.heapMB)}MB goroutines=${baseline.json?.goroutines}\x1b[0m`)

  let clients = []

  if (SCENARIO === 's1') {
    // 单房间 N 客户端
    console.log(`\x1b[90m单房间 ${CLIENTS} 客户端，每客户端 ${RATE} op/s，持续 ${DURATION}s\x1b[0m`)
    const room = await createRoom('压测 S1')
    console.log(`  房间 ${room.code}`)
    const per = Math.max(1, Math.ceil(RAMP / CLIENTS))
    for (let i = 0; i < CLIENTS; i++) {
      const c = new LoadClient(i, room.code, room.key, 's1')
      await c.connect()
      clients.push(c)
      await sleep(per)
    }
    console.log(`  ${clients.length} 个连接就绪`)
    const tick = setInterval(() => {
      for (const c of clients) c.sendOp()
    }, 1000 / RATE)
    await sleep(DURATION * 1000)
    clearInterval(tick)
  } else if (SCENARIO === 's2') {
    // M 连接跨 K 房间保活
    console.log(`\x1b[90m${CONNS} 连接 / ${ROOMS} 房间，持续 ${DURATION}s\x1b[0m`)
    const rooms = []
    for (let i = 0; i < ROOMS; i++) rooms.push(await createRoom(`压测 S2-${i}`))
    console.log(`  ${rooms.length} 个房间已建`)

    const per = Math.max(1, Math.ceil(RAMP / CONNS))
    const t0 = Date.now()
    for (let i = 0; i < CONNS; i++) {
      const room = rooms[i % rooms.length]
      const c = new LoadClient(i, room.code, room.key, 's2')
      try {
        await c.connect()
        clients.push(c)
      } catch {
        errors++
      }
      if (i % 100 === 0) {
        process.stdout.write(`\r  已连接 ${clients.length}/${CONNS}  (${((Date.now() - t0) / 1000).toFixed(0)}s)   `)
      }
      await sleep(per)
    }
    console.log(`\r  ${clients.length}/${CONNS} 个连接就绪 (${((Date.now() - t0) / 1000).toFixed(0)}s)`)

    // 每 10s 一次心跳 + cursor
    const tick = setInterval(() => {
      for (const c of clients) {
        if (c.ws?.readyState !== 1) continue
        c.ws.send(
          JSON.stringify({
            v: 1,
            type: 'presence.cursor',
            roomId: c.code,
            clientId: c.cid,
            ts: Date.now(),
            payload: { x: Math.random() * 1000, y: Math.random() * 1000 },
          }),
        )
      }
    }, 10000)

    const samples = []
    const sampler = setInterval(async () => {
      const d = await http('GET', '/ops/data')
      if (d.json) samples.push({ t: Date.now(), heap: d.json.heapMB, gor: d.json.goroutines, conn: d.json.connections })
    }, 15000)

    await sleep(DURATION * 1000)
    clearInterval(tick)
    clearInterval(sampler)
    if (samples.length >= 2) {
      const first = samples[0]
      const last = samples[samples.length - 1]
      console.log(`\x1b[90m  内存: ${fmt(first.heap)}MB → ${fmt(last.heap)}MB\x1b[0m`)
      console.log(`\x1b[90m  goroutine: ${first.gor} → ${last.gor}\x1b[0m`)
      console.log(`\x1b[90m  服务端连接数峰值: ${Math.max(...samples.map((s) => s.conn))}\x1b[0m`)
      globalThis.__s2 = { first, last }
    }
  } else if (SCENARIO === 's3') {
    // 高吞吐
    const perRoom = Math.max(1, Math.ceil(CLIENTS / ROOMS))
    console.log(`\x1b[90m${ROOMS} 房间 × ${perRoom} 客户端，每客户端 ${RATE} msg/s，持续 ${DURATION}s\x1b[0m`)
    const rooms = []
    for (let i = 0; i < ROOMS; i++) rooms.push(await createRoom(`压测 S3-${i}`))

    const per = Math.max(1, Math.ceil(RAMP / CLIENTS))
    for (let i = 0; i < CLIENTS; i++) {
      const room = rooms[Math.floor(i / perRoom) % rooms.length]
      try {
        const c = new LoadClient(i, room.code, room.key, 's3')
        await c.connect()
        clients.push(c)
      } catch {
        errors++
      }
      if (i % 50 === 0) process.stdout.write(`\r  已连接 ${clients.length}/${CLIENTS}   `)
      await sleep(per)
    }
    console.log(`\r  ${clients.length}/${CLIENTS} 个连接就绪`)

    const tick = setInterval(() => {
      for (const c of clients) c.sendOp()
    }, 1000 / RATE)
    await sleep(DURATION * 1000)
    clearInterval(tick)
  }

  // ---------- 收尾 ----------
  stop = true
  await sleep(1500) // 让在途消息落地

  const after = await http('GET', '/ops/data')
  const q = quantiles(latencies)
  const opsSent = clients.reduce((s, c) => s + c.opSeq, 0)

  console.log('\n\x1b[1m结果\x1b[0m')
  console.log(`  客户端数        ${clients.length}`)
  console.log(`  发送 op 数      ${opsSent}`)
  console.log(`  ack 数          ${acks}`)
  console.log(`  收到广播数      ${broadcasts}`)
  console.log(`  错误            ${errors}`)
  console.log(`  意外断开        ${closedUnexpectedly}`)
  if (q.count) {
    console.log(`  延迟 p50        ${fmt(q.p50)} ms`)
    console.log(`  延迟 p95        ${fmt(q.p95)} ms`)
    console.log(`  延迟 p99        ${fmt(q.p99)} ms`)
    console.log(`  延迟 max        ${fmt(q.max)} ms`)
    console.log(`  延迟样本数      ${q.count}`)
  }
  console.log(`  服务端堆        ${fmt(after.json?.heapMB)} MB`)
  console.log(`  服务端 goroutine ${after.json?.goroutines}`)
  console.log(`  服务端连接数    ${after.json?.connections}`)
  console.log(`  p95(服务端直方图) ${fmt(after.json?.broadcastLatencyP95Ms)} ms`)
  console.log(`  慢消费者踢除    ${after.json?.slowConsumerKicks}`)

  for (const c of clients) c.close()
  await sleep(500)

  const okRun = errors === 0 && closedUnexpectedly === 0
  console.log(okRun ? '\n\x1b[32m无错误\x1b[0m' : '\n\x1b[33m存在错误或意外断开（见上）\x1b[0m')
  process.exit(okRun ? 0 : 2)
}

main().catch((e) => {
  console.error('\x1b[31m压测异常:\x1b[0m', e.message)
  process.exit(1)
})
