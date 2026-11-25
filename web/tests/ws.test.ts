import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { WsClient, isRetryableClose, isRetryableHandshakeStatus } from '@/net/ws'
import type { ServerMessage } from '@/net/types'

/* --------------------------- 假 WebSocket --------------------------- */

type Handler = ((event: unknown) => void) | null

class FakeSocket {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3
  static instances: FakeSocket[] = []

  readyState = FakeSocket.CONNECTING
  onopen: Handler = null
  onmessage: Handler = null
  onerror: Handler = null
  onclose: Handler = null
  readonly sent: string[] = []
  readonly url: string

  constructor(url: string) {
    this.url = url
    FakeSocket.instances.push(this)
  }

  send(data: string): void {
    if (this.readyState !== FakeSocket.OPEN) throw new Error('socket not open')
    this.sent.push(data)
  }

  close(code = 1000, reason = ''): void {
    this.readyState = FakeSocket.CLOSED
    this.onclose?.({ code, reason })
  }

  /** 测试驱动：模拟升级成功 */
  open(): void {
    this.readyState = FakeSocket.OPEN
    this.onopen?.({})
  }

  /** 测试驱动：模拟收到一帧 */
  receive(data: unknown): void {
    this.onmessage?.({ data: typeof data === 'string' ? data : JSON.stringify(data) })
  }

  /** 测试驱动：模拟网络断开 */
  drop(code = 1006): void {
    this.readyState = FakeSocket.CLOSED
    this.onclose?.({ code, reason: 'network' })
  }

  static reset(): void {
    FakeSocket.instances = []
  }

  static get last(): FakeSocket {
    const socket = FakeSocket.instances[FakeSocket.instances.length - 1]
    if (!socket) throw new Error('没有创建过 socket')
    return socket
  }
}

const OPEN = 1

function makeClient(overrides: Partial<ConstructorParameters<typeof WsClient>[0]> = {}): {
  client: WsClient
  messages: { type: string; seq?: number }[]
  statuses: string[]
  closes: { code: number; willRetry: boolean }[]
  unknowns: string[]
} {
  const messages: { type: string; seq?: number }[] = []
  const statuses: string[] = []
  const closes: { code: number; willRetry: boolean }[] = []
  const unknowns: string[] = []

  const client = new WsClient(
    {
      roomId: 'K7M2QX',
      clientId: 'c_test',
      name: '我',
      // 测试在 Node 环境跑，没有 window；用固定 origin 构造握手 URL
      urlFactory: (params) => {
        const url = new URL('ws://localhost:8080/ws')
        url.searchParams.set('room', params.room.toUpperCase())
        url.searchParams.set('cid', params.cid)
        if (params.name) url.searchParams.set('name', params.name)
        if (params.key) url.searchParams.set('key', params.key)
        if (params.rt) url.searchParams.set('rt', params.rt)
        if (params.st) url.searchParams.set('st', params.st)
        return url.toString()
      },
      socketFactory: (url) => new FakeSocket(url) as unknown as WebSocket,
      ...overrides,
    },
    {
      onMessage: (message: ServerMessage, seq) => {
        messages.push({ type: message.type, ...(seq !== undefined ? { seq } : {}) })
      },
      onStatus: (status) => statuses.push(status),
      onClose: (code, _reason, willRetry) => closes.push({ code, willRetry }),
      onUnknownFrame: (reason) => unknowns.push(reason),
    },
  )
  return { client, messages, statuses, closes, unknowns }
}

