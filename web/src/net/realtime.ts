/**
 * 实时协调器：把 WS、序号状态机、pending 队列、乐观渲染串成一条线。
 *
 * 分层理由：`seq.ts` 是纯函数（可单测），`ws.ts` 只管帧收发。
 * 这里负责**策略**——何时发 op、何时重放、何时回滚、何时切 online。
 * 依赖全部由接口注入，因此能用假 Transport 测完整流程（见 tests/realtime.test.ts）。
 *
 * 契约：docs/PROTOCOL.md §3（时序）、§4（消息）、§6（seq/pending/补齐）。
 */
import { buildEnvelope } from './envelope'
import type { Envelope } from './envelope'
import type { PersistedOp, PersistedRoomState } from './persistence'
import {
  applySnapshotSeq,
  collectSnapshot,
  createAssembly,
  createPendingQueue,
  createResyncThrottle,
  createSeqState,
  dequeuePending,
  drainBuffer,
  enqueuePending,
  findPending,
  isAssemblyStale,
  needsResync,
  receiveEvent,
  replayOrder,
  type PendingOp,
  type SeqEvent,
  type SeqState,
} from './seq'
import type {
  AckPayload,
  ClientMessageType,
  ConnectionStatus,
  CursorBroadcastPayload,
  JoinedPayload,
  MemberRolePayload,
  OpAddPayload,
  OpBroadcastPayload,
  PresencePatchPayload,
  PresenceUser,
  ResyncEvent,
  ResyncEventsPayload,
  RoomLimits,
  ServerMessage,
  SnapshotPayload,
} from './types'
import { DEFAULT_LIMITS } from './types'
import type { Shape } from '@/canvas/shapes'

/* ------------------------------- 依赖注入 ------------------------------- */

export interface Transport {
  connect(): void
  disconnect(): void
  send<T>(type: ClientMessageType, payload: T, allowBeforeJoined?: boolean): boolean
  sendEnvelope(envelope: Envelope<unknown>): boolean
  checkAlive(): void
}

export interface Persistence {
  load(): Promise<PersistedRoomState | null>
  save(state: { appliedSeq: number; epoch: number; pending: readonly PersistedOp[] }): void
}

/** 渲染层钩子：由 store 实现，协调器不直接碰 Pinia。 */
export interface ScenePort {
  /** 应用一条远端事件（已去重、已按序） */
  applyRemoteOp(payload: OpBroadcastPayload): void
  /** 快照整体替换（收齐后调用一次）。入参是**协议形状**，由实现转本地模型 */
  replaceAll(shapes: readonly unknown[], seq: number, epoch: number): void
  /** 乐观渲染：本地 op 立即上屏，pending 60% 不透明度 */
  applyLocalOp(payload: OpAddPayload): void
  /** 回滚某条 op 的本地效果（只删仍处于 pending 的图形） */
  rollbackOp(opId: string, shapeIds: readonly string[]): void
  /** 把某条 op 转为已确认（去掉 pending 标记）；需要 shapIds 才能精确定位 */
  confirmOp(opId: string, shapeIds: readonly string[]): void
  /** 当前图形 id 列表（snapshot 后清理已在快照中的 pending） */
  listShapeIds(): readonly string[]
  /**
   * 应用一条成员角色变更（`member.role`，PERMISSIONS §3.1 / §6.2）。
   * 与 op 一样走 seq 通道，因此这里只负责「拿到已按序的 payload 后怎么改状态」。
   */
  applyMemberRole(payload: MemberRolePayload): void
}

