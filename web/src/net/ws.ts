/**
 * WebSocket 客户端：连接、心跳、退避重连、状态机。
 *
 * 契约：docs/PROTOCOL.md §2（握手/信封）、§3（出站）、§6.2（状态机）、§6.4（重连）。
 *
 * 职责边界（刻意做窄）：
 *   这里只管**连接与帧收发 + 断线判定**，不碰图形数据。
 *   序号、pending、乐观渲染全在 `realtime.ts` / `seq.ts`——
 *   那两层可以在没有浏览器的情况下单测，这是本项目质量的关键。
 */
import { buildEnvelope, parseFrame, serializeEnvelope } from './envelope'
import type { EnvelopeContext } from './envelope'
import { buildWsUrl, type HandshakeParams } from './rest'
import type { ClientMessageType, ConnectionStatus, Envelope, ServerMessage } from './types'
import { errorText } from './types'

/** 退避序列（协议 §6.4：0.5 → 1 → 2 → 4 → 8，上限 8s；这里上限取 10s 更保守）。 */
const BACKOFF_MS = [500, 1000, 2000, 4000, 8000, 10000] as const
/** 心跳间隔：25s 发 ping（协议 §3.5 建议 20s，取 25s 留余量）。 */
const PING_INTERVAL_MS = 25_000
/** 45s 没收到任何帧 → 判定死链（覆盖半开连接）。 */
const DEAD_LINK_MS = 45_000

/** WebSocket readyState 常量（不用全局 WebSocket，SSR/测试环境没有它）。 */
const SOCKET_CONNECTING = 0
const SOCKET_OPEN = 1

/** 抖动 ±20%，避免断网恢复后所有客户端同时重连（惊群）。 */
function withJitter(ms: number): number {
  const factor = 0.8 + Math.random() * 0.4
  return Math.round(ms * factor)
}

export interface WsClientOptions {
  roomId: string
  clientId: string
  name: string
  key?: string
  /** 只读 token（`?rt=`）；存在时服务端强制 viewer */
  rt?: string
  /** 服务端签发的会话 token（`?st=`）；重连时必须原样带回 */
  sessionToken?: string
  /** WS URL 构造函数（测试注入用） */
  urlFactory?: (params: HandshakeParams) => string
  /** WebSocket 构造器（测试注入假实现） */
  socketFactory?: (url: string) => WebSocket
  now?: () => number
}

export type WsListener = {
  onOpen?: () => void
  /** `envelopeSeq` 为信封顶层 seq（只有持久消息有），用于缺口检测 */
  onMessage?: (message: ServerMessage, envelopeSeq: number | undefined) => void
  /** 无法解析 / 未知 type 的帧（不打断连接） */
  onUnknownFrame?: (reason: string, type: string) => void
  onStatus?: (status: ConnectionStatus) => void
  /** 收到服务端 error 帧 */
  onServerError?: (code: string, message: string, fatal: boolean) => void
  /** 连接关闭。`willRetry` 表示是否会自动重连 */
  onClose?: (code: number, reason: string, willRetry: boolean) => void
}

/** 连接关闭原因：握手被 HTTP 拒绝时浏览器只给 1006，我们靠 HTTP 探测补足。 */
export interface CloseInfo {
  code: number
  reason: string
  retryable: boolean
}

export class WsClient {
  private socket: WebSocket | null = null
  private status: ConnectionStatus = 'offline'
  private retryIndex = 0
  private retryTimer: ReturnType<typeof setTimeout> | null = null
  private pingTimer: ReturnType<typeof setInterval> | null = null
  private lastFrameAt = 0
  private disposed = false
  /** 收到过 joined，才允许发 op.add（协议 §3 时序） */
  private joined = false
  /** 演示用：在此时间戳之前拒绝建连（模拟持续断网） */
  private offlineUntil = 0
  private readonly listener: WsListener
  private readonly envelopeCtx: EnvelopeContext

