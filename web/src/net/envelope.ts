/**
 * 信封：出站消息的唯一构造入口 + 入站消息的白名单校验。
 *
 * **业务代码禁止手写 `JSON.stringify({v, type, ...})`** —— 这是字段漂移的
 * 唯一来源（docs/CONTRACT-ALIGNMENT.md §3.2）。
 *
 * 契约：docs/PROTOCOL.md §2.2 / §2.3。
 */
import {
  CLIENT_MESSAGE_TYPES,
  PROTOCOL_VERSION,
  SERVER_MESSAGE_TYPES,
  type ClientMessageType,
  type Envelope,
  type ServerMessage,
  type ServerMessageType,
} from './types'

/* --------------------------------- 出站 ---------------------------------- */

export interface EnvelopeContext {
  roomId: string
  clientId: string
  /** 时钟注入，便于测试与诊断；默认 `Date.now` */
  now?: () => number
  /** 协议版本注入（默认 1），测试极端版本时用 */
  version?: number
}

/**
 * 构造一个完整信封。**只调用这一个函数**，不要在各处拼对象。
 * `payload` 为 undefined 时补 `{}`（协议要求 payload 永不为 null）。
 */
export function buildEnvelope<T>(ctx: EnvelopeContext, type: ClientMessageType, payload: T): Envelope<T> {
  if (!ctx.roomId) throw new Error('信封缺少 roomId')
  if (!ctx.clientId) throw new Error('信封缺少 clientId')
  return {
    v: (ctx.version ?? PROTOCOL_VERSION) as typeof PROTOCOL_VERSION,
    type,
    roomId: ctx.roomId,
    clientId: ctx.clientId,
    ts: (ctx.now ?? Date.now)(),
    payload: (payload ?? {}) as T,
  }
}

/** 序列化为 WS 文本帧，并做大包自检（协议上限 64KB）。 */
export function serializeEnvelope(envelope: Envelope<unknown>, maxBytes = 65536): string {
  const text = JSON.stringify(envelope)
  // 用 TextEncoder 精确按 UTF-8 字节计（中文一字 3 字节，length 会低估）
  const bytes = new TextEncoder().encode(text).length
  if (bytes > maxBytes) {
    throw new Error(`消息超出 ${maxBytes} 字节限制（实际 ${bytes}）`)
  }
  return text
}

/* --------------------------------- 入站 ---------------------------------- */

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value)

const SERVER_TYPE_SET: ReadonlySet<string> = new Set(SERVER_MESSAGE_TYPES)
export const CLIENT_TYPE_SET: ReadonlySet<string> = new Set(CLIENT_MESSAGE_TYPES)

/** 便于其它模块统一引用信封类型。 */
export type { Envelope }

export interface ParsedFrame {
  ok: true
  message: ServerMessage
}

export interface RejectedFrame {
  ok: false
  /** `unknown_type` | `malformed` | `bad_version` */
  reason: 'unknown_type' | 'malformed' | 'bad_version'
  type: string
}

/**
 * 解析入站帧。
 *
 * 规则（PROTOCOL §2.3）：
 *  - 未知 `type` → 忽略（`reason:'unknown_type'`），**不报错、不断线**；
 *  - 缺字段 / 类型错 / payload 非对象 → 丢弃该条；
 *  - `v` 不是 1 → 标记 `bad_version`（由上层决定提示刷新，但不在此抛异常）。
 *
 * 只做结构校验，payload 内部字段由各 handler 自行收窄（避免几百行守卫）。
 */
export function parseFrame(raw: string, supportedVersion = PROTOCOL_VERSION): ParsedFrame | RejectedFrame {
  let data: unknown
  try {
    data = JSON.parse(raw)
  } catch {
    return { ok: false, reason: 'malformed', type: '' }
  }

  if (!isRecord(data)) return { ok: false, reason: 'malformed', type: '' }

  const type = typeof data.type === 'string' ? data.type : ''
  if (!type) return { ok: false, reason: 'malformed', type: '' }

  // 版本先判：老/新版本的消息类型可能不同，先给出明确原因
  const v = typeof data.v === 'number' ? data.v : PROTOCOL_VERSION
  if (v !== supportedVersion) return { ok: false, reason: 'bad_version', type }

  if (!SERVER_TYPE_SET.has(type)) return { ok: false, reason: 'unknown_type', type }
  if (!isRecord(data.payload)) return { ok: false, reason: 'malformed', type }
  if (typeof data.roomId !== 'string' || !data.roomId) {
    return { ok: false, reason: 'malformed', type }
  }

  return { ok: true, message: data as unknown as ServerMessage }
}

/** 判断某个 type 是否是服务端合法消息（白名单）。 */
export function isServerType(type: string): type is ServerMessageType {
  return SERVER_TYPE_SET.has(type)
}

/* ------------------------------ payload 取值助手 ----------------------------- */

/**
 * 从任意 payload 安全取值。handler 里密集使用时，
 * 集中在这里做类型收窄，避免到处写 `as`。
 */
export const read = {
  str(source: unknown, key: string, fallback = ''): string {
    if (!isRecord(source)) return fallback
    const value = source[key]
    return typeof value === 'string' ? value : fallback
  },
  num(source: unknown, key: string, fallback = 0): number {
    if (!isRecord(source)) return fallback
    const value = source[key]
    return typeof value === 'number' && Number.isFinite(value) ? value : fallback
  },
  bool(source: unknown, key: string, fallback = false): boolean {
    if (!isRecord(source)) return fallback
    const value = source[key]
    return typeof value === 'boolean' ? value : fallback
  },
  record(source: unknown, key: string): Record<string, unknown> | null {
    if (!isRecord(source)) return null
    const value = source[key]
    return isRecord(value) ? value : null
  },
  array(source: unknown, key: string): readonly unknown[] {
    if (!isRecord(source)) return []
    const value = source[key]
    return Array.isArray(value) ? value : []
  },
}