export interface RealtimeListeners {
  onStatus(status: ConnectionStatus, pending: number): void
  onJoined(payload: JoinedPayload): void
  onMembers(users: readonly PresenceUser[]): void
  onCursor(cursor: CursorBroadcastPayload): void
  onToast(message: string, tone: 'info' | 'ok' | 'warn'): void
  onError(code: string, message: string, fatal: boolean): void
  onRoomNotFound(): void
  /** 消息类型白名单外的帧（仅诊断） */
  onUnknownFrame?(reason: string, type: string): void
}
export interface RealtimeOptions {
  roomId: string
  clientId: string
  displayName: string
  key?: string
  rt?: string
  transport: Transport
  scene: ScenePort
  listeners: RealtimeListeners
  persistence?: Persistence
  now?: () => number
  /**
   * 断线重试若干次后用来确认「房间是否还存在」。
   * 浏览器不允许读取 WS 握手失败原因，只能靠 REST 补足语义（避免无限「重连中」）。
   * 返回 false 表示房间确实不存在/已过期。
   */
  probeRoom?: () => Promise<boolean>
  /**
   * 大房间分页兜底：`snapshot_too_large` 时调用，由调用方用
   * `GET /api/rooms/{code}/snapshot` 翻完所有页并返回完整集合。
   * 返回值会走与 WS 分块快照相同的 `commitSnapshot` 路径。
   */
  loadSnapshot?: () => Promise<{ shapes: readonly unknown[]; seq: number; epoch: number }>
}

/**
 * 持久事件体：缺口补齐后要能直接应用，所以 body 一起存。
 *
 * `member.role` 与 `op.add` **共用同一条 seq 通道**（PERMISSIONS §10）：
 * 角色变更会改变「谁有权写」这一房间可观察状态，必须能被断线补齐。
 */
export type PersistentBody =
  | { readonly kind: 'op'; readonly payload: OpBroadcastPayload }
  | { readonly kind: 'member.role'; readonly payload: MemberRolePayload }

/* --------------------------------- 协调器 -------------------------------- */

export class RealtimeClient {
  private readonly seqState: SeqState<PersistentBody> = createSeqState<PersistentBody>()
  private pending: PendingOp[] = []
  private assembly = createAssembly<Shape>()
  private status: ConnectionStatus = 'connecting'
  private joinedPayload: JoinedPayload | null = null
  private limits: RoomLimits = DEFAULT_LIMITS
  /** 已应用 opId 去重集合（协议 §6.4 零重复） */
  private readonly appliedOpIds = new Set<string>()
  /** 已见过的 shape id（epoch 变更时清理） */
  /**
   * resync.request 本地节流：1 秒内最多发 1 次，窗口内的后续请求用 trailing 合并
   * （最后一次必须补发，否则期间新产生的缺口不会被拉取）。
   * 目的：不依赖服务端 `rate_limited` 兜底，避免多付一次退避。
   */
  private readonly resyncThrottle = createResyncThrottle(() => this.sendResyncNow())
  private cursorLastSent = 0
  private waitingForResync = false
  private disposed = false
  /** 连续重连次数（用于触发房间探测） */
  private reconnectAttempts = 0
  /** 正在探测房间是否存在（避免并发探测） */
  private probing = false
  /** 最近一次 resync 的原因（trailing 补发时用最新值） */
  private pendingResyncReason: 'gap' | 'hello' | 'manual' | null = null
  /** REST 分页快照正在加载（避免并发翻页） */
  private restSnapshotLoading = false
  /** 传输层当前是否真的连通（没有它就不能宣称 online） */
  private transportUp = false

  constructor(private readonly options: RealtimeOptions) {}

  /* ------------------------------ 生命周期 ------------------------------ */

  async start(): Promise<void> {
    await this.restore()
    this.emitStatus()
    this.options.transport.connect()
  }

  private async restore(): Promise<void> {
    const persistence = this.options.persistence
    if (!persistence) return
    const saved = await persistence.load()
    if (!saved) return

    // Do NOT restore appliedSeq: it is persisted together with the pending queue,
    // but the scene itself (the shapes) is not. After a page reload the canvas is
    // empty while the stored seq claims "already synced up to N", so asking the
    // server for events after N returns nothing and the user sees a blank board.
    //
    // Resetting to 0 forces a full snapshot on every page load. That is the
    // correct trade: a snapshot is a few KB and always authoritative, whereas a
    // stale seq silently loses the entire drawing.
    //
    // Live reconnects (same page, transport dropped) do not go through here, so
    // the incremental fast path is unaffected.
    this.seqState.appliedSeq = 0
    this.seqState.epoch = saved.epoch

    // Pending ops are safe to restore: they are local edits not yet acknowledged,
    // and they are replayed with their original opIds after the snapshot lands.
    this.pending = createPendingQueue(
      saved.pending.map((op) => ({
        opId: op.opId,
        payload: op.payload,
        createdAt: op.createdAt,
        shapeIds: op.shapeIds,
      })),
    )
  }