  constructor(
    private readonly options: WsClientOptions,
    listener: WsListener,
  ) {
    this.listener = listener
    this.envelopeCtx = {
      roomId: options.roomId.toUpperCase(),
      clientId: options.clientId,
      ...(options.now ? { now: options.now } : {}),
    }
  }

  get currentStatus(): ConnectionStatus {
    return this.status
  }

  get isJoined(): boolean {
    return this.joined
  }

  /**
   * 采用服务端下发的会话身份。
   *
   * 服务端可能拒绝客户端自报的 cid（没有有效 token 时），并返回新的
   * `you.clientId` 与 `sessionToken`。此后的握手与信封都必须使用这一对值。
   */
  setSession(token: string, clientId: string): void {
    if (token) this.options.sessionToken = token
    if (clientId && clientId !== this.options.clientId) {
      this.options.clientId = clientId
      this.envelopeCtx.clientId = clientId
    }
  }

  private setStatus(next: ConnectionStatus): void {
    if (this.status === next) return
    this.status = next
    this.listener.onStatus?.(next)
  }

  /* -------------------------------- 连接 --------------------------------- */

  connect(): void {
    if (this.disposed) return
    this.clearRetry()
    // 演示用强制离线窗口：期间直接排下一次重试，不真的建连
    if (this.offlineUntil > Date.now()) {
      this.setStatus('reconnecting')
      this.scheduleRetry('forced_offline')
      return
    }
    // 注意：这里必须用状态数字而不是 WebSocket.OPEN（Node/SSR 下没有全局 WebSocket）
    const state = this.socket?.readyState
    if (state === SOCKET_CONNECTING || state === SOCKET_OPEN) return
    // 旧 socket 已关闭：先斩断引用，避免 close 回调再次触发重连
    this.socket = null
    this.setStatus(this.retryIndex === 0 ? 'connecting' : 'reconnecting')

    const params: HandshakeParams = {
      room: this.options.roomId,
      cid: this.options.clientId,
      name: this.options.name,
      ...(this.options.key ? { key: this.options.key } : {}),
      ...(this.options.rt ? { rt: this.options.rt } : {}),
      ...(this.options.sessionToken ? { st: this.options.sessionToken } : {}),
    }
    const url = this.options.urlFactory ? this.options.urlFactory(params) : buildWsUrl(params)
    const factory = this.options.socketFactory ?? ((target: string) => new WebSocket(target))

    let socket: WebSocket
    try {
      socket = factory(url)
    } catch (error) {
      this.scheduleRetry(String(error))
      return
    }
    this.socket = socket
    this.lastFrameAt = Date.now()

    socket.onopen = () => {
      if (this.disposed) return
      this.retryIndex = 0
      this.lastFrameAt = Date.now()
      // 连接层已通；应用层（online）要等 joined + 同步完成，由 realtime 决定。
      // 这里把状态从 connecting/reconnecting 推进到「已连上、待握手」的语义位置：
      // 仍报 connecting，是因为此时还不能发 op.add（协议 §3 时序）。
      this.startHeartbeat()
      this.listener.onOpen?.()
    }

    socket.onmessage = (event: MessageEvent) => {
      if (this.disposed) return
      this.lastFrameAt = Date.now()
      this.handleRaw(typeof event.data === 'string' ? event.data : '')
    }

    socket.onerror = () => {
      // 具体原因不可得（浏览器安全限制）；onclose 会紧接着触发
    }

    socket.onclose = (event: CloseEvent) => {
      if (this.disposed) return
      this.stopHeartbeat()
      this.socket = null
      this.joined = false
      const retryable = isRetryableClose(event.code)
      if (!retryable) {
        this.setStatus('offline')
        this.listener.onClose?.(event.code, event.reason || 'closed', false)
        return
      }
      this.setStatus('reconnecting')
      this.listener.onClose?.(event.code, event.reason || 'network', true)
      this.scheduleRetry(event.reason || `close_${event.code}`)
    }
  }

  /* --------------------------------- 收帧 --------------------------------- */