beforeEach(() => {
  FakeSocket.reset()
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('WsClient：连接与握手 URL', () => {
  it('connect 建立 socket 且状态为 connecting', () => {
    const { client, statuses } = makeClient()
    client.connect()
    expect(FakeSocket.instances).toHaveLength(1)
    expect(statuses).toContain('connecting')
    expect(FakeSocket.last.url).toContain('room=K7M2QX')
    expect(FakeSocket.last.url).toContain('cid=c_test')
  })

  it('token/rt 进握手 URL（只读链路）', () => {
    const { client } = makeClient({ rt: 'eyJhbGciOi...' })
    client.connect()
    expect(FakeSocket.last.url).toContain('rt=')
  })

  it('editorKey 进握手 URL 的 key 参数', () => {
    const { client } = makeClient({ key: 'abc43chars' })
    client.connect()
    expect(FakeSocket.last.url).toContain('key=abc43chars')
  })

  it('session token 进握手 URL，且 setSession 会同时更新 cid', () => {
    const { client } = makeClient()
    client.setSession('sess-token', 'c_server_assigned')
    client.connect()
    expect(FakeSocket.last.url).toContain('st=sess-token')
    expect(FakeSocket.last.url).toContain('cid=c_server_assigned')
  })

  it('lastSeq 不出现在 URL（协议 §2.1 要求走 hello）', () => {
    const { client } = makeClient()
    client.connect()
    expect(FakeSocket.last.url).not.toContain('lastSeq')
  })

  it('open 后可以发 hello（joined 之前唯一允许的 type）', () => {
    const { client } = makeClient()
    client.connect()
    FakeSocket.last.open()
    expect(client.isJoined).toBe(false)
    // hello 允许先发
    // 这里直接用 send 验证 allowBeforeJoined 逻辑
    expect(client.send('hello', { lastSeq: 0, epoch: 0 }, true)).toBe(true)
    // op.add 在 joined 之前被拒（返回 false，由上层入 pending）
    expect(client.send('op.add', { opId: 'a' })).toBe(false)
  })

  it('joined 之后才允许 op.add', () => {
    const { client } = makeClient()
    client.connect()
    FakeSocket.last.open()
    FakeSocket.last.receive({
      v: 1,
      type: 'joined',
      roomId: 'K7M2QX',
      clientId: 'srv',
      ts: 1,
      payload: { you: { clientId: 'c', role: 'editor' }, room: { seq: 0, epoch: 1 }, limits: {} },
    })
    expect(client.isJoined).toBe(true)
    expect(client.send('op.add', { opId: 'a', action: 'create' })).toBe(true)
  })
})

describe('WsClient：白名单与脏帧', () => {
  it('未知 type 被忽略，不触发 onMessage、不断线', () => {
    const { client, messages, unknowns } = makeClient()
    client.connect()
    FakeSocket.last.open()
    FakeSocket.last.receive({
      v: 1,
      type: 'future.thing',
      roomId: 'K7M2QX',
      clientId: 'srv',
      ts: 1,
      payload: {},
    })
    expect(messages).toHaveLength(0)
    expect(unknowns).toContain('unknown_type')
    expect(FakeSocket.last.readyState).toBe(OPEN)
  })

  it('非 JSON 帧被忽略，连接保持', () => {
    const { client, messages, unknowns } = makeClient()
    client.connect()
    FakeSocket.last.open()
    FakeSocket.last.receive('<<< garbage >>>')
    expect(messages).toHaveLength(0)
    expect(unknowns).toContain('malformed')
    expect(FakeSocket.last.readyState).toBe(OPEN)
  })

  it('信封 seq 透传给上层（持久消息缺口检测要用）', () => {
    const { client, messages } = makeClient()
    client.connect()
    FakeSocket.last.open()
    FakeSocket.last.receive({
      v: 1,
      type: 'op.add',
      seq: 42,
      roomId: 'K7M2QX',
      clientId: 'peer',
      ts: 1,
      payload: { opId: 'x', action: 'create' },
    })
    expect(messages[0]).toEqual({ type: 'op.add', seq: 42 })
  })

  it('非持久消息没有 seq（透传 undefined）', () => {
    const { client, messages } = makeClient()
    client.connect()
    FakeSocket.last.open()
    FakeSocket.last.receive({
      v: 1,
      type: 'pong',
      roomId: 'K7M2QX',
      clientId: 'srv',
      ts: 1,
      payload: { ts: 1 },
    })
    expect(messages[0]).toEqual({ type: 'pong' })
  })
})

describe('WsClient：心跳与死链判定', () => {
  it('每 25s 发一次 ping', () => {
    const { client } = makeClient()
    client.connect()
    FakeSocket.last.open()
    // 每 25s 的心跳前先喂一个 pong，避免 45s 静默触发的死链判定
    // （死链判定本身由下一条用例单独覆盖）
    for (let i = 0; i < 2; i += 1) {
      vi.advanceTimersByTime(25_000)
      FakeSocket.last.receive({
        v: 1,
        type: 'pong',
        roomId: 'K7M2QX',
        clientId: 'srv',
        ts: 1,
        payload: { ts: 1 },
      })
    }
    const pings = FakeSocket.last.sent.filter((raw) => raw.includes('"ping"'))
    expect(pings.length).toBeGreaterThanOrEqual(2)
  })

  it('45s 无任何帧 → 主动断开重连（半开连接检测）', () => {
    const { client } = makeClient()
    client.connect()
    FakeSocket.last.open()
    const first = FakeSocket.last
    // 不推进 lastFrameAt（收不到帧），推进到超过 45s
    vi.advanceTimersByTime(50_000)
    // 心跳发现静默过久 → close 该 socket
    expect(first.readyState).toBe(3) // CLOSED
  })

  it('收到任意帧刷新 lastFrameAt（不会被误判死链）', () => {
    const { client } = makeClient()
    client.connect()
    FakeSocket.last.open()
    // 每 30s 收一次 pong，推进 120s 也不该判定死链
    for (let i = 0; i < 4; i += 1) {
      vi.advanceTimersByTime(30_000)
      FakeSocket.last.receive({
        v: 1,
        type: 'pong',
        roomId: 'K7M2QX',
        clientId: 'srv',
        ts: 1,
        payload: { ts: 1 },
      })
    }
    expect(FakeSocket.last.readyState).toBe(OPEN)
  })
})

describe('WsClient：退避重连', () => {
  it('断线后按 0.5s 首次重连（含 ±20% 抖动）', () => {
    const { client, statuses, closes } = makeClient()
    client.connect()
    FakeSocket.last.open()
    FakeSocket.last.drop(1006)
    expect(statuses).toContain('reconnecting')
    expect(closes[0]).toMatchObject({ code: 1006, willRetry: true })
    // 抖动区间 [400, 600]
    vi.advanceTimersByTime(700)
    expect(FakeSocket.instances.length).toBe(2)
  })

  it('连续失败时退避递增：0.5 → 1 → 2 → 4 → 8（上限 10s，含 ±20% 抖动）', () => {
    const { client } = makeClient()
    client.connect()
    const delays: number[] = []

    for (let attempt = 0; attempt < 6; attempt += 1) {
      FakeSocket.last.drop(1006)
      let elapsed = 0
      const before = FakeSocket.instances.length
      while (FakeSocket.instances.length === before && elapsed < 15_000) {
        vi.advanceTimersByTime(100)
        elapsed += 100
      }
      delays.push(elapsed)
    }

    // 第一轮 0.5s ±20% → [400, 600]（量化到 100ms 粒度后最多 700）
    expect(delays[0]).toBeGreaterThanOrEqual(400)
    expect(delays[0]).toBeLessThanOrEqual(700)
    // 最后一轮应落在 8s ±20% → 抖动上限会顶到 10s×1.2 = 12s
    // （BACKOFF_MS 上限 10000，再叠加 ±20% 抖动）
    const last = delays[delays.length - 1] ?? 0
    expect(last).toBeGreaterThanOrEqual(6000)
    expect(last).toBeLessThanOrEqual(12_000)
    // 整体趋势必须递增（第 4 轮起明显大于第 1 轮）
    expect(delays[3] ?? 0).toBeGreaterThan((delays[0] ?? 0) * 2)
    void client
  })

  it('成功连接后退避计数归零（下次断线仍是 0.5s 起）', () => {
    const { client } = makeClient()
    client.connect()
    FakeSocket.last.open()
    FakeSocket.last.drop(1006)
    vi.advanceTimersByTime(700) // 重连一次
    FakeSocket.last.open() // 成功
    FakeSocket.last.drop(1006) // 再断
    const before = FakeSocket.instances.length
    vi.advanceTimersByTime(700) // 应该是快速重连（0.5s 级），不是 1s+
    expect(FakeSocket.instances.length).toBe(before + 1)
  })

  it('1008（策略违规）不重连', () => {
    const { client, closes } = makeClient()
    client.connect()
    FakeSocket.last.open()
    FakeSocket.last.drop(1008)
    expect(closes[0]?.willRetry).toBe(false)
    vi.advanceTimersByTime(2000)
    expect(FakeSocket.instances).toHaveLength(1)
  })

  it('主动 disconnect 后不再重连', () => {
    const { client } = makeClient()
    client.connect()
    FakeSocket.last.open()
    client.disconnect()
    vi.advanceTimersByTime(20_000)
    expect(FakeSocket.instances).toHaveLength(1)
  })
})

describe('WsClient：前台恢复检查', () => {
  it('socket 已关闭时 checkAlive 立刻重连（不等退避）', () => {
    const { client } = makeClient()
    client.connect()
    FakeSocket.last.open()
    // 模拟设备休眠：socket 挂了但 onclose 没触发（直接标记状态）
    FakeSocket.last.readyState = 3
    const before = FakeSocket.instances.length
    client.checkAlive()
    expect(FakeSocket.instances.length).toBe(before + 1)
  })

  it('静默超过 45s 时 checkAlive 触发重连', () => {
    const { client } = makeClient()
    client.connect()
    FakeSocket.last.open()
    const before = FakeSocket.instances.length
    // 直接把系统时间推进
    const realNow = Date.now
    Date.now = () => realNow() + 60_000
    try {
      client.checkAlive()
    } finally {
      Date.now = realNow
    }
    expect(FakeSocket.instances.length).toBe(before + 1)
  })
})

describe('WsClient：错误处理', () => {
  it('fatal 错误停止重连（避免风暴）', () => {
    const { client, statuses } = makeClient()
    client.connect()
    FakeSocket.last.open()
    FakeSocket.last.receive({
      v: 1,
      type: 'error',
      roomId: 'K7M2QX',
      clientId: 'srv',
      ts: 1,
      payload: { code: 'unsupported_version', message: 'too old', fatal: true },
    })
    expect(statuses).toContain('offline')
  })

  it('非 fatal 错误不断线', () => {
    const { client } = makeClient()
    client.connect()
    FakeSocket.last.open()
    FakeSocket.last.receive({
      v: 1,
      type: 'error',
      roomId: 'K7M2QX',
      clientId: 'srv',
      ts: 1,
      payload: { code: 'rate_limited', message: 'slow down', fatal: false },
    })
    expect(FakeSocket.last.readyState).toBe(OPEN)
  })
})

describe('错误码可重试判定', () => {
  it('1008 不可重试，其余可重试', () => {
    expect(isRetryableClose(1008)).toBe(false)
    expect(isRetryableClose(1006)).toBe(true)
    expect(isRetryableClose(4001)).toBe(true)
    expect(isRetryableClose(1000)).toBe(true)
  })

  it('握手 429/503/403/404 不可重试', () => {
    expect(isRetryableHandshakeStatus(429)).toBe(false)
    expect(isRetryableHandshakeStatus(503)).toBe(false)
    expect(isRetryableHandshakeStatus(403)).toBe(false)
    expect(isRetryableHandshakeStatus(404)).toBe(false)
    expect(isRetryableHandshakeStatus(500)).toBe(true)
    expect(isRetryableHandshakeStatus(200)).toBe(true)
  })
})