  private persist(): void {
    this.options.persistence?.save({
      appliedSeq: this.seqState.appliedSeq,
      epoch: this.seqState.epoch,
      pending: this.pending.map((op) => ({
        opId: op.opId,
        payload: op.payload,
        createdAt: op.createdAt,
        shapeIds: op.shapeIds,
      })),
    })
  }

  stop(): void {
    if (this.disposed) return
    this.disposed = true
    this.resyncThrottle.cancel()
    this.pendingResyncReason = null
    this.options.transport.disconnect()
    this.persist()
  }

  /**
   * 采用服务端下发的权威 clientId。
   *
   * `applyRemoteBody` 用 clientId 判断“这是不是我自己的 op”，握手 hello 也
   * 用它构造信封；服务端可能拒绝客户端候选 cid，所以 joined 之后必须同步。
   */
  setClientId(clientId: string): void {
    if (!clientId) return
    this.options.clientId = clientId
  }

  /* -------------------------------- 状态 -------------------------------- */

  get currentStatus(): ConnectionStatus {
    return this.status
  }

  get pendingCount(): number {
    return this.pending.length
  }

  get appliedSeq(): number {
    return this.seqState.appliedSeq
  }

  get currentEpoch(): number {
    return this.seqState.epoch
  }

  get roomLimits(): RoomLimits {
    return this.limits
  }

  get joinedInfo(): JoinedPayload | null {
    return this.joinedPayload
  }

  private setStatus(next: ConnectionStatus): void {
    if (this.status === next) return
    this.status = next
    this.emitStatus()
  }

  private emitStatus(): void {
    this.options.listeners.onStatus(this.status, this.pending.length)
  }

  private now(): number {
    return (this.options.now ?? Date.now)()
  }

  /* ------------------------------- 连接回调 ------------------------------ */

  /** WS open：立刻发 hello（与 joined 并发，互不等待；协议 §3 时序）。 */
  onOpen(): void {
    if (this.disposed) return
    this.transportUp = true
    const envelope = buildEnvelope(
      { roomId: this.options.roomId, clientId: this.options.clientId, now: () => this.now() },
      'hello',
      {
        lastSeq: this.seqState.appliedSeq,
        epoch: this.seqState.epoch,
        clientVersion: '1.0.0',
        viewport: {
          w: typeof window !== 'undefined' ? window.innerWidth : 0,
          h: typeof window !== 'undefined' ? window.innerHeight : 0,
          dpr: typeof window !== 'undefined' ? window.devicePixelRatio || 1 : 1,
        },
      },
    )
    this.options.transport.sendEnvelope(envelope)
  }

  onClose(_code: number, _reason: string, willRetry: boolean): void {
    if (this.disposed) return
    this.transportUp = false
    this.joinedPayload = null
    // 连接断了：排队的 trailing resync 已无意义（下次 open/joined 会重新判断）
    this.resyncThrottle.cancel()
    this.pendingResyncReason = null
    this.assembly = createAssembly<Shape>()
    this.waitingForResync = false
    this.setStatus(willRetry ? 'reconnecting' : 'offline')

    if (!willRetry) {
      this.reconnectAttempts = 0
      return
    }

    // 连续重试却始终拿不到 joined：很可能是房间已被清理（服务端重启/过期）。
    // 浏览器不让读握手失败原因（只有 1006），所以用一次 REST 探测补足语义，
    // 避免用户对着「重连中…」无限等待。
    this.reconnectAttempts += 1
    const shouldProbe = this.reconnectAttempts >= 3 && !this.probing
    if (!shouldProbe || !this.options.probeRoom) return

    this.probing = true
    void this.options
      .probeRoom()
      .then((alive) => {
        if (!alive) {
          this.options.listeners.onRoomNotFound()
          this.stop()
        }
      })
      .catch(() => {
        /* 探测失败（网络还没好）：继续重连，不误判房间不存在 */
      })
      .finally(() => {
        this.probing = false
      })
  }