  private handleRaw(raw: string): void {
    if (!raw) return
    const parsed = parseFrame(raw)
    if (!parsed.ok) {
      // 未知 type / 脏帧 / 版本不符：一律忽略，绝不断线（协议 §2.3.1）
      this.listener.onUnknownFrame?.(parsed.reason, parsed.type)
      return
    }
    const message = parsed.message
    if (message.type === 'joined') this.joined = true
    if (message.type === 'error') {
      const payload = message.payload
      this.listener.onServerError?.(
        payload.code,
        payload.message || errorText(payload.code),
        payload.fatal === true,
      )
      if (payload.fatal === true) {
        // 服务端会随后关闭连接；这里先停掉重连以免风暴
        this.stopHeartbeat()
        this.joined = false
        this.setStatus('offline')
      }
    }
    // 信封顶层 seq 只有持久消息才有（op.add / room.meta）
    this.listener.onMessage?.(message, typeof message.seq === 'number' ? message.seq : undefined)
  }

  /* --------------------------------- 发帧 --------------------------------- */

  /**
   * 发送一条消息。**未连接时不静默丢弃**：返回 false 由上层入 pending 队列。
   * `allowBeforeJoined` 仅 `hello` 使用（协议 §3 时序允许先发 hello）。
   */
  send<T>(type: ClientMessageType, payload: T, allowBeforeJoined = false): boolean {
    const socket = this.socket
    if (!socket || socket.readyState !== SOCKET_OPEN) return false
    if (!this.joined && !allowBeforeJoined) return false

    try {
      const envelope = buildEnvelope(this.envelopeCtx, type, payload)
      socket.send(serializeEnvelope(envelope))
      return true
    } catch {
      // 序列化失败（超大帧）或 socket 已关：由上层降级处理
      return false
    }
  }

  /** 发送已构造好的信封（重放 pending 时避免重新编号）。 */
  sendEnvelope(envelope: Envelope<unknown>): boolean {
    const socket = this.socket
    if (!socket || socket.readyState !== SOCKET_OPEN) return false
    try {
      socket.send(serializeEnvelope(envelope))
      return true
    } catch {
      return false
    }
  }

  /* -------------------------------- 心跳 --------------------------------- */

  private startHeartbeat(): void {
    this.stopHeartbeat()
    this.pingTimer = setInterval(() => {
      const silent = Date.now() - this.lastFrameAt
      if (silent > DEAD_LINK_MS) {
        // 半开连接：浏览器不会自己发现，必须主动断
        this.forceReconnect('heartbeat_timeout')
        return
      }
      void silent
      this.send('ping', { ts: Date.now() }, true)
    }, PING_INTERVAL_MS)
  }

  private stopHeartbeat(): void {
    if (this.pingTimer) {
      clearInterval(this.pingTimer)
      this.pingTimer = null
    }
  }

  /**
   * 前台恢复时调用：如果链路静默过久或已断开，立刻重连（不等退避计时器）。
   * 移动端切回前台时 socket 常常已经是死的但 onclose 还没触发。
   */
  checkAlive(): void {
    if (this.disposed) return
    const socket = this.socket
    if (!socket) {
      this.retryIndex = 0
      this.connect()
      return
    }
    // socket 已关/正在关：走强制重连（先断开引用再连，避免旧 close 回调抢跑）
    if (socket.readyState !== SOCKET_OPEN) {
      this.reconnectNow('socket_not_open')
      return
    }
    if (Date.now() - this.lastFrameAt > DEAD_LINK_MS) {
      this.reconnectNow('silent_too_long')
    }
  }

  /**
   * 立刻重连（跳过退避等待）。
   * 用于「前台恢复」「socket 已死但 onclose 未触发」这类场景：
   * 退避计时器此刻只会白白拖延恢复。
   */
  private reconnectNow(reason: string): void {
    this.clearRetry()
    this.stopHeartbeat()
    const old = this.socket
    this.socket = null
    this.joined = false
    this.retryIndex = 0
    if (old) {
      // 摘掉回调，避免旧 socket 的 close 干扰新连接
      old.onclose = null
      old.onerror = null
      old.onmessage = null
      old.onopen = null
      try {
        if (old.readyState === SOCKET_OPEN || old.readyState === SOCKET_CONNECTING) old.close(4000, reason)
      } catch {
        /* 已关闭 */
      }
    }
    this.setStatus('reconnecting')
    this.connect()
  }

