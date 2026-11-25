/**
 * 协议类型定义。**唯一依据：`docs/PROTOCOL.md`（v1，冻结）。**
 *
 * 信封：`{ v, type, seq?, roomId, clientId, ts, payload }`
 *  - 只有持久消息（`op.add`、`room.meta`）带信封级 `seq`；
 *  - `ack` 的 seq 在 `payload.seq` 里，信封层没有；
 *  - `payload` 永不为 null。
 *
 * 全部字段 readonly：服务端权威数据不允许被上层就地改写。
 */
import type { Shape } from '@/canvas/shapes'

export const PROTOCOL_VERSION = 1 as const

/* ------------------------------- 基础约束值 ------------------------------- */

export const MAX_MESSAGE_BYTES = 65536
export const MAX_SHAPE_BYTES = 32768
export const MAX_SHAPES_PER_ROOM = 20000
export const MAX_APPEND_POINTS = 512
export const MAX_TEXT_LENGTH = 2000

/** 与 PROTOCOL §7 对齐的默认限额；以 `joined.limits` 的运行时值为准。 */
export const DEFAULT_LIMITS: RoomLimits = {
  maxMessageBytes: MAX_MESSAGE_BYTES,
  maxShapeBytes: MAX_SHAPE_BYTES,
  maxShapesPerRoom: MAX_SHAPES_PER_ROOM,
  opsPerSecond: 60,
  opsBurst: 120,
  cursorPerSecond: 20,
  resumeMaxOps: 2000,
  resumeMaxBytes: 4194304,
}

/* --------------------------------- 身份 --------------------------------- */

/**
 * 房间角色（PERMISSIONS §1）。
 * `owner` 隐含 `editor` 的全部能力；判断"能不能画"一律用 `owner || editor`。
 */
export type Role = 'owner' | 'editor' | 'viewer'

/** 是否拥有写权限（owner 隐含 editor 能力，PERMISSIONS §1 / §5）。 */
export function canRoleEdit(role: Role): boolean {
  return role === 'owner' || role === 'editor'
}

/**
 * 对外连接状态。UI 只依赖四态（PROTOCOL §6.2 的内部 `syncing` 已折叠进 `reconnecting`，
 * 因为用户在同步期看到的文案与重连期一致；待同步条数由 `pending` 单独表达）。
 */
export type ConnectionStatus = 'connecting' | 'online' | 'reconnecting' | 'offline'

/** PROTOCOL §4.1 `joined.payload.you` / §4.4 `by` / §4.6 `user` 的统一形状。 */
export interface PresenceUser {
  readonly clientId: string
  readonly userId: string
  readonly name: string
  readonly color: string
  readonly role: Role
}

/** `joined.payload.you`：在 presence 形状上补一个便捷布尔（PERMISSIONS §4.3）。 */
export interface JoinedUser extends PresenceUser {
  /** 等价于 `role === 'owner'`（契约要求必填；运行时缺失时前端按 role 兜底） */
  readonly isOwner: boolean
}

export interface RoomLimits {
  readonly maxMessageBytes: number
  readonly maxShapeBytes: number
  readonly maxShapesPerRoom: number
  readonly opsPerSecond: number
  readonly opsBurst: number
  readonly cursorPerSecond: number
  readonly resumeMaxOps: number
  readonly resumeMaxBytes: number
}

/* -------------------------------- 信封 ---------------------------------- */

export interface Envelope<P = unknown> {
  readonly v: typeof PROTOCOL_VERSION
  readonly type: string
  readonly seq?: number
  readonly roomId: string
  readonly clientId: string
  readonly ts: number
  readonly payload: P
}

/* ------------------------------ 客户端 → 服务端 ----------------------------- */

/** §3.1 `hello`：升级后客户端立即发送，不等 `joined`。 */
export interface HelloPayload {
  readonly lastSeq: number
  readonly epoch: number
  readonly clientVersion?: string
  readonly viewport?: { readonly w: number; readonly h: number; readonly dpr: number }
}

export type OpAction = 'create' | 'update' | 'delete' | 'append'

/** §3.2 `op.add` 的 payload。`shape`/`id`/`points` 按 action 二选一必填。 */
export interface OpAddPayload {
  readonly opId: string
  readonly action: OpAction
  readonly shape?: Shape
  readonly id?: string
  readonly points?: ReadonlyArray<readonly number[]>
  readonly rev?: number
}

export interface CursorPayload {
  readonly x: number
  readonly y: number
}

export interface ResyncRequestPayload {
  readonly fromSeq: number
  readonly epoch?: number
  readonly reason?: 'gap' | 'hello' | 'manual'
}

export interface PingPayload {
  readonly ts: number
}