  onUnknownFrame(reason: string, type: string): void {
    this.options.listeners.onUnknownFrame?.(reason, type)
  }

  /* ------------------------------- 消息入口 ------------------------------ */

  /** 测试/调试：最近 N 条入站消息（排查「收到了但没应用」用） */
  readonly inboundLog: { type: string; seq?: number }[] = []

  onMessage(message: ServerMessage, envelopeSeq?: number): void {
    this.inboundLog.push({
      type: message.type,
      ...(envelopeSeq !== undefined ? { seq: envelopeSeq } : {}),
    })
    if (this.inboundLog.length > 40) this.inboundLog.shift()
    switch (message.type) {
      case 'joined':
        this.handleJoined(message.payload)
        break
      case 'resync.snapshot':
        this.handleSnapshot(message.payload)
        break
      case 'resync.events':
        this.handleResyncEvents(message.payload)
        break
      case 'op.add':
        if (typeof envelopeSeq === 'number') {
          this.onPersistent(envelopeSeq, message.payload)
        }
        break
      case 'member.role':
        // 带 seq 的持久消息：必须走与 op.add 相同的缺口检测 / 补齐路径。
        // 万一服务端漏了 seq（实现缺陷），退化为直接应用而不是静默丢弃——
        // 角色应用是幂等的，重复应用不会产生错误状态。
        if (typeof envelopeSeq === 'number') {
          this.onPersistent(envelopeSeq, message.payload)
        } else {
          this.options.scene.applyMemberRole(message.payload)
        }
        break
      case 'ack':
        this.handleAck(message.payload)
        break
      case 'presence.join':
      case 'presence.leave':
        this.handlePresence(message.payload)
        break
      case 'presence.cursor':
        this.options.listeners.onCursor(message.payload)
        break
      case 'pong':
        break
      case 'room.meta':
        break
      case 'error':
        this.handleError(message.payload.code, message.payload.message, message.payload.fatal === true)
        break
      default:
        break
    }
  }

  /** 服务端 error 帧 / ack.rejected 的统一处理。 */
  handleError(code: string, message: string, fatal: boolean): void {
    this.options.listeners.onError(code, message, fatal)
    if (code === 'room_not_found') {
      this.options.listeners.onRoomNotFound()
      return
    }
    if (code === 'forbidden') {
      this.options.listeners.onToast('没有权限执行这个操作（仅房主可管理成员）', 'warn')
      return
    }
    if (code === 'target_offline') {
      this.options.listeners.onToast('对方已经离开房间', 'warn')
      return
    }
    if (code === 'invalid_grant') {
      this.options.listeners.onToast('授权参数无效，转移房主请用「转移房主」', 'warn')
      return
    }
    if (code === 'snapshot_too_large') {
      // WS 快照不适用于大房间（协议 §4.2）：自动降级到 REST 分页，
      // 拉完后**复用同一条 commitSnapshot 路径**替换视图，不写第二套逻辑。
      this.waitingForResync = false
      this.options.listeners.onToast('房间内容较多，正在改用分页加载', 'info')
      void this.loadSnapshotViaRest()
      return
    }
    if (fatal) {
      this.setStatus('offline')
      this.options.listeners.onToast(message, 'warn')
    }
  }

  /* ------------------------------- joined ------------------------------- */

  private handleJoined(payload: JoinedPayload): void {
    if (!isJoinedPayload(payload)) {
      this.options.listeners.onToast('房间信息不完整，请刷新重试', 'warn')
      return
    }
    const firstJoin = this.joinedPayload === null
    this.joinedPayload = payload
    this.limits = { ...DEFAULT_LIMITS, ...payload.limits }

    const serverEpoch = payload.room.epoch
    if (this.seqState.epoch !== 0 && serverEpoch !== this.seqState.epoch) {
      // epoch 变化 = 本地缓存作废（协议 §6.6）；回滚未确认的乐观渲染
      for (const op of this.pending) {
        this.options.scene.rollbackOp(op.opId, op.shapeIds)
      }
      this.pending = []
      this.appliedOpIds.clear()
      this.seqState.appliedSeq = 0
      this.seqState.buffer.clear()
      this.seqState.epoch = serverEpoch
      this.persist()
      this.options.listeners.onToast('房间已重置，正在重新同步', 'info')
    } else if (this.seqState.epoch === 0) {
      this.seqState.epoch = serverEpoch
    }

    this.options.listeners.onJoined(payload)
    this.reconnectAttempts = 0

    // 重复的 joined（服务端重发）不应重置到 reconnecting，否则会闪一下状态胶囊
    if (!firstJoin && this.status === 'online') {
      if (needsResync(this.seqState, payload.room.seq)) this.requestResync('hello')
      return
    }

    this.setStatus('reconnecting')

    if (needsResync(this.seqState, payload.room.seq)) {
      this.requestResync('hello')
    } else {
      this.enterLiveIfReady()
    }
  }