  private forceReconnect(reason: string): void {
    const socket = this.socket
    this.stopHeartbeat()
    this.socket = null
    this.joined = false
    try {
      socket?.close(4000, reason)
    } catch {
      /* 已关闭 */
    }
    this.setStatus('reconnecting')
    this.retryIndex = 0
    this.scheduleRetry(reason)
  }

  /* -------------------------------- 重连 --------------------------------- */

  private scheduleRetry(reason: string): void {
    if (this.disposed || this.retryTimer) return
    void reason
    const base = BACKOFF_MS[Math.min(this.retryIndex, BACKOFF_MS.length - 1)] ?? 10_000
    this.retryIndex += 1
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null
      this.connect()
    }, withJitter(base))
  }

  private clearRetry(): void {
    if (this.retryTimer) {
      clearTimeout(this.retryTimer)
      this.retryTimer = null
    }
  }

  /** 主动断开（离开房间 / 页面卸载）。不再自动重连。 */
  disconnect(): void {
    this.disposed = true
    this.clearRetry()
    this.stopHeartbeat()
    const socket = this.socket
    this.socket = null
    if (socket) {
      try {
        if (socket.readyState === SOCKET_OPEN) {
          this.send('bye', {}, true)
        }
        socket.close(1000, 'client_bye')
      } catch {
        /* noop */
      }
    }
    this.joined = false
    this.setStatus('offline')
  }

  /** 仅测试用：重置退避计数 */
  resetBackoffForTest(): void {
    this.retryIndex = 0
  }

  /**
   * 仅测试/演示用：模拟网络骤断（不设置 disposed，因此会自动重连）。
   * 用于在 DevTools 里复现「地铁断网 → 恢复」的完整链路。
   */
  simulateNetworkDrop(reason = 'simulated_drop'): void {
    const socket = this.socket
    this.stopHeartbeat()
    this.socket = null
    this.joined = false
    if (socket) {
      socket.onclose = null
      socket.onerror = null
      socket.onmessage = null
      socket.onopen = null
      try {
        socket.close(4000, reason)
      } catch {
        /* 已关闭 */
      }
    }
    this.setStatus('reconnecting')
    // 必须显式告诉上层「连接已断」：否则 realtime 还认为 transport 可用，
    // 状态胶囊会一直显示「已连接」，而 pending 永远不重放。
    this.listener.onClose?.(4000, reason, true)
    this.retryIndex = 0
    this.scheduleRetry(reason)
  }

  /**
   * 仅测试/演示用：把链路「钉死」在离线态 N 毫秒（期间 connect 直接失败），
   * 用来演示 F6「断网继续画 → 恢复后补齐」。
   * 不改变任何业务状态，只让连接层真实不可用。
   */
  forceOffline(durationMs: number): void {
    this.simulateNetworkDrop('forced_offline')
    this.offlineUntil = Date.now() + durationMs
  }

  /** 仅测试/演示用：立刻恢复（提前结束强制离线） */
  forceOnline(): void {
    this.offlineUntil = 0
    this.retryIndex = 0
    this.clearRetry()
    this.connect()
  }
}

/** 这些 close code 重试无意义（策略违规/鉴权失败）。 */
export function isRetryableClose(code: number): boolean {
  // 1008 = policy violation（Origin/鉴权），1009 内部错误也常由协议不兼容引起
  return code !== 1008
}

/** 握手阶段的 HTTP 状态 → 是否该重试（供上层在 REST 探测后判定）。 */
export function isRetryableHandshakeStatus(status: number): boolean {
  return status !== 429 && status !== 503 && status !== 403 && status !== 404
}

export type { Envelope }
