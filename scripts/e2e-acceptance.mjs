#!/usr/bin/env node
/**
 * CoDraw · 独立协议级验收（PM 验收用，不依赖前后端任何单元测试）
 *
 * 设计原则：
 *  1. 只依赖 Node 24 内置的 fetch 与 WebSocket，零第三方依赖。
 *  2. 只按 docs/PROTOCOL.md 的字段断言，不复用任何一方代码 —— 独立事实核对。
 *  3. 每条断言打印 PASS/FAIL 与关键字段，失败即退出码 1。
 *
 * 用法：
 *   node scripts/e2e-acceptance.mjs [--base http://localhost:8080]
 *
 * 注意：需要服务端 ALLOW_EMPTY_ORIGIN=true（Node 的 WebSocket 不发 Origin 头）。
 */

const BASE = (() => {
  const i = process.argv.indexOf('--base')
  return i > -1 ? process.argv[i + 1] : 'http://localhost:8080'
})()
const WS_BASE = BASE.replace(/^http/, 'ws')

let passed = 0
let failed = 0
const failures = []

function ok(name, detail = '') {
  passed++
  console.log(`  \x1b[32mPASS\x1b[0m ${name}${detail ? `  \x1b[90m${detail}\x1b[0m` : ''}`)
}
function bad(name, detail = '') {
  failed++
  failures.push(`${name}${detail ? ` — ${detail}` : ''}`)
  console.log(`  \x1b[31mFAIL\x1b[0m ${name}${detail ? `  \x1b[31m${detail}\x1b[0m` : ''}`)
}
function assert(cond, name, detail = '') {
  if (cond) ok(name, detail)
  else bad(name, detail)
  return cond
}
function section(t) {
  console.log(`\n\x1b[1m${t}\x1b[0m`)
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

/** 建一个受控 WS 客户端：收件箱 + 按类型等待 */
class Client {
  constructor(ws) {
    this.ws = ws
    this.inbox = []
    this.closed = false
    this.closeCode = null
    this.waiters = []
    ws.addEventListener('message', (ev) => {
      let msg
      try {
        msg = JSON.parse(ev.data)
      } catch {
        return
      }
      this.inbox.push(msg)
      this.waiters = this.waiters.filter((w) => {
        if (w.match(msg)) {
          w.resolve(msg)
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

  static connect(params) {
    const q = new URLSearchParams(params).toString()
    const ws = new WebSocket(`${WS_BASE}/ws?${q}`)
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('WS 连接超时(10s)')), 10000)
      ws.addEventListener('open', () => {
        clearTimeout(timer)
        resolve(new Client(ws))
      })
      ws.addEventListener('error', () => {
        clearTimeout(timer)
        reject(new Error('WS 连接失败'))
      })
    })
  }

  send(type, payload, extra = {}) {
    this.ws.send(
      JSON.stringify({
        v: 1,
        type,
        roomId: this.roomId,
        clientId: this.clientId,
        ts: Date.now(),
        payload,
        ...extra,
      }),
    )
  }

  /** 等待满足条件的消息（已收到的也算） */
  wait(match, timeoutMs = 5000) {
    const hit = this.inbox.find(match)
    if (hit) return Promise.resolve(hit)
    return new Promise((resolve, reject) => {
      const entry = {
        match,
        resolve: (msg) => {
          clearTimeout(timer)
          resolve(msg)
        },
      }
      const timer = setTimeout(() => {
        this.waiters = this.waiters.filter((w) => w !== entry)
        reject(new Error(`等待消息超时(${timeoutMs}ms)`))
      }, timeoutMs)
      this.waiters.push(entry)
    })
  }

  waitType(type, timeoutMs) {
    return this.wait((m) => m.type === type, timeoutMs)
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

function makeShape(id, over = {}) {
  return {
    id,
    kind: 'rect',
    geom: { x: 10, y: 20, w: 100, h: 60 },
    style: { stroke: '#16181d', fill: 'none', strokeWidth: 2, opacity: 1, dash: 'solid' },
    data: {},
    text: null,
    ...over,
  }
}

async function http(method, path, body, headers = {}) {
  const res = await fetch(`${BASE}${path}`, {
    method,
    headers: { 'Content-Type': 'application/json', ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await res.text()
  let json = null
  try {
    json = text ? JSON.parse(text) : null
  } catch {}
  return { status: res.status, json, text, headers: res.headers }
}

async function main() {
  console.log(`\x1b[1mCoDraw 协议级验收\x1b[0m  目标 ${BASE}`)

  // ---------------------------------------------------------------- 0. 探活
  section('0. 探活与运维端点')
  const health = await http('GET', '/healthz')
  assert(health.status === 200, '/healthz 返回 200', `status=${health.status}`)
  assert(health.json?.status === 'ok', '/healthz status=ok', JSON.stringify(health.json))

  const ready = await http('GET', '/readyz')
  assert(ready.status === 200, '/readyz 返回 200', `status=${ready.status}`)
  assert(ready.json?.status === 'ready', '/readyz status=ready', JSON.stringify(ready.json))

  const metrics = await http('GET', '/metrics')
  assert(metrics.status === 200, '/metrics 可访问', `status=${metrics.status}`)
  assert(
    metrics.text.includes('codraw_'),
    '/metrics 含 codraw_ 指标',
    metrics.text.split('\n').filter((l) => l.startsWith('codraw_')).length + ' 条',
  )

  const ops = await http('GET', '/ops/data')
  assert(ops.status === 200, '/ops/data 可访问', `status=${ops.status}`)
  assert(typeof ops.json?.connections === 'number', '/ops/data 含 connections', `connections=${ops.json?.connections}`)

  // ---------------------------------------------------------------- 1. 建房
  section('1. 建房与元信息')
  const created = await http('POST', '/api/rooms', { title: 'PM 验收' })
  assert(created.status === 201, 'POST /api/rooms 返回 201', `status=${created.status}`)
  const code = created.json?.room?.code
  const editorKey = created.json?.keys?.editorKey
  assert(/^[2-9A-HJKMNP-Z]{6}$/.test(code || ''), '房间码格式合法(31 字符集)', `code=${code}`)
  assert(!/[0O1IL]/.test(code || 'x'), '房间码不含易混淆字符', `code=${code}`)
  assert((editorKey || '').length === 43, 'editorKey 长度 43', `len=${editorKey?.length}`)
  assert(typeof created.json?.urls?.edit === 'string' && created.json.urls.edit.includes(code), '创建响应含可用的 urls.edit')

  const info = await http('GET', `/api/rooms/${code}`)
  assert(info.status === 200, 'GET /api/rooms/{code} 返回 200', `status=${info.status}`)
  assert(info.json?.role === 'viewer', '无 key 查询回显 viewer', `role=${info.json?.role}`)

  const bad = await http('GET', '/api/rooms/ZZZZZZ')
  assert(bad.status === 404, '不存在的房间返回 404', `status=${bad.status}`)

  // ---------------------------------------------------------------- 2. 首帧时序
  section('2. WS 首帧时序（joined / hello 并发互不等待）')
  const cidA = 'c_pm_a_' + Date.now()
  const a = await Client.connect({ room: code, cid: cidA, name: '验收A', key: editorKey })
  a.roomId = code
  a.clientId = cidA

  const joinedA = await a.waitType('joined')
  assert(!!joinedA, '服务端主动发 joined（不等 hello）')
  // v2 权限模型：第一个进入房间的人成为房主（owner），editorKey 不再直接给 editor
  assert(
    joinedA.payload?.you?.role === 'owner',
    '首个连接成为房主（owner）',
    `role=${joinedA.payload?.you?.role}`,
  )
  assert(joinedA.payload?.you?.isOwner === true, 'joined 带 isOwner=true')
  assert(joinedA.payload?.ownerClientId === cidA, 'ownerClientId 指向首个连接', `owner=${joinedA.payload?.ownerClientId}`)
  assert(joinedA.payload?.you?.name === '验收A', 'joined 回显昵称', `name=${joinedA.payload?.you?.name}`)
  assert(joinedA.seq === undefined, 'joined 不占用 seq', `seq=${joinedA.seq}`)

  // 此刻还没有图形数据（joined 不带 shapes）
  assert(!Array.isArray(joinedA.payload?.shapes), 'joined 不携带图形数据')

  a.send('hello', { lastSeq: 0, epoch: 1 })
  const snapA = await a.waitType('resync.snapshot', 8000)
  assert(!!snapA, 'hello{lastSeq:0} 后收到 resync.snapshot')
  assert(Array.isArray(snapA.payload?.shapes), 'snapshot 含 shapes 数组', `count=${snapA.payload?.shapes?.length}`)

  // ---------------------------------------------------------------- 3. 双人同步
  section('3. 双人实时同步与 seq 单调')
  const cidB = 'c_pm_b_' + Date.now()
  const b = await Client.connect({ room: code, cid: cidB, name: '验收B', key: editorKey })
  b.roomId = code
  b.clientId = cidB
  const joinedB = await b.waitType('joined')
  // 第二个进入者即使持 editorKey 也是 viewer（房主独占，需显式授权）
  assert(
    joinedB.payload?.you?.role === 'viewer',
    '第二个进入者持 editorKey 仍为 viewer（房主独占）',
    `role=${joinedB.payload?.you?.role}`,
  )
  b.send('hello', { lastSeq: 0, epoch: 1 })
  await b.waitType('resync.snapshot', 8000)

  // 房主授权 B 为 editor（新增权限模型的核心路径）
  a.send('room.grant', { targetClientId: cidB, role: 'editor' })
  const grantBcast = await b.wait(
    (m) => m.type === 'member.role' && m.payload?.clientId === cidB && m.payload?.role === 'editor',
    5000,
  )
  assert(!!grantBcast, 'B 收到 member.role 授权通知')
  assert(typeof grantBcast.seq === 'number', 'member.role 带 seq（可断线补齐）', `seq=${grantBcast.seq}`)
  b.role = 'editor'

  /**
   * 由「当前房主」授权某人成为 editor。
   *
   * v2 权限模型下 editorKey 不再给编辑权，必须由房主显式授权。
   * 房主可能因断线移交而变化，所以这里动态解析当前 owner 的连接。
   */
  let ownerConn = a
  function currentOwnerConn() {
    return ownerConn ?? a
  }

  async function connectAsEditor(tag) {
    const cid = `c_pm_${tag}_${Date.now()}_${Math.random().toString(36).slice(2, 6)}`
    const c = await Client.connect({ room: code, cid, name: tag, key: editorKey })
    c.roomId = code
    c.clientId = cid
    await c.waitType('joined')
    c.send('hello', { lastSeq: 0, epoch: 1 })
    await c.wait((m) => m.type === 'resync.snapshot' || m.type === 'resync.events', 8000)

    const owner = currentOwnerConn()
    owner.send('room.grant', { targetClientId: cid, role: 'editor' })
    await c.wait((m) => m.type === 'member.role' && m.payload?.clientId === cid && m.payload?.role === 'editor', 5000)
    return c
  }

  // B 应收到 A 的 presence.join
  const presenceJoin = await b.wait((m) => m.type === 'presence.join', 5000).catch(() => null)
  assert(!!presenceJoin, 'B 收到 A 的 presence.join')

  const shape1 = makeShape('sh_pm_001')
  a.send('op.add', { opId: 'op_pm_001', action: 'create', shape: shape1 })

  const ack1 = await a.wait((m) => m.type === 'ack' && m.payload?.opId === 'op_pm_001', 5000)
  assert(ack1.payload?.status === 'applied', 'A 收到 ack status=applied', `status=${ack1.payload?.status}`)
  // v2：房主授权 B 时占用了一个 seq，所以首条 op 的 seq 不再是 1。
  // 断言改为「seq 是正整数且单调递增」，不写死具体值。
  const seq1 = ack1.payload?.seq
  assert(typeof seq1 === 'number' && seq1 > 0, '首条 op 拿到正 seq', `seq=${seq1}`)
  assert(ack1.seq === undefined, 'ack 的 seq 在 payload 内（信封无 seq）', `envelope.seq=${ack1.seq}`)

  const bOp1 = await b.wait((m) => m.type === 'op.add' && m.payload?.shape?.id === 'sh_pm_001', 5000)
  assert(!!bOp1, 'B 收到 A 的 op.add 广播')
  assert(bOp1.seq === seq1, '广播 seq 与 ack seq 一致', `bcast=${bOp1.seq} ack=${seq1}`)
  assert(bOp1.clientId === cidA, 'op.add 信封 clientId 是发起者', `clientId=${bOp1.clientId}`)
  assert(bOp1.payload?.shape?.author?.cid === cidA, 'shape.author 由服务端按连接身份覆盖', `author=${bOp1.payload?.shape?.author?.cid}`)
  assert(bOp1.payload?.shape?.rev === 1, 'create 后 rev=1', `rev=${bOp1.payload?.shape?.rev}`)

  // 第二条：B 画，A 收，seq 连续
  const shape2 = makeShape('sh_pm_002')
  b.send('op.add', { opId: 'op_pm_002', action: 'create', shape: shape2 })
  const aOp2 = await a.wait((m) => m.type === 'op.add' && m.payload?.shape?.id === 'sh_pm_002', 5000)
  assert(aOp2.seq === seq1 + 1, 'seq 单调递增（+1）', `seq=${aOp2.seq} 前一条=${seq1}`)

  // ---------------------------------------------------------------- 4. 幂等
  section('4. opId 幂等去重')
  a.send('op.add', { opId: 'op_pm_001', action: 'create', shape: shape1 }) // 重发同 opId
  const dup = await a.wait(
    (m) => m.type === 'ack' && m.payload?.opId === 'op_pm_001' && m.payload?.status === 'duplicate',
    5000,
  )
  assert(!!dup, '重发同 (cid,opId) 返回 status=duplicate')
  assert(dup.payload?.seq === seq1, 'duplicate 复用首次 seq', `seq=${dup.payload?.seq} 首次=${seq1}`)

  await sleep(300)
  const bOpsForShape1 = b.all('op.add').filter((m) => m.payload?.shape?.id === 'sh_pm_001')
  assert(bOpsForShape1.length === 1, 'B 侧未收到重复图形', `收到 ${bOpsForShape1.length} 次`)

  // ---------------------------------------------------------------- 5. 只读
  section('5. 只读 token 与服务端强制拒绝')
  const ro = await http('POST', `/api/rooms/${code}/readonly-token`, { editorKey })
  assert(ro.status === 200, 'POST /readonly-token 返回 200', `status=${ro.status}`)
  const roToken = ro.json?.token
  assert(typeof roToken === 'string' && roToken.length > 20, '返回只读 token', `len=${roToken?.length}`)
  assert(ro.json?.role === 'viewer', 'token 角色为 viewer')

  const cidV = 'c_pm_v_' + Date.now()
  const v = await Client.connect({ room: code, cid: cidV, name: '只读客', rt: roToken })
  v.roomId = code
  v.clientId = cidV
  const joinedV = await v.waitType('joined')
  assert(joinedV.payload?.you?.role === 'viewer', 'rt 得到 viewer 角色', `role=${joinedV.payload?.you?.role}`)

  v.send('hello', { lastSeq: 0, epoch: 1 })
  const snapV = await v.waitType('resync.snapshot', 8000)
  assert(
    snapV.payload?.shapes?.length === 2,
    '只读端能看到已有图形',
    `count=${snapV.payload?.shapes?.length}`,
  )

  // 关键：viewer 直接发写请求（绕过 UI）
  v.send('op.add', { opId: 'op_pm_viewer', action: 'create', shape: makeShape('sh_pm_hack') })
  const rejectAck = await v.wait((m) => m.type === 'ack' && m.payload?.opId === 'op_pm_viewer', 5000)
  assert(rejectAck.payload?.status === 'rejected', 'viewer 写入被拒 status=rejected', `status=${rejectAck.payload?.status}`)
  assert(rejectAck.payload?.code === 'forbidden', '拒绝码为 forbidden', `code=${rejectAck.payload?.code}`)

  // 连接必须仍然存活
  v.send('ping', {})
  const pongV = await v.waitType('pong', 5000).catch(() => null)
  assert(!!pongV, '被拒后连接仍存活（ping→pong）')
  assert(!v.closed, '被拒后连接未被关闭', `closeCode=${v.closeCode}`)

  // A 不应看到越权图形
  await sleep(300)
  const hackSeen = a.all('op.add').filter((m) => m.payload?.shape?.id === 'sh_pm_hack')
  assert(hackSeen.length === 0, '越权写入未广播给他人')

  // ---------------------------------------------------------------- 6. 断线补齐
  section('6. 断线重连按 seq 增量补齐（F6）')
  // A 最后应用到的 seq：从它收到的 op.add 广播里取最大值
  // （不能写死，授权/转移事件也占 seq）
  const seenSeqs = a.all('op.add').map((m) => m.seq).filter((s) => typeof s === 'number')
  const beforeClose = seenSeqs.length > 0 ? Math.max(...seenSeqs) : 0
  console.log(`\x1b[90m  A 断开前已应用 seq=${beforeClose}（共收到 ${seenSeqs.length} 条广播）\x1b[0m`)
  a.close()
  await sleep(400)

  // A 离线期间，B 画 3 笔
  const offlineSeqStart = beforeClose + 1
  for (let i = 3; i <= 5; i++) {
    b.send('op.add', { opId: `op_pm_00${i}`, action: 'create', shape: makeShape(`sh_pm_00${i}`) })
    await b.wait((m) => m.type === 'ack' && m.payload?.opId === `op_pm_00${i}`, 5000)
  }

  // A 重连，带上已应用的 lastSeq
  const a2 = await Client.connect({ room: code, cid: cidA, name: '验收A', key: editorKey })
  a2.roomId = code
  a2.clientId = cidA
  const joinedA2 = await a2.waitType('joined')

  // v2 权限模型：A 在 grace window 内重连，房主**没有被移交**，因此恢复 owner。
  // （若断线超过 LEAVE_GRACE，房主会移交给下一个人，A 重连时降为 editor）
  assert(
    joinedA2.payload?.you?.role === 'owner',
    'grace window 内重连恢复 owner（未移交）',
    `role=${joinedA2.payload?.you?.role}`,
  )
  assert(
    joinedA2.payload?.ownerClientId === cidA,
    '房主仍是 A（未移交）',
    `owner=${joinedA2.payload?.ownerClientId}`,
  )
  // A 仍是房主，后续授权由重连后的 a2 发起
  ownerConn = a2

  a2.send('hello', { lastSeq: beforeClose, epoch: 1 })

  const resync = await a2.wait(
    (m) => m.type === 'resync.events' || m.type === 'resync.snapshot',
    8000,
  )
  if (resync.type === 'resync.events') {
    const seqs = resync.payload?.events?.map((e) => e.seq) ?? []
    assert(seqs.length === 3, 'resync.events 只补 lastSeq 之后的 3 条', `seqs=[${seqs}]`)
    assert(
      seqs[0] === offlineSeqStart && seqs[seqs.length - 1] === offlineSeqStart + 2,
      '补齐区间连续，无缺口无重复',
      `[${seqs[0]}..${seqs[seqs.length - 1]}] 期望起点 ${offlineSeqStart}`,
    )
  } else {
    // 服务端选择快照也是合法的（保留窗口不足时），但要保证内容完整
    assert(
      resync.payload?.shapes?.length === 5,
      '退化快照时图形完整（5 条）',
      `count=${resync.payload?.shapes?.length}`,
    )
  }

  // ---------------------------------------------------------------- 7. 越权与限流
  section('7. 边界与安全')
  const cidC = 'c_pm_c_' + Date.now()
  const c = await Client.connect({ room: code, cid: cidC, name: '无钥客' })
  c.roomId = code
  c.clientId = cidC
  const joinedC = await c.waitType('joined')
  assert(joinedC.payload?.you?.role === 'viewer', '无 key 连接默认 viewer', `role=${joinedC.payload?.you?.role}`)
  c.close()

  // 大消息拒绝
  const d = await connectAsEditor('d')

  // 7a. 帧级超限：原始帧 > 64KB（独立连接，因为服务端可能直接关闭连接）
  const f = await connectAsEditor('f')

  const hugeRaw = JSON.stringify({
    v: 1,
    type: 'op.add',
    roomId: code,
    clientId: f.clientId,
    ts: Date.now(),
    payload: { opId: 'op_pm_huge_frame', action: 'create', shape: { id: 'sh_pm_huge', junk: 'x'.repeat(70000) } },
  })
  f.ws.send(hugeRaw)
  // 合法行为有两种：回 error/ack，或直接以 1009 关闭连接
  await sleep(1200)
  const frameRejected =
    f.closed || f.inbox.some((m) => m.type === 'error' || m.type === 'ack')
  assert(
    frameRejected,
    '超 64KB 原始帧被拒（error / ack / 关闭连接）',
    f.closed ? `连接已关闭 code=${f.closeCode}` : `msg=${f.inbox.map((m) => m.type).join(',')}`,
  )
  f.close()

  // 7b. shape 级超限：消息 < 64KB，但单个 shape 序列化 > 32KB（独立连接）
  const g = await connectAsEditor('g')

  const manyPoints = Array.from({ length: 2600 }, (_, i) => [i * 1.5, i * 0.75])
  const bigShape = makeShape('sh_pm_bigshape', {
    kind: 'freehand',
    data: { points: manyPoints, closed: false },
  })
  const shapeBytes = JSON.stringify(bigShape).length
  g.send('op.add', { opId: 'op_pm_bigshape', action: 'create', shape: bigShape })
  const shapeErr = await g
    .wait((m) => m.type === 'ack' && m.payload?.opId === 'op_pm_bigshape', 6000)
    .catch(() => null)
  const shapeErrCode = shapeErr?.payload?.code ?? shapeErr?.payload?.status
  assert(
    !!shapeErr && (shapeErr.payload?.status === 'rejected' || ['shape_too_large', 'invalid_op'].includes(shapeErrCode)),
    '单 shape 超 32KB 被拒',
    `shape=${(shapeBytes / 1024).toFixed(1)}KB status=${shapeErr?.payload?.status} code=${shapeErrCode}`,
  )
  g.close()

  // ---------------------------------------------------------------- 8. 撤销只读
  section('8. 只读 token 撤销')
  const revoke = await http('POST', `/api/rooms/${code}/readonly-token/revoke`, { editorKey, all: true })
  assert(revoke.status === 200, 'POST /readonly-token/revoke 返回 200', `status=${revoke.status}`)
  assert(revoke.json?.revoked === true, '返回 revoked=true')

  const cidE = 'c_pm_e_' + Date.now()
  let revokedRejected = false
  let revokedStatus = 0
  try {
    const e = await Client.connect({ room: code, cid: cidE, name: '旧token', rt: roToken })
    e.close()
  } catch (err) {
    revokedRejected = true
  }
  // 也检查 HTTP 侧：旧 token 不应再被接受
  const checkOld = await http('GET', `/api/rooms/${code}?rt=${encodeURIComponent(roToken)}`)
  revokedStatus = checkOld.status
  assert(
    revokedRejected || checkOld.json?.role === 'viewer',
    '撤销后旧 token 不再具备编辑能力',
    `ws拒绝=${revokedRejected} http.status=${revokedStatus}`,
  )

  // ---------------------------------------------------------------- 收尾
  a2.close()
  b.close()
  v.close()
  d.close()
  await sleep(300)

  console.log(`\n\x1b[1m结果\x1b[0m  通过 \x1b[32m${passed}\x1b[0m  失败 \x1b[31m${failed}\x1b[0m`)
  if (failed > 0) {
    console.log('\n\x1b[31m失败项：\x1b[0m')
    for (const f of failures) console.log(`  - ${f}`)
    process.exit(1)
  }
  console.log('\x1b[32m全部通过\x1b[0m')
}

main().catch((err) => {
  console.error('\n\x1b[31m验收脚本异常终止：\x1b[0m', err.message)
  process.exit(1)
})