  /* ------------------------------ resync 请求 ----------------------------- */

  /**
   * 发起补齐请求。节流到 1 秒 1 次（trailing 合并），
   * 避免与服务端 2 次/秒的限流正面冲突。
   */
  private requestResync(reason: 'gap' | 'hello' | 'manual'): void {
    this.waitingForResync = true
    this.setStatus('reconnecting')
    this.pendingResyncReason = reason
    this.resyncThrottle.request()
  }

  /** 真正发出 `resync.request`（由节流器在允许的时点调用）。 */
  private sendResyncNow(): void {
    if (this.disposed) return
    const reason = this.pendingResyncReason ?? 'gap'
    const sent = this.options.transport.send('resync.request', {
      fromSeq: this.seqState.appliedSeq + 1,
      epoch: this.seqState.epoch,
      reason,
    })
    if (!sent) {
      // transport 还不可用：下一次 onOpen/joined 会重试
      this.waitingForResync = false
      return
    }
    this.waitingForResync = true
  }

  /* ------------------------------- snapshot ------------------------------ */

  private handleSnapshot(payload: SnapshotPayload): void {
    if (!isSnapshotPayload(payload)) return

    const result = collectSnapshot<Shape>(this.assembly, {
      epoch: payload.epoch,
      seq: payload.seq,
      chunk: payload.chunk,
      total: payload.total,
      items: payload.shapes,
      complete: payload.complete,
    })

    if (result.status === 'mismatch') {
      this.assembly = createAssembly<Shape>()
      return
    }
    if (result.status === 'collecting') return

    this.commitSnapshot(result.items, result.seq, result.epoch)
  }

  /**
   * 快照落地：**只有这一条路径**。
   *
   * `resync.snapshot` 分块收齐后走这里；`snapshot_too_large` 时 REST 分页
   * 拉完也走这里——保证「收齐才替换 / epoch 处理 / pending 清理 / 兜底补齐」
   * 四件事只有一份实现，不会出现两套逻辑漂移（PM 明确要求）。
   */
  private commitSnapshot(items: readonly unknown[], seq: number, epoch: number): void {
    // 收齐才整体替换（绝不半替换，协议 §4.2）
    const outcome = applySnapshotSeq(this.seqState, seq, epoch)
    this.options.scene.replaceAll(items, seq, epoch)
    this.assembly = createAssembly<Shape>()
    this.waitingForResync = false

    if (outcome === 'epoch_changed') {
      this.appliedOpIds.clear()
      this.options.listeners.onToast('房间已重置，正在重新同步', 'info')
    }

    // 快照已包含的 pending 视为已入库，丢弃以避免重放叠加（协议 §6.4）
    this.dropPendingCoveredBySnapshot()

    this.persist()
    drainBuffer<PersistentBody>(this.seqState, (events) => {
      for (const event of events) this.applyRemoteBody(event.data)
    })
    this.enterLiveIfReady()
  }

  /**
   * 大房间兜底：走 `GET /api/rooms/{code}/snapshot` 分页拉取（协议 §1.5）。
   * 拉完交给 `commitSnapshot()`——与 WS 分块快照共用同一条落地路径。
   */
  private async loadSnapshotViaRest(): Promise<void> {
    const loader = this.options.loadSnapshot
    if (!loader) {
      this.options.listeners.onToast('当前连接无法分页加载，请刷新页面', 'warn')
      return
    }
    if (this.restSnapshotLoading) return
    this.restSnapshotLoading = true
    try {
      const snapshot = await loader()
      if (this.disposed) return
      // 分页结果已是完整集合（loader 内部按 shape.id 字典序翻到底），
      // 这里直接落库；形状是协议形态，由 ScenePort 负责转本地模型。
      this.commitSnapshot(snapshot.shapes, snapshot.seq, snapshot.epoch)
      this.options.listeners.onToast('已加载完整白板内容', 'ok')
    } catch {
      this.options.listeners.onToast('白板内容加载失败，请刷新重试', 'warn')
    } finally {
      this.restSnapshotLoading = false
    }
  }

