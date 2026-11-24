/**
 * 房间会话：把 store、WsClient、RealtimeClient、IndexedDB 组装成一个可用的连接。
 *
 * 这是唯一「知道全部依赖」的地方，RoomView 只调 `connectRoom()` / `dispose()`。
 * 所有网络副作用集中在此文件，便于审计（不需要在 5 个文件里找 fetch）。
 */
import { WsClient } from '@/net/ws'
import { RealtimeClient, type Persistence, type Transport } from '@/net/realtime'
import { ApiError, fetchFullSnapshot, fetchRoom } from '@/net/rest'
import { createScenePort, type MemberRoleStore, type SceneStore } from '@/stores/scenePort'
import {
  createThrottledSaver,
  loadRoomState,
  type PersistedOp,
  type PersistedRoomState,
} from '@/net/persistence'
import type { ConnectionStatus, PresenceUser } from '@/net/types'
import { pushToast } from '@/components/ui'
import type { useBoardStore } from '@/stores/board'

type BoardStore = ReturnType<typeof useBoardStore>

export interface RoomSessionOptions {
  store: BoardStore
  roomCode: string
  /** 编辑密钥（持有则 editor）；只读 token 走 `rt` */
  editorKey?: string
  /** 只读 token（`?rt=`）；存在时强制 viewer */
  readonlyToken?: string
  displayName: string
  /** 房间不存在时跳转 404 */
  onRoomNotFound: () => void
}

export interface RoomSession {
  realtime: RealtimeClient
  ws: WsClient
  dispose: () => void
  /** 页面回到前台时调用 */
  checkAlive: () => void
  /** 上报光标（世界坐标） */
  sendCursor: (x: number, y: number) => void
}

/**
 * 建立房间会话。幂等：重复调用会先销毁旧连接。
 */
