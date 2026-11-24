/**
 * REST 客户端。契约：docs/PROTOCOL.md §1。
 *
 * 路由（注意是 `/api/rooms`，没有 `/v1`）：
 *   POST /api/rooms
 *   GET  /api/rooms/{code}
 *   POST /api/rooms/{code}/readonly-token
 *   POST /api/rooms/{code}/readonly-token/revoke
 *   GET  /api/rooms/{code}/snapshot?key=&after=&limit=
 */

import type { RoomLimits, Role } from './types'
import type { Shape } from '@/canvas/shapes'

export const API_BASE = '/api'

/** 服务端统一错误体（PROTOCOL §1）。 */
export interface ApiErrorBody {
  readonly error?: {
    readonly code?: string
    readonly message?: string
    readonly requestId?: string
  }
}

export class ApiError extends Error {
  readonly code: string
  readonly status: number
  readonly requestId: string | undefined

  constructor(code: string, message: string, status: number, requestId?: string) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
    this.requestId = requestId
  }
}

interface RequestOptions {
  method?: 'GET' | 'POST'
  body?: unknown
  signal?: AbortSignal
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const init: RequestInit = {
    method: options.method ?? 'GET',
    headers: { Accept: 'application/json' },
  }
  if (options.body !== undefined) {
    init.headers = { ...init.headers, 'Content-Type': 'application/json' }
    init.body = JSON.stringify(options.body)
  }
  if (options.signal) init.signal = options.signal

  const response = await fetch(`${API_BASE}${path}`, init)

  if (!response.ok) {
    let code = `http_${response.status}`
    let message = response.statusText || '请求失败'
    let requestId: string | undefined
    try {
      const body = (await response.json()) as ApiErrorBody
      if (body.error?.code) code = body.error.code
      if (body.error?.message) message = body.error.message
      requestId = body.error?.requestId
    } catch {
      /* 非 JSON 错误体：保留 HTTP 状态文案 */
    }
    throw new ApiError(code, message, response.status, requestId)
  }

  return (await response.json()) as T
}

/* -------------------------------- 房间创建 -------------------------------- */

export interface CreateRoomResponse {
  readonly room: {
    readonly code: string
    readonly title: string
    readonly createdAt: number
    readonly lastActiveAt: number
    readonly shapeCount: number
  }
  readonly keys: {
    readonly editorKey: string
    readonly viewerKey: string
  }
  readonly urls: {
    readonly edit: string
    readonly view: string
  }
  readonly limits: RoomLimits
}

export async function createRoom(title?: string, signal?: AbortSignal): Promise<CreateRoomResponse> {
  const body = title ? { title } : {}
  return request<CreateRoomResponse>('/rooms', { method: 'POST', body, signal })
}

/* -------------------------------- 房间查询 -------------------------------- */

export interface RoomInfo {
  readonly code: string
  readonly title: string
  readonly createdAt: number
  readonly lastActiveAt: number
  readonly shapeCount: number
  readonly online: number
  readonly role: Role
  readonly isFull: boolean
}

export async function fetchRoom(code: string, key?: string, signal?: AbortSignal): Promise<RoomInfo> {
  const query = key ? `?key=${encodeURIComponent(key)}` : ''
  return request<RoomInfo>(`/rooms/${encodeURIComponent(code)}${query}`, { signal })
}

/* ------------------------------- 只读 token ------------------------------- */

export interface ReadonlyToken {
  readonly token: string
  readonly role: 'viewer'
  readonly expiresAt: number
  readonly url: string
}

export async function issueReadonlyToken(
  code: string,
  editorKey: string,
  ttlSeconds = 86400,
  signal?: AbortSignal,
): Promise<ReadonlyToken> {
  return request<ReadonlyToken>(`/rooms/${encodeURIComponent(code)}/readonly-token`, {
    method: 'POST',
    body: { editorKey, ttlSeconds },
    signal,
  })
}

export async function revokeReadonlyToken(
  code: string,
  editorKey: string,
  all = true,
  signal?: AbortSignal,
): Promise<{ revoked: boolean }> {
  return request<{ revoked: boolean }>(`/rooms/${encodeURIComponent(code)}/readonly-token/revoke`, {
    method: 'POST',
    body: { editorKey, all },
    signal,
  })
}

/* --------------------------------- 快照分页 -------------------------------- */

export interface SnapshotPage {
  readonly seq: number
  readonly epoch: number
  readonly shapes: readonly Shape[]
  readonly next: string | null
  readonly complete: boolean
}

/** REST 快照兜底（PROTOCOL §1.5）。图形数 > 5000 时使用。 */
export async function fetchSnapshotPage(
  code: string,
  key: string,
  after: string | null,
  limit = 500,
  signal?: AbortSignal,
): Promise<SnapshotPage> {
  const params = new URLSearchParams({ key, limit: String(limit) })
  if (after) params.set('after', after)
  return request<SnapshotPage>(`/rooms/${encodeURIComponent(code)}/snapshot?${params.toString()}`, { signal })
}

/** 逐页拉完整个快照（上限保护，避免异常服务端导致死循环）。 */
export async function fetchFullSnapshot(
  code: string,
  key: string,
  signal?: AbortSignal,
  maxPages = 40,
): Promise<{ seq: number; epoch: number; shapes: Shape[] }> {
  const shapes: Shape[] = []
  let after: string | null = null
  let seq = 0
  let epoch = 1
  for (let page = 0; page < maxPages; page += 1) {
    const chunk = await fetchSnapshotPage(code, key, after, 500, signal)
    seq = chunk.seq
    epoch = chunk.epoch
    shapes.push(...chunk.shapes)
    if (chunk.complete || !chunk.next) {
      return { seq, epoch, shapes }
    }
    after = chunk.next
  }
  throw new ApiError('snapshot_too_large', '快照分页超过上限', 0)
}

/* -------------------------------- WS 握手 URL ------------------------------- */

export interface HandshakeParams {
  room: string
  cid: string
  name?: string
  key?: string
  /** 只读 token（`?rt=`，强制降权 viewer） */
  rt?: string
}

/**
 * 构造 WS URL。`lastSeq` **不放 URL**（PROTOCOL §2.1），
 * 由升级后的首帧 `hello` 携带，避免 URL 缓存产生陈旧状态。
 */
export function buildWsUrl(params: HandshakeParams, origin = window.location): string {
  const protocol = origin.protocol === 'https:' ? 'wss:' : 'ws:'
  const url = new URL('/ws', `${protocol}//${origin.host}`)
  url.searchParams.set('room', params.room.toUpperCase())
  url.searchParams.set('cid', params.cid)
  if (params.name) url.searchParams.set('name', params.name)
  if (params.key) url.searchParams.set('key', params.key)
  if (params.rt) url.searchParams.set('rt', params.rt)
  return url.toString()
}