  /** 快照里已有同 id 图形 → 对应 pending 视为已入库。 */
  private dropPendingCoveredBySnapshot(): void {
    const ids = new Set(this.options.scene.listShapeIds())
    const survivors: PendingOp[] = []
    for (const op of this.pending) {
      const covered = op.shapeIds.length > 0 && op.shapeIds.every((id) => ids.has(id))
      if (covered) {
        this.options.scene.confirmOp(op.opId, op.shapeIds)
        this.appliedOpIds.add(op.opId)
      } else {
        survivors.push(op)
      }
    }
    if (survivors.length !== this.pending.length) {
      this.pending = survivors
      this.emitStatus()
    }
  }

  /* ----------------------------- resync.events ---------------------------- */

  private handleResyncEvents(payload: ResyncEventsPayload): void {
    if (!Array.isArray(payload.events)) return
    const { fromSeq } = payload

    for (let i = 0; i < payload.events.length; i += 1) {
      const event = payload.events[i]
      if (!event) return
      // 服务端必须严格连续，否则视为 gap（协议 §4.3）
      if (event.seq !== fromSeq + i) {
        this.requestResync('gap')
        return
      }
      const body = eventToBody(event)
      const effects = receiveEvent<PersistentBody>(this.seqState, { seq: event.seq, data: body })
      for (const effect of effects) {
        if (effect.kind === 'apply') {
          for (const applied of effect.events) this.applyRemoteBody(applied.data)
        } else if (effect.kind === 'gap') {
          this.requestResync('gap')
          return
        } else if (effect.kind === 'reset') {
          this.requestResync('gap')
          return
        }
      }
    }

    this.persist()
    drainBuffer<PersistentBody>(this.seqState, (events) => {
      for (const event of events) this.applyRemoteBody(event.data)
    })

    if (payload.hasMore) {
      this.requestResync('gap')
      return
    }

    this.waitingForResync = false
    this.enterLiveIfReady()
  }

  /* ------------------------------ op.add 广播 ----------------------------- */

  /**
   * 处理带信封 `seq` 的持久消息（op.add / member.role 共享同一入口）。
   *
   * 序号入口只有一个，避免广播与补发走两条不同路径（PERMISSIONS §10）。
   */
  onPersistent(seq: number, payload: OpBroadcastPayload | MemberRolePayload): void {
    if (!Number.isFinite(seq) || seq <= 0) return
    const body = toPersistentBody(payload)
    if (!body) return

    const effects = receiveEvent<PersistentBody>(this.seqState, { seq, data: body })
    for (const effect of effects) {
      if (effect.kind === 'apply') {
        for (const applied of effect.events) this.applyRemoteBody(applied.data)
        this.persist()
        this.enterLiveIfReady()
      } else if (effect.kind === 'gap') {
        this.requestResync('gap')
      } else if (effect.kind === 'reset') {
        this.requestResync('gap')
      }
    }
  }

  /**
   * 应用一条远端体（广播或补发共用）。
   *
   * 自己的 op：不重复应用（已乐观上屏），只做标记。
   * 已应用过：直接丢（`appliedOpIds` 是零重复的最后一道闸）。
   */
  private applyRemoteBody(body: PersistentBody): void {
    if (body.kind === 'member.role') {
      this.options.scene.applyMemberRole(body.payload)
      return
    }
    const payload = body.payload
    const own = payload.by?.clientId === this.options.clientId
    if (own) {
      const existing = findPending(this.pending, payload.opId)
      if (existing) this.options.scene.confirmOp(payload.opId, existing.shapeIds)
      this.appliedOpIds.add(payload.opId)
      return
    }
    if (this.appliedOpIds.has(payload.opId)) return
    this.appliedOpIds.add(payload.opId)
    this.options.scene.applyRemoteOp(payload)
  }

