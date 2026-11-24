/**
 * IndexedDB 持久化（不是 localStorage——pending 队列可能有几十 KB 且需原子写）。
 *
 * 存三样东西，缺一不可（PROTOCOL §6.4「零丢失的三个必要条件」）：
 *   1. `appliedSeq` —— 重连时补发的起点
 *   2. `pending[]`  —— 未确认的 op（含 opId，重放必须复用）
 *   3. `clientId`   —— 同一浏览器标签页生命周期内不变，服务端靠它去重
 *
 * 为什么按房间分 key：同一浏览器可能同时开两个房间的标签页，
 * 共用一个 appliedSeq 会导致互相覆盖。
 */

const DB_NAME = 'codraw'
const DB_VERSION = 1
const STORE = 'room-state'

export interface PersistedRoomState {
  /** `${roomId}:${clientId}` */
  readonly key: string
  readonly roomId: string
  readonly clientId: string
  readonly appliedSeq: number
  readonly epoch: number
  readonly pending: readonly PersistedOp[]
  readonly updatedAt: number
}

export interface PersistedOp {
  readonly opId: string
  readonly payload: unknown
  readonly createdAt: number
  readonly shapeIds: readonly string[]
}

let dbPromise: Promise<IDBDatabase> | null = null

function openDb(): Promise<IDBDatabase> {
  if (dbPromise) return dbPromise
  dbPromise = new Promise((resolve, reject) => {
    if (typeof indexedDB === 'undefined') {
      reject(new Error('IndexedDB 不可用'))
      return
    }
    const request = indexedDB.open(DB_NAME, DB_VERSION)
    request.onupgradeneeded = () => {
      const db = request.result
      if (!db.objectStoreNames.contains(STORE)) {
        db.createObjectStore(STORE, { keyPath: 'key' })
      }
    }
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error ?? new Error('IndexedDB 打开失败'))
  })
  return dbPromise
}

function tx<T>(mode: IDBTransactionMode, run: (store: IDBObjectStore) => IDBRequest<T>): Promise<T> {
  return openDb().then(
    (db) =>
      new Promise<T>((resolve, reject) => {
        const transaction = db.transaction(STORE, mode)
        const request = run(transaction.objectStore(STORE))
        request.onsuccess = () => resolve(request.result)
        request.onerror = () => reject(request.error ?? new Error('IndexedDB 操作失败'))
      }),
  )
}

const stateKey = (roomId: string, clientId: string): string => `${roomId.toUpperCase()}:${clientId}`

/** 读取房间本地状态；不存在或 IndexedDB 不可用时返回 null（降级为纯内存）。 */
export async function loadRoomState(roomId: string, clientId: string): Promise<PersistedRoomState | null> {
  try {
    const raw = await tx<PersistedRoomState | undefined>('readonly', (store) =>
      store.get(stateKey(roomId, clientId)),
    )
    if (!raw || raw.roomId !== roomId.toUpperCase()) return null
    return raw
  } catch {
    return null
  }
}

/** 写入（节流由调用方控制；每次写整个小对象，避免多表事务）。 */
export async function saveRoomState(state: Omit<PersistedRoomState, 'key' | 'updatedAt'>): Promise<void> {
  try {
    const record: PersistedRoomState = {
      ...state,
      key: stateKey(state.roomId, state.clientId),
      updatedAt: Date.now(),
    }
    await tx('readwrite', (store) => store.put(record))
  } catch {
    /* 隐私模式 / 配额满：静默降级为内存模式，不打断绘制 */
  }
}

export async function clearRoomState(roomId: string, clientId: string): Promise<void> {
  try {
    await tx('readwrite', (store) => store.delete(stateKey(roomId, clientId)))
  } catch {
    /* noop */
  }
}

/**
 * 节流写入器：把高频调用合并成「最多每 N 毫秒一次」。
 * 落笔时 pending 每加一条都会变化，不节流会打爆 IDB 事务。
 */
export function createThrottledSaver(delayMs = 400): {
  schedule: (snapshot: () => Omit<PersistedRoomState, 'key' | 'updatedAt'>) => void
  flush: () => Promise<void>
  dispose: () => void
} {
  let timer: ReturnType<typeof setTimeout> | null = null
  let pendingSnapshot: (() => Omit<PersistedRoomState, 'key' | 'updatedAt'>) | null = null
  let inFlight: Promise<void> = Promise.resolve()

  const flush = async (): Promise<void> => {
    if (timer) {
      clearTimeout(timer)
      timer = null
    }
    if (!pendingSnapshot) return
    const build = pendingSnapshot
    pendingSnapshot = null
    const state = build()
    inFlight = inFlight.then(() => saveRoomState(state))
    await inFlight
  }

  return {
    schedule(snapshot) {
      pendingSnapshot = snapshot
      if (timer) return
      timer = setTimeout(() => {
        timer = null
        void flush()
      }, delayMs)
    },
    flush,
    dispose() {
      if (timer) clearTimeout(timer)
      timer = null
      pendingSnapshot = null
    },
  }
}
