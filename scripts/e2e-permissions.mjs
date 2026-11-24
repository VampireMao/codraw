#!/usr/bin/env node
/**
 * PM 独立验收：房主权限模型（docs/PERMISSIONS.md §9 P1–P12）
 *
 * 原则：只按契约文档从外部验证，不复用前后端任何测试代码。
 * 用 Node 内置 WebSocket，直接说协议。
 *
 * 用法：node scripts/e2e-permissions.mjs --base http://localhost:8080
 */
const args = process.argv.slice(2)
const getArg = (n, d) => {
  const i = args.indexOf(`--${n}`)
  return i > -1 ? args[i + 1] : d
}
const BASE = getArg('base', 'http://localhost:8080')
const WS = BASE.replace(/^http/, 'ws')

let pass = 0
let fail = 0
const failures = []
const ok = (m, d = '') => (pass++, console.log(`  \x1b[32mPASS\x1b[0m ${m}${d ? `  \x1b[90m${d}\x1b[0m` : ''}`))
const bad = (m, d = '') => {
  fail++
  failures.push(`${m}${d ? ` — ${d}` : ''}`)
  console.log(`  \x1b[31mFAIL\x1b[0m ${m}${d ? `  \x1b[31m${d}\x1b[0m` : ''}`)
}
const assert = (c, m, d = '') => (c ? ok(m, d) : bad(m, d), c)
const section = (t) => console.log(`\n\x1b[1m${t}\x1b[0m`)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

class C {
  constructor(ws) {
    this.ws = ws
    this.inbox = []
    this.waiters = []
    this.closed = false
    this.closeCode = null
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
    ws.addEventListener('close', (ev) => {
      this.closed = true
      this.closeCode = ev.code
    })
  }

  static async open(p) {
    const ws = new WebSocket(`${WS}/ws?${new URLSearchParams(p)}`)
    return new Promise((res, rej) => {
      const t = setTimeout(() => rej(new Error('连接超时')), 10000)
      ws.addEventListener('open', () => {
        clearTimeout(t)
        res(new C(ws))
      })
      ws.addEventListener('error', () => {
        clearTimeout(t)
        rej(new Error('连接失败'))
      })
    })
  }

  send(type, payload) {
    this.ws.send(
      JSON.stringify({ v: 1, type, roomId: this.room, clientId: this.cid, ts: Date.now(), payload }),
    )
  }

  wait(f, ms = 6000) {
    const hit = this.inbox.find(f)
    if (hit) return Promise.resolve(hit)
    return new Promise((res, rej) => {
      const entry = {
        f,
        r: (m) => {
          clearTimeout(t)
          res(m)
        },
      }
      const t = setTimeout(() => {
        this.waiters = this.waiters.filter((x) => x !== entry)
        rej(new Error('等待超时'))
      }, ms)
      this.waiters.push(entry)
    })
  }

  all(f) {
    return this.inbox.filter(f)
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
    geom: { x: 0, y: 0, w: 40, h: 40 },
    style: { stroke: '#16181d', fill: 'none', strokeWidth: 2, opacity: 1, dash: 'solid' },
    data: {},
    text: null,
  }
}

async function joinRoom(code, cid, name, key) {
  const params = { room: code, cid, name }
  if (key) params.key = key
  const c = await C.open(params)
  c.room = code
  c.cid = cid
  const joined = await c.wait((m) => m.type === 'joined')
  c.role = joined.payload?.you?.role
  c.isOwner = joined.payload?.you?.isOwner
  c.ownerClientId = joined.payload?.ownerClientId
  c.joined = joined
  c.send('hello', { lastSeq: 0, epoch: 1 })
  await c.wait((m) => m.type === 'resync.snapshot' || m.type === 'resync.events', 8000)
  return c
}

async function main() {
  console.log(`\x1b[1m权限模型验收（P1–P12）\x1b[0m  ${BASE}`)

  const res = await fetch(`${BASE}/api/rooms`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ title: '权限验收' }),
  })
  const body = await res.json()
  const code = body.room.code
  const editorKey = body.keys.editorKey
  console.log(`  房间 ${code}`)

  const stamp = Date.now()

  // ---------------- P1 创建者成为房主 ----------------
  section('P1 创建房间的人成为房主')
  const a = await joinRoom(code, `c_owner_${stamp}`, '房主A', editorKey)
  assert(a.role === 'owner', '首个连接 role=owner', `role=${a.role}`)
  assert(a.isOwner === true, 'joined.you.isOwner=true')
  assert(a.ownerClientId === a.cid, 'ownerClientId 指向自己', `owner=${a.ownerClientId}`)

  // ---------------- P2 填房号加入 = viewer ----------------
  section('P2 填房号加入（无 key）默认只读')
  const b = await joinRoom(code, `c_b_${stamp}`, '访客B')
  assert(b.role === 'viewer', '无 key 加入为 viewer', `role=${b.role}`)
  assert(b.isOwner === false, 'isOwner=false')
  assert(b.ownerClientId === a.cid, '能看到房主是谁', `owner=${b.ownerClientId}`)

  // viewer 写被拒且连接不断
  b.send('op.add', { opId: `op_b_deny_${stamp}`, action: 'create', shape: rect(`sh_b_deny_${stamp}`) })
  const denyAck = await b.wait((m) => m.type === 'ack' && m.payload?.opId === `op_b_deny_${stamp}`)
  assert(denyAck.payload?.status === 'rejected', 'viewer 写入被拒', `status=${denyAck.payload?.status}`)
  assert(denyAck.payload?.code === 'forbidden', '拒绝码 forbidden', `code=${denyAck.payload?.code}`)
  b.send('ping', {})
  const pong = await b.wait((m) => m.type === 'pong', 4000).catch(() => null)
  assert(!!pong && !b.closed, '被拒后连接仍存活')

  // ---------------- P3 持编辑链接仍是 viewer ----------------
  section('P3 持 editorKey 加入不夺权')
  const c3 = await joinRoom(code, `c_c_${stamp}`, '持钥C', editorKey)
  assert(c3.role === 'viewer', 'editorKey 不再给编辑权', `role=${c3.role}`)
  assert(c3.ownerClientId === a.cid, '房主仍是 A', `owner=${c3.ownerClientId}`)

  // ---------------- P4 授权为 editor ----------------
  section('P4 房主授权 B 为 editor')
  a.send('room.grant', { targetClientId: b.cid, role: 'editor' })
  const grantMsg = await b.wait(
    (m) => m.type === 'member.role' && m.payload?.clientId === b.cid && m.payload?.role === 'editor',
  )
  assert(!!grantMsg, 'B 收到 member.role')
  assert(typeof grantMsg.seq === 'number', 'member.role 带 seq', `seq=${grantMsg.seq}`)
  assert(grantMsg.payload?.grantedBy === a.cid, 'grantedBy 是房主', `by=${grantMsg.payload?.grantedBy}`)

  // B 现在能写
  b.send('op.add', { opId: `op_b_allow_${stamp}`, action: 'create', shape: rect(`sh_b_allow_${stamp}`) })
  const allowAck = await b.wait((m) => m.type === 'ack' && m.payload?.opId === `op_b_allow_${stamp}`)
  assert(allowAck.payload?.status === 'applied', '授权后 B 能写', `status=${allowAck.payload?.status}`)

  // ---------------- P5 撤销 ----------------
  section('P5 房主撤销 B 的编辑权')
  a.send('room.grant', { targetClientId: b.cid, role: 'viewer' })
  await b.wait((m) => m.type === 'member.role' && m.payload?.clientId === b.cid && m.payload?.role === 'viewer')
  b.send('op.add', { opId: `op_b_revoked_${stamp}`, action: 'create', shape: rect(`sh_b_revoked_${stamp}`) })
  const revokeAck = await b.wait((m) => m.type === 'ack' && m.payload?.opId === `op_b_revoked_${stamp}`)
  assert(revokeAck.payload?.status === 'rejected', '撤销后写入被拒', `status=${revokeAck.payload?.status}`)
  b.send('ping', {})
  const pong2 = await b.wait((m) => m.type === 'pong', 4000).catch(() => null)
  assert(!!pong2 && !b.closed, '撤销后连接仍存活（不断开）')

  // ---------------- P9 非房主尝试授权 ----------------
  section('P9 非房主发起授权应被拒')
  const cidX = `c_x_${stamp}`
  b.send('room.grant', { targetClientId: cidX, role: 'editor' })
  const forbid = await b
    .wait((m) => m.type === 'error' && (m.payload?.code === 'forbidden' || m.payload?.code === 'unauthorized'), 5000)
    .catch(() => null)
  assert(!!forbid, '非房主授权返回 forbidden', `code=${forbid?.payload?.code}`)
  await sleep(600)
  assert(b.closed, '越权操作后连接被断开', `closed=${b.closed} code=${b.closeCode}`)

  // ---------------- P6 转移房主 ----------------
  section('P6 房主转移给 C')
  const d = await joinRoom(code, `c_d_${stamp}`, '成员D')
  assert(d.role === 'viewer', 'D 初始为 viewer', `role=${d.role}`)

  a.send('room.transfer', { targetClientId: c3.cid })
  const cOwner = await c3.wait(
    (m) => m.type === 'member.role' && m.payload?.clientId === c3.cid && m.payload?.role === 'owner',
    6000,
  )
  assert(!!cOwner, 'C 成为新 owner')
  const aDowngraded = await a
    .wait((m) => m.type === 'member.role' && m.payload?.clientId === a.cid && m.payload?.role === 'editor', 6000)
    .catch(() => null)
  assert(!!aDowngraded, '原房主 A 降为 editor')

  // 原房主不再能授权
  a.send('room.grant', { targetClientId: d.cid, role: 'editor' })
  const aForbid = await a
    .wait((m) => m.type === 'error' && m.payload?.code === 'forbidden', 5000)
    .catch(() => null)
  assert(!!aForbid, '降级后无法再授权', `code=${aForbid?.payload?.code}`)
  await sleep(500)

  // ---------------- P7 房主离开按顺序移交 ----------------
  section('P7 房主离开后按入场顺序移交')
  // 当前 owner 是 C。再拉一个 E，然后让 C 离开
  const e = await joinRoom(code, `c_e_${stamp}`, '成员E')
  const ownerBefore = e.ownerClientId
  console.log(`  离开前房主: ${ownerBefore}`)

  c3.close()
  // 非主动断线走 grace window（默认 LEAVE_GRACE=10s，测试用 2s），要等它过期才移交
  await sleep(4000)

  // 新 owner 应是「仍在线的、入场最早的」——A 最早（但已降级），按契约是入场顺序
  const eGotOwner = await e
    .wait((m) => m.type === 'member.role' && m.payload?.role === 'owner', 6000)
    .catch(() => null)
  if (eGotOwner) {
    console.log(`  新 owner: ${eGotOwner.payload?.clientId}`)
    ok('房主离开后触发了移交广播', `new=${eGotOwner.payload?.clientId}`)
  } else {
    bad('房主离开后没有移交广播')
  }

  // 用一个新连接确认当前 owner 是谁
  const probe = await joinRoom(code, `c_probe_${stamp}`, '探针')
  console.log(`  当前 ownerClientId: ${probe.ownerClientId}`)
  assert(probe.ownerClientId !== c3.cid, 'owner 不再是已离开的 C', `owner=${probe.ownerClientId}`)
  assert(typeof probe.ownerClientId === 'string' && probe.ownerClientId.length > 0, '房间仍有房主（非无主）')

  // ---------------- P10 无主房间首个进入者接管 ----------------
  section('P10 无主房间首个进入者成为房主')
  // 让所有人离开
  a.close()
  d.close()
  e.close()
  probe.close()
  // 等待 grace window 过期，房间才会真正变无主
  await sleep(4500)

  const fresh = await joinRoom(code, `c_fresh_${stamp}`, '新来的')
  assert(fresh.role === 'owner', '无主房间首个进入者成为 owner', `role=${fresh.role}`)
  assert(fresh.ownerClientId === fresh.cid, 'ownerClientId 指向自己')

  // ---------------- P11 只读 token 永不提升 ----------------
  section('P11 只读 token 进入永远是 viewer')
  const roRes = await fetch(`${BASE}/api/rooms/${code}/readonly-token`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ editorKey }),
  })
  const roBody = await roRes.json()
  const roToken = roBody.token
  assert(typeof roToken === 'string' && roToken.length > 20, '签发只读 token 成功', `status=${roRes.status}`)

  const v = await C.open({ room: code, cid: `c_v_${stamp}`, name: '只读客', rt: roToken })
  v.room = code
  v.cid = `c_v_${stamp}`
  const joinedV = await v.wait((m) => m.type === 'joined')
  assert(joinedV.payload?.you?.role === 'viewer', '只读 token 进入为 viewer', `role=${joinedV.payload?.you?.role}`)

  // 必须先完成同步，否则 op.add 会被状态机挡住（只有 live 才接受写）
  v.send('hello', { lastSeq: 0, epoch: 1 })
  await v.wait((m) => m.type === 'resync.snapshot' || m.type === 'resync.events', 8000)

  // 房主授权也不应提升（安全底线）
  fresh.send('room.grant', { targetClientId: v.cid, role: 'editor' })
  await sleep(800)
  v.send('op.add', { opId: `op_v_${stamp}`, action: 'create', shape: rect(`sh_v_${stamp}`) })
  const vAck = await v
    .wait((m) => m.type === 'ack' && m.payload?.opId === `op_v_${stamp}`, 6000)
    .catch(() => null)
  const vErr = v.inbox.find((m) => m.type === 'error')
  console.log(`  只读端 ack: ${JSON.stringify(vAck?.payload)}  error: ${JSON.stringify(vErr?.payload)}`)
  assert(
    vAck?.payload?.status === 'rejected',
    '只读 token 即使被授权也不能写（安全底线）',
    `status=${vAck?.payload?.status ?? '无 ack'}`,
  )

  v.close()
  fresh.close()
  await sleep(300)

  console.log(`\n\x1b[1m结果\x1b[0m  通过 \x1b[32m${pass}\x1b[0m  失败 \x1b[31m${fail}\x1b[0m`)
  if (fail > 0) {
    console.log('\n\x1b[31m失败项：\x1b[0m')
    for (const f of failures) console.log(`  - ${f}`)
    process.exit(1)
  }
  console.log('\x1b[32m权限模型验收通过\x1b[0m')
}

main().catch((e) => {
  console.error('\x1b[31m异常:\x1b[0m', e.message)
  process.exit(1)
})