  /* --------------------------------- ack --------------------------------- */

  private handleAck(payload: AckPayload): void {
    if (!payload || typeof payload.opId !== 'string') return

    if (payload.status === 'applied' || payload.status === 'duplicate') {
      this.options.scene.confirmOp(payload.opId, findPending(this.pending, payload.opId)?.shapeIds ?? [])
      this.pending = dequeuePending(this.pending, payload.opId)
      this.appliedOpIds.add(payload.opId)
      this.persist()
      this.emitStatus()
      return
    }

    // rejected：回滚乐观渲染并提示，禁止静默丢弃（协议 §4.5）
    const failed = findPending(this.pending, payload.opId)
    this.pending = dequeuePending(this.pending, payload.opId)
    if (failed) this.options.scene.rollbackOp(failed.opId, failed.shapeIds)

    const code = payload.code ?? 'invalid_op'
    this.options.listeners.onError(code, payload.message ?? '操作被拒绝', false)
    if (code === 'forbidden') {
      // 被撤销授权 / 只读身份继续画：连接不断，本地回滚 + 明确提示
      this.options.listeners.onToast('只读模式 · 房主可授予你编辑权限', 'warn')
    } else {
      this.options.listeners.onToast(payload.message ?? '操作被拒绝，已撤销', 'warn')
    }
    this.persist()
    this.emitStatus()
  }

  /* ------------------------------- presence ------------------------------ */

  private handlePresence(payload: PresencePatchPayload): void {
    if (!Array.isArray(payload.users)) return
    this.options.listeners.onMembers(payload.users)
  }

  /* ------------------------------ 房主操作 ------------------------------ */

  /**
   * 房主授权/撤销成员（PERMISSIONS §3.1）。
   *
   * **只发不收**：不做本地乐观更新，改状态一律等 `member.role` 回来——
   * 否则一旦服务端拒绝（越权/目标离线），UI 会短暂显示一个假的角色。
   */
  grantRole(targetClientId: string, role: 'editor' | 'viewer'): boolean {
    if (!targetClientId) return false
    const sent = this.options.transport.send('room.grant', { targetClientId, role })
    if (!sent) this.options.listeners.onToast('连接不稳定，请稍后重试', 'warn')
    return sent
  }

  /** 房主手动移交（PERMISSIONS §2.3）。成功后原房主降为 editor。 */
  transferOwner(targetClientId: string): boolean {
    if (!targetClientId) return false
    const sent = this.options.transport.send('room.transfer', { targetClientId })
    if (!sent) this.options.listeners.onToast('连接不稳定，请稍后重试', 'warn')
    return sent
  }

  /* -------------------------------- 写操作 ------------------------------- */

  /**
   * 提交一条本地 op：**始终先乐观渲染**，然后
   *  - online → 立即发网络
   *  - 否则 → 只入 pending（协议 §6.2：只有 live 才发 op.add）
   */
  submitOp(payload: OpAddPayload, shapeIds: readonly string[]): void {
    if (this.appliedOpIds.has(payload.opId)) return

    this.options.scene.applyLocalOp(payload)
    const op: PendingOp = {
      opId: payload.opId,
      payload,
      createdAt: this.now(),
      shapeIds: [...shapeIds],
    }

    const result = enqueuePending(this.pending, op)
    if (result.overflow) {
      this.options.listeners.onToast('离线改动过多，最早的改动可能丢失', 'warn')
    }
    this.pending = result.queue
    this.persist()
    this.emitStatus()

    if (this.status === 'online') {
      this.options.transport.send('op.add', op.payload)
    }
  }

  /* ------------------------------ 进入 online ----------------------------- */

  private enterLiveIfReady(): void {
    if (this.disposed) return
    // 传输层没通就不能自称 online（否则状态胶囊会对用户撒谎）
    if (!this.transportUp) {
      if (this.status === 'online') this.setStatus('reconnecting')
      return
    }
    if (this.status === 'online') return
    if (!this.joinedPayload) return
    if (this.waitingForResync) return
    if (this.seqState.buffer.size > 0) return

    this.setStatus('online')
    // 进入 live 后立刻重放 pending（复用原 opId，协议 §6.4）
    for (const op of replayOrder(this.pending)) {
      this.options.transport.send('op.add', op.payload)
    }
  }