/** 客户端允许发送的全部 type（PROTOCOL §3 + PERMISSIONS §3.1 / §2.3）。 */
export const CLIENT_MESSAGE_TYPES = [
  'hello',
  'op.add',
  'presence.cursor',
  'resync.request',
  'ping',
  'bye',
  'room.grant',
  'room.transfer',
] as const

export type ClientMessageType = (typeof CLIENT_MESSAGE_TYPES)[number]

/** `room.grant`：房主给成员授权（PERMISSIONS §3.1）。只允许 editor/viewer。 */
export interface RoomGrantPayload {
  readonly targetClientId: string
  readonly role: 'editor' | 'viewer'
}

/** `room.transfer`：房主手动移交（PERMISSIONS §2.3）。目标必须是当前在线成员。 */
export interface RoomTransferPayload {
  readonly targetClientId: string
}

/* ------------------------------ 服务端 → 客户端 ----------------------------- */

/** §4.1 `joined`：连接建立后服务端主动发的第一条业务消息，**不含图形**。 */
export interface JoinedPayload {
  readonly you: JoinedUser
  readonly room: {
    readonly code: string
    readonly title: string
    readonly epoch: number
    readonly seq: number
  }
  readonly online: number
  readonly instanceId: string
  readonly limits: RoomLimits
  readonly serverTime?: number
  /** 当前房主；`null` = 无主房间（PERMISSIONS §4.3 / §6.3） */
  readonly ownerClientId?: string | null
  /** 服务端签发的会话 token；重连时作为 `?st=` 带回 */
  readonly sessionToken?: string
}

/** §4.2 `resync.snapshot`：分块快照，收齐才替换视图。 */
export interface SnapshotPayload {
  readonly epoch: number
  readonly seq: number
  readonly chunk: number
  readonly total: number
  readonly shapes: readonly Shape[]
  readonly complete: boolean
}

/**
 * §4.3 `resync.events` 的元素。
 *
 * 主体是 op.add 的 payload + seq + by；但 `member.role` 也带 seq、也走这条补齐通道
 * （PERMISSIONS §3.1 / §10），所以这里是一个可辨识联合。
 */
export interface ResyncOpEvent {
  readonly seq: number
  /** 服务端可能显式标注类型；op 事件缺省即可 */
  readonly type?: 'op.add'
  readonly opId: string
  readonly action: OpAction
  readonly shape?: Shape
  readonly id?: string
  readonly points?: ReadonlyArray<readonly number[]>
  readonly by?: PresenceUser
  readonly ts?: number
}

/** 补齐通道里的成员角色变更（PERMISSIONS §3.1）。 */
export interface ResyncMemberRoleEvent {
  readonly seq: number
  readonly type: 'member.role'
  readonly clientId: string
  readonly role: Role
  readonly grantedBy?: string
}

export type ResyncEvent = ResyncOpEvent | ResyncMemberRoleEvent

export interface ResyncEventsPayload {
  readonly epoch: number
  readonly fromSeq: number
  readonly toSeq: number
  readonly events: readonly ResyncEvent[]
  readonly hasMore: boolean
}

/** §4.4 `op.add` 广播 payload（与 op.add 上行 payload 同形 + `by`）。 */
export interface OpBroadcastPayload extends OpAddPayload {
  readonly by?: PresenceUser
}

/** §4.5 `ack`：seq 在 payload 里。 */
export type AckStatus = 'applied' | 'duplicate' | 'rejected'

export interface AckPayload {
  readonly opId: string
  readonly status: AckStatus
  readonly seq?: number
  readonly code?: string
  readonly message?: string
}

/** §4.6 `presence.join` / `presence.leave`。`users` 是全量列表，直接替换。 */
export interface PresencePatchPayload {
  readonly user?: PresenceUser
  readonly users: readonly PresenceUser[]
  readonly online: number
}

/** §4.7 `presence.cursor`。 */
export interface CursorBroadcastPayload {
  readonly x: number
  readonly y: number
  readonly from: PresenceUser
}

/** §4.8 `room.meta`。 */
export interface RoomMetaPayload {
  readonly title?: string
  readonly readonlyRevokedAt?: number
}

/** §4.9 `error`。 */
export interface ErrorPayload {
  readonly code: string
  readonly message: string
  readonly fatal?: boolean
  readonly opId?: string
  readonly retryAfterMs?: number
}

/**
 * `member.role`：成员角色变更广播（PERMISSIONS §3.1）。
 *
 * **带信封级 `seq`**：它改变房间可观察状态（谁有权写），必须走断线补齐通道，
 * 不能新开一条无 seq 的广播路径（PERMISSIONS §10「共同」）。
 */