export async function createRoomSession(options: RoomSessionOptions): Promise<RoomSession> {
  const { store, roomCode } = options
  const cid = store.cid
  const normalizedCode = roomCode.toUpperCase()

  // ---------- 1. 持久化 ----------
  const saver = createThrottledSaver(400)
  const persistence: Persistence = {
    load: () => loadRoomState(normalizedCode, cid),
    save: (state: { appliedSeq: number; epoch: number; pending: readonly PersistedOp[] }) => {
      saver.schedule(() => ({
        roomId: normalizedCode,
        clientId: cid,
        appliedSeq: state.appliedSeq,
        epoch: state.epoch,
        pending: state.pending.map((op) => ({
          opId: op.opId,
          payload: op.payload,
          createdAt: op.createdAt,
          shapeIds: op.shapeIds,
        })),
      }))
    },
  }

  // ---------- 2. WS + 协调器 ----------
  // transport 是个薄转发层：它把「发送」推迟到调用时解析 ws，
  // 因此可以在 ws 构造完成后再真正建连（避免构造期回调先于赋值）。
  let ws: WsClient | null = null
  const requireWs = (): WsClient => {
    if (!ws) throw new Error('WS 尚未初始化')
    return ws
  }

  const transport: Transport = {
    connect: () => requireWs().connect(),
    disconnect: () => ws?.disconnect(),
    send: (type, payload, allowBeforeJoined) => requireWs().send(type, payload, allowBeforeJoined),
    sendEnvelope: (envelope) => requireWs().sendEnvelope(envelope),
    checkAlive: () => requireWs().checkAlive(),
  }

  const scene = createScenePort({
    get shapes() {
      return store.shapes
    },
    get cid() {
      return store.cid
    },
    get role() {
      return store.role
    },
    get ownerClientId() {
      return store.ownerClientId
    },
    get members() {
      return store.members
    },
    addShape: (shape) => store.addShape(shape),
    addShapes: (shapes) => store.addShapes(shapes),
    upsertShape: (shape) => store.upsertShape(shape),
    updateShape: (shape) => store.updateShape(shape),
    removeShapes: (ids) => store.removeShapes(ids),
    markDeleted: (id) => store.markDeleted(id),
    setPending: (id, pending) => store.setPending(id, pending),
    setShapes: (shapes) => store.setShapes(shapes),
    findShape: (id) => store.findShape(id),
    touch: () => store.touch(),
    setRole: (role) => store.setRole(role),
    setOwnerClientId: (ownerClientId) => store.setOwnerClientId(ownerClientId),
    setMemberRole: (clientId, role) => store.setMemberRole(clientId, role),
    notify: (message, tone) => pushToast(message, tone),
  } satisfies SceneStore & MemberRoleStore)

  const realtime = new RealtimeClient({
    roomId: normalizedCode,
    clientId: cid,
    displayName: options.displayName,
    ...(options.editorKey ? { key: options.editorKey } : {}),
    ...(options.readonlyToken ? { rt: options.readonlyToken } : {}),
    transport,
    scene,
    persistence,
    // 连续 3 次重连失败后探测房间是否还存在（服务端重启会丢内存房间）
    probeRoom: async () => {
      try {
        await fetchRoom(normalizedCode, options.editorKey)
        return true
      } catch (error) {
        if (error instanceof ApiError && error.code === 'room_not_found') return false
        // 其他错误（网络仍未恢复/5xx）：不好判断，按「还存在」处理，继续重连
        return true
      }
    },
    // 大房间兜底：`snapshot_too_large` 时用 REST 分页翻完全部图形。
    // 复用 REST 客户端里已有的分页实现（fetchFullSnapshot）。
    loadSnapshot: async () => {
      const key = options.readonlyToken ?? options.editorKey ?? ''
      const full = await fetchFullSnapshot(normalizedCode, key)
      return { shapes: full.shapes, seq: full.seq, epoch: full.epoch }
    },
    listeners: {
      onStatus(status, pending) {
        store.setStatus(status, pending)
        store.setAppliedSeq(realtime.appliedSeq)
      },
      onJoined(payload) {
        store.applyJoined({
          you: payload.you,
          room: payload.room,
          online: payload.online,
          limits: payload.limits,
          ownerClientId: payload.ownerClientId ?? null,
        })
        store.setAppliedSeq(realtime.appliedSeq)
        // 首次加入后立刻把成员列表填上（joined 不带 users，先放自己）
        if (store.members.length === 0) {
          store.setMembers([payload.you])
        }
      },
      onMembers(users: readonly PresenceUser[]) {
        store.setMembers(users)
      },
      onCursor(cursor) {
        cursorBus.emit(cursor)
      },
      onToast(message, tone) {
        pushToast(message, tone)
      },
      onError(_code, message, fatal) {
        if (fatal) pushToast(message, 'warn', 4000)
      },
      onRoomNotFound() {
        options.onRoomNotFound()
      },
      onUnknownFrame(retryable, type) {
        if (import.meta.env.DEV) {
          // 只在开发环境留痕：生产环境静默（协议要求不报错不断线）
          void type
          void retryable
        }
      },
    },
  })

  ws = new WsClient(
    {
      roomId: normalizedCode,
      clientId: cid,
      name: options.displayName,
      ...(options.editorKey ? { key: options.editorKey } : {}),
      ...(options.readonlyToken ? { rt: options.readonlyToken } : {}),
    },
    {
      onOpen: () => realtime.onOpen(),
      onMessage: (message, envelopeSeq) => realtime.onMessage(message, envelopeSeq),
      onUnknownFrame: (reason, type) => realtime.onUnknownFrame(reason, type),
      onStatus: (status: ConnectionStatus) => {
        // 传输层状态优先：socket 断了就必须显示「重连中 / 离线」，
        // 即使 realtime 层还记得自己曾经 online（否则 UI 会对用户撒谎）。
        // realtime 层负责在同步完成后把状态推回 online（见 onJoined / enterLiveIfReady）。
        if (status === 'offline' || status === 'reconnecting' || status === 'connecting') {
          store.setStatus(status, realtime.pendingCount)
        }
      },
      onServerError: (code, message, fatal) => realtime.handleError(code, message, fatal),
      onClose: (code, reason, willRetry) => realtime.onClose(code, reason, willRetry),
    },
  )

  // 把 pending 数量与 appliedSeq 同步到 UI。
  // realtime 内部状态更新是同步的，但 store 是响应式镜像，需要补一次；
  // 用 250ms 轮询足够（数字变化对用户是「瞬时」感知，且不在渲染热路径上）。
  const pendingTicker = window.setInterval(() => {
    const nextPending = realtime.pendingCount
    const nextSeq = realtime.appliedSeq
    if (nextPending !== store.pendingCount) store.setStatus(store.status, nextPending)
    if (nextSeq !== store.appliedSeq) store.setAppliedSeq(nextSeq)
  }, 250)

  const handleVisibility = (): void => {
    if (document.visibilityState === 'visible') requireWs().checkAlive()
  }
  document.addEventListener('visibilitychange', handleVisibility)

  const onOnline = (): void => requireWs().checkAlive()
  const onOffline = (): void => store.setStatus('offline', realtime.pendingCount)
  window.addEventListener('online', onOnline)
  window.addEventListener('offline', onOffline)

  await realtime.start()

  // 开发环境暴露会话，便于联调/排障（生产构建里这段被 tree-shake 掉）
  if (import.meta.env.DEV && typeof window !== 'undefined') {
    const dbg = window as unknown as { __codraw?: { session: RoomSession | null } }
    const active = requireWs()
    dbg.__codraw = { session: null }
    dbg.__codraw.session = {
      realtime,
      ws: active,
      dispose() {
        window.clearInterval(pendingTicker)
        document.removeEventListener('visibilitychange', handleVisibility)
        window.removeEventListener('online', onOnline)
        window.removeEventListener('offline', onOffline)
        realtime.stop()
        void saver.flush()
        saver.dispose()
      },
      checkAlive: () => active.checkAlive(),
      sendCursor: (x, y) => realtime.sendCursor(x, y),
    }
  }

  const sessionWs = requireWs()
  return {
    realtime,
    ws: sessionWs,
    dispose() {
      window.clearInterval(pendingTicker)
      document.removeEventListener('visibilitychange', handleVisibility)
      window.removeEventListener('online', onOnline)
      window.removeEventListener('offline', onOffline)
      realtime.stop()
      void saver.flush()
      saver.dispose()
    },
    checkAlive() {
      sessionWs.checkAlive()
    },
    sendCursor(x, y) {
      realtime.sendCursor(x, y)
    },
  }
}

/** 调试/集成测试出口：把当前房间会话挂到 window（仅开发环境）。 */
if (import.meta.env.DEV && typeof window !== 'undefined') {
  interface DebugWindow extends Window {
    __codraw?: { session: RoomSession | null }
  }
  const dbg = window as DebugWindow
  dbg.__codraw = dbg.__codraw ?? { session: null }
}

/* ------------------------------ 光标事件总线 ------------------------------ */

type CursorHandler = (cursor: { x: number; y: number; from: PresenceUser }) => void

class CursorBus {
  private handlers = new Set<CursorHandler>()

  on(handler: CursorHandler): () => void {
    this.handlers.add(handler)
    return () => this.handlers.delete(handler)
  }

  emit(cursor: { x: number; y: number; from: PresenceUser }): void {
    for (const handler of this.handlers) handler(cursor)
  }
}

/** 光标不入 store（20Hz 更新会引发全应用重渲染），走独立总线给渲染层。 */
export const cursorBus = new CursorBus()

/** 便于测试与调试：读取本地持久化快照。 */
export async function readPersistedState(roomCode: string, cid: string): Promise<PersistedRoomState | null> {
  return loadRoomState(roomCode.toUpperCase(), cid)
}