  /* --------------------------------- 光标 -------------------------------- */

  /** 上报光标，节流到 ≤20Hz（协议 §7）。 */
  sendCursor(x: number, y: number): void {
    if (this.status !== 'online') return
    const now = this.now()
    if (now - this.cursorLastSent < 50) return
    this.cursorLastSent = now
    this.options.transport.send('presence.cursor', { x, y })
  }

  /* -------------------------------- 测试钩子 ------------------------------ */

  debugState(): {
    status: ConnectionStatus
    appliedSeq: number
    epoch: number
    pending: number
    buffer: number
  } {
    return {
      status: this.status,
      appliedSeq: this.seqState.appliedSeq,
      epoch: this.seqState.epoch,
      pending: this.pending.length,
      buffer: this.seqState.buffer.size,
    }
  }

  snapshotPending(): readonly PendingOp[] {
    return [...this.pending]
  }

  hydrateForTest(appliedSeq: number, epoch: number, pending: readonly PendingOp[]): void {
    this.seqState.appliedSeq = appliedSeq
    this.seqState.epoch = epoch
    this.pending = [...pending]
  }

  isAssemblyStale(now = Date.now()): boolean {
    return isAssemblyStale(this.assembly, now)
  }

  restFallbackNeeded(now = Date.now()): boolean {
    return isAssemblyStale(this.assembly, now)
  }
}

/* -------------------------------- 纯函数助手 ------------------------------- */

function isJoinedPayload(payload: unknown): payload is JoinedPayload {
  if (typeof payload !== 'object' || payload === null) return false
  const value = payload as Partial<JoinedPayload>
  if (typeof value.you !== 'object' || value.you === null) return false
  if (typeof value.room !== 'object' || value.room === null) return false
  return (
    typeof value.you.role === 'string' &&
    typeof value.you.color === 'string' &&
    typeof value.room.seq === 'number' &&
    typeof value.room.epoch === 'number'
  )
}

function isSnapshotPayload(payload: unknown): payload is SnapshotPayload {
  if (typeof payload !== 'object' || payload === null) return false
  const value = payload as Partial<SnapshotPayload>
  return (
    typeof value.epoch === 'number' &&
    typeof value.seq === 'number' &&
    typeof value.chunk === 'number' &&
    typeof value.total === 'number' &&
    Array.isArray(value.shapes)
  )
}

/**
 * 持久消息 payload → 内部可辨识联合（`member.role` 走同一条 seq 通道）。
 * 结构不合法时返回 null（脏帧不崩、不推进 seq）。
 */
function toPersistentBody(payload: OpBroadcastPayload | MemberRolePayload): PersistentBody | null {
  if (!payload || typeof payload !== 'object') return null
  if (typeof (payload as MemberRolePayload).clientId === 'string') {
    const role = (payload as MemberRolePayload).role
    if (role === 'owner' || role === 'editor' || role === 'viewer') {
      return { kind: 'member.role', payload: payload as MemberRolePayload }
    }
    return null
  }
  if (typeof (payload as OpBroadcastPayload).opId !== 'string') return null
  return { kind: 'op', payload: payload as OpBroadcastPayload }
}

/** resync.events 的元素 → 持久消息体（op 与 member.role 共用补齐通道）。 */
export function eventToBody(event: ResyncEvent): PersistentBody {
  if (event.type === 'member.role') {
    return {
      kind: 'member.role',
      payload: {
        clientId: event.clientId,
        role: event.role,
        ...(event.grantedBy ? { grantedBy: event.grantedBy } : {}),
      },
    }
  }
  return {
    kind: 'op',
    payload: {
      opId: event.opId,
      action: event.action,
      ...(event.shape ? { shape: event.shape } : {}),
      ...(event.id ? { id: event.id } : {}),
      ...(event.points ? { points: event.points } : {}),
      ...(event.by ? { by: event.by } : {}),
    },
  }
}

export type { SeqEvent }
