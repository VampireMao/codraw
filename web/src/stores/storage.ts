/** 本地持久化小工具：所有 key 带 `codraw.` 前缀（UX-SPEC §2）。 */

const PREFIX = 'codraw.'

export function readLocal(key: string): string | null {
  try {
    return window.localStorage.getItem(PREFIX + key)
  } catch {
    return null
  }
}

export function writeLocal(key: string, value: string): void {
  try {
    window.localStorage.setItem(PREFIX + key, value)
  } catch {
    /* 隐私模式 / 配额满：静默降级，不影响绘制 */
  }
}

export function readJson<T>(key: string, fallback: T): T {
  const raw = readLocal(key)
  if (!raw) return fallback
  try {
    return JSON.parse(raw) as T
  } catch {
    return fallback
  }
}

export function writeJson(key: string, value: unknown): void {
  try {
    writeLocal(key, JSON.stringify(value))
  } catch {
    /* noop */
  }
}

/**
 * clientId：**每个标签页独立**，刷新后保持不变。
 *
 * 为什么不用 localStorage：同 origin 的所有标签页共享 localStorage，
 * 开两个标签页演示时会拿到同一个 cid，服务端按 `(clientId, opId)` 去重后
 * 会把第二个标签页的 op 当成重复丢掉，且广播里 `by.clientId === 本地 cid`
 * 会让两个标签页都把对方的 op 误判为「自己的」而不应用——正是 F5 要展示的场景。
 *
 * sessionStorage 天然按标签页隔离、刷新不丢，符合协议 §2.1
 * 「同一标签页生命周期内不变，重连必须复用」。
 */
export function ensureClientId(): string {
  const existing = readSession('cid')
  if (existing && /^[\w-]{8,40}$/.test(existing)) return existing
  const cid =
    typeof crypto !== 'undefined' && 'randomUUID' in crypto
      ? crypto.randomUUID().replace(/-/g, '').slice(0, 20)
      : Math.random().toString(36).slice(2, 12) + Date.now().toString(36).slice(-8)
  writeSession('cid', cid)
  return cid
}

/**
 * 记住服务端下发的权威 clientId。
 *
 * 必须写回 sessionStorage：IndexedDB 的 pending 队列按 `${room}:${cid}` 分键，
 * 刷新后要在 joined 之前就知道该去哪个键加载，否则上一段离线的笔迹会读不到。
 */
export function writeClientId(cid: string): void {
  const value = cid.trim()
  if (!/^[\w-]{8,40}$/.test(value)) return
  writeSession('cid', value)
}

/** 每个房间记住自己的权威 clientId（同一标签页切换房间时不串身份）。 */
export function readRoomClientId(code: string): string | null {
  const value = readSession(`cid.${code.toUpperCase()}`)
  return value && /^[\w-]{8,40}$/.test(value) ? value : null
}

export function writeRoomClientId(code: string, cid: string): void {
  const value = cid.trim()
  if (!/^[\w-]{8,40}$/.test(value)) return
  writeSession(`cid.${code.toUpperCase()}`, value)
}

function readSession(key: string): string | null {
  try {
    return window.sessionStorage.getItem(PREFIX + key)
  } catch {
    return null
  }
}

function writeSession(key: string, value: string): void {
  try {
    window.sessionStorage.setItem(PREFIX + key, value)
  } catch {
    /* 隐私模式：每次刷新换新 id，可接受 */
  }
}

/**
 * 每个房间一个服务端签发的会话 token（`joined.sessionToken`）。
 *
 * 它把 clientId 绑定到服务端签名上：没有 token 的连接会拿到一个全新的
 * clientId，所以仅凭 URL 里的 cid 无法冒充房主/已授权编辑。放 sessionStorage
 * 是为了按标签页隔离、刷新后仍能保持同一身份。
 */
export function readRoomSession(code: string): string | null {
  return readSession(`st.${code.toUpperCase()}`)
}

export function writeRoomSession(code: string, token: string): void {
  if (!token) return
  writeSession(`st.${code.toUpperCase()}`, token)
}

export function clearRoomSession(code: string): void {
  try {
    window.sessionStorage.removeItem(PREFIX + `st.${code.toUpperCase()}`)
  } catch {
    /* noop */
  }
}

export function readName(): string {
  return readLocal('name')?.slice(0, 20) ?? ''
}

export function writeName(name: string): void {
  writeLocal('name', name.trim().slice(0, 20))
}

/** 每个房间独立的编辑钥匙（Phase A 本地模拟；Phase B 从 URL ?k= 读取）。 */
export function readRoomKey(code: string): string | null {
  return readLocal(`key.${code.toUpperCase()}`)
}

export function writeRoomKey(code: string, key: string): void {
  writeLocal(`key.${code.toUpperCase()}`, key)
}