export interface MemberRolePayload {
  readonly clientId: string
  readonly role: Role
  /** 发起授权的房主 cid；服务端主动移交时可能缺省 */
  readonly grantedBy?: string
}

/** §4.10 `pong`。 */
export interface PongPayload {
  readonly ts: number
  readonly serverTime?: number
}

/** 服务端全部 type（PROTOCOL §4 表 + PERMISSIONS §3.1 `member.role`）。 */
export const SERVER_MESSAGE_TYPES = [
  'joined',
  'presence.join',
  'presence.leave',
  'presence.cursor',
  'op.add',
  'ack',
  'resync.snapshot',
  'resync.events',
  'room.meta',
  'member.role',
  'error',
  'pong',
] as const

export type ServerMessageType = (typeof SERVER_MESSAGE_TYPES)[number]

/** 强类型化的服务端消息（按 type 收窄 payload）。 */
export type ServerMessage =
  | (Envelope<JoinedPayload> & { readonly type: 'joined' })
  | (Envelope<PresencePatchPayload> & { readonly type: 'presence.join' | 'presence.leave' })
  | (Envelope<CursorBroadcastPayload> & { readonly type: 'presence.cursor' })
  | (Envelope<OpBroadcastPayload> & { readonly type: 'op.add' })
  | (Envelope<AckPayload> & { readonly type: 'ack' })
  | (Envelope<SnapshotPayload> & { readonly type: 'resync.snapshot' })
  | (Envelope<ResyncEventsPayload> & { readonly type: 'resync.events' })
  | (Envelope<RoomMetaPayload> & { readonly type: 'room.meta' })
  | (Envelope<MemberRolePayload> & { readonly type: 'member.role' })
  | (Envelope<ErrorPayload> & { readonly type: 'error' })
  | (Envelope<PongPayload> & { readonly type: 'pong' })

/* --------------------------------- 连接接口 -------------------------------- */

export interface RoomConnectionHandlers {
  /** 状态变化；`pending` 为待确认 op 数 */
  onStatus(status: ConnectionStatus, pending: number): void
  onJoined(message: Envelope<JoinedPayload>): void
  onPresence(
    message: Envelope<PresencePatchPayload> & { readonly type: 'presence.join' | 'presence.leave' },
  ): void
  onCursor(message: Envelope<CursorBroadcastPayload>): void
  onOp(message: Envelope<OpBroadcastPayload>): void
  onAck(message: Envelope<AckPayload>): void
  onSnapshot(message: Envelope<SnapshotPayload>): void
  onEvents(message: Envelope<ResyncEventsPayload>): void
  onError(message: Envelope<ErrorPayload>): void
  /** 未知 / 不可解析的帧；不得抛异常打断读循环 */
  onUnknown(type: string): void
}

/* ------------------------------- 错误码文案 ------------------------------- */

/** 错误码 → 用户可读文案（PROTOCOL §8）。未知 code 走兜底。 */
export const ERROR_TEXT: Readonly<Record<string, string>> = {
  bad_request: '操作格式不正确，已忽略',
  unsupported_version: '页面版本过旧，请刷新',
  unsupported_type: '忽略了不支持的消息',
  unauthenticated: '身份信息失效，请重新进入房间',
  forbidden: '没有权限：只读成员无法编辑，房主可授予权限',
  target_offline: '对方已经离开房间',
  invalid_grant: '授权参数无效（房主转移请用「转移房主」）',
  invalid_op: '操作无效，已撤销',
  rate_limited: '操作太快了，稍等一秒',
  message_too_large: '这一笔太长，已自动分段',
  shape_not_found: '图形已不存在，正在重新同步',
  shape_deleted: '图形已被删除',
  shape_too_large: '图形超出大小限制',
  duplicate_shape: '图形重复，已重新生成',
  room_full_shapes: '房间图形已达上限，请稍后再试',
  room_not_found: '房间不存在或已过期',
  room_full: '房间人数已满',
  server_busy: '服务器繁忙，请稍后再试',
  snapshot_too_large: '房间内容过多，正在改用分页加载',
  internal: '服务器开小差了，请重试',
}

export function errorText(code: string): string {
  return ERROR_TEXT[code] ?? '网络异常，请稍后再试'
}

/** 握手阶段的 HTTP 错误码（PROTOCOL §2.1），决定是否该自动重连。 */
export const FATAL_HANDSHAKE_CODES: ReadonlySet<string> = new Set([
  'room_not_found',
  'room_full',
  'forbidden',
  'unsupported_version',
])

/** 是否应该自动重连：房间满/不存在这类错误重试无意义。 */
export function isRetryableError(code: string): boolean {
  return !FATAL_HANDSHAKE_CODES.has(code)
}
