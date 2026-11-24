/**
 * 序号状态机（纯逻辑，不依赖 DOM / WS，可直接单测）。
 *
 * 契约：docs/PROTOCOL.md §6.3（缺口检测）、§6.4（重连补齐）、§6.6（epoch）。
 *
 * 设计原则：**所有决策返回「效果列表」**，由调用方执行（发网络、改 store）。
 * 这样断线补齐的全部边界都能在没有浏览器、没有后端的条件下测出来——
 * 这是 F6「零丢失零重复」唯一可信的验证方式。
 */

/** 一条持久消息。`data` 原样保存，缺口补齐后可直接应用，无需回头找原始帧。 */
export interface SeqEvent<D = unknown> {
  readonly seq: number
  readonly data: D
}

export type SeqEffect<D> =
  /** 按顺序应用这些事件（已连续，且包含刚从缓冲里取出的） */
  | { readonly kind: 'apply'; readonly events: readonly SeqEvent<D>[] }
  /** 重复事件，丢弃（不产生任何 UI 变化） */
  | { readonly kind: 'drop'; readonly seq: number }
  /** 发现缺口，已入缓冲；请求补发 [fromSeq, ...] */
  | { readonly kind: 'gap'; readonly buffered: number; readonly fromSeq: number }
  /** 缺口过多 / 需要重新拉全量快照 */
  | { readonly kind: 'reset'; readonly reason: 'too_many_gaps' }

export interface SeqState<D = unknown> {
  /** 已连续应用的最大 seq（持久化到 IndexedDB） */
  appliedSeq: number
  /** 房间写入世代（持久化；变化时清空本地视图） */
  epoch: number
  /** 缺口缓冲：seq → 完整事件。Map 保证乱序到达也能按序吐出 */
  buffer: Map<number, SeqEvent<D>>
}

export function createSeqState<D = unknown>(appliedSeq = 0, epoch = 0): SeqState<D> {
  return { appliedSeq, epoch, buffer: new Map() }
}

/** 缓冲上限：超过说明服务端/网络严重异常，直接转快照比无限占内存安全。 */
export const MAX_BUFFER = 512

/**
 * 接收一条持久事件。
 *
 * - `seq <= appliedSeq` → drop（重放去重）
 * - `seq === appliedSeq + 1` → 应用，然后**自动 flush 缓冲中连续的部分**
 *   （一次缺口请求能补多条，不必每来一条都发 resync）
 * - `seq > appliedSeq + 1` → 入 buffer，返回 gap 请求
 */
export function receiveEvent<D>(state: SeqState<D>, event: SeqEvent<D>): SeqEffect<D>[] {
  const { seq } = event

  if (!Number.isFinite(seq) || seq <= 0) {
    return [{ kind: 'drop', seq }]
  }

  // 已应用过（重复广播 / 重放）
  if (seq <= state.appliedSeq) {
    state.buffer.delete(seq)
    return [{ kind: 'drop', seq }]
  }

  // 正好接上
  if (seq === state.appliedSeq + 1) {
    const applied: SeqEvent<D>[] = [event]
    state.appliedSeq = seq
    // 缓冲里可能已攒着后续连续事件（乱序到达），一并吐出
    for (;;) {
      const next = state.buffer.get(state.appliedSeq + 1)
      if (!next) break
      state.buffer.delete(state.appliedSeq + 1)
      applied.push(next)
      state.appliedSeq += 1
    }
    return [{ kind: 'apply', events: applied }]
  }

  // 重复缓冲（同 seq 收到两次）
  if (state.buffer.has(seq)) {
    return [{ kind: 'drop', seq }]
  }

  // 缺口
  state.buffer.set(seq, event)
  if (state.buffer.size > MAX_BUFFER) {
    state.buffer.clear()
    return [{ kind: 'reset', reason: 'too_many_gaps' }]
  }
  return [{ kind: 'gap', buffered: state.buffer.size, fromSeq: state.appliedSeq + 1 }]
}

/** 批量接收（resync.events 用）。 */
export function receiveEvents<D>(state: SeqState<D>, events: readonly SeqEvent<D>[]): SeqEffect<D>[] {
  const effects: SeqEffect<D>[] = []
  for (const event of events) effects.push(...receiveEvent(state, event))
  return effects
}

/** 缺口补齐后，把缓冲里能连续的部分一次性取出（返回 true 表示有进展）。 */
export function drainBuffer<D>(state: SeqState<D>, emit: (events: readonly SeqEvent<D>[]) => void): boolean {
  let progressed = false
  const applied: SeqEvent<D>[] = []
  for (;;) {
    const next = state.buffer.get(state.appliedSeq + 1)
    if (!next) break
    state.buffer.delete(state.appliedSeq + 1)
    applied.push(next)
    state.appliedSeq += 1
    progressed = true
  }
  if (applied.length > 0) emit(applied)
  return progressed
}

/**
 * 应用快照：调用方把分块攒齐后传入。
 * 快照本身代表该 seq 的完整状态，因此直接置位。
 */
export function applySnapshotSeq(
  state: SeqState,
  snapshotSeq: number,
  snapshotEpoch: number,
): 'ok' | 'epoch_changed' {
  const epochChanged = state.epoch !== 0 && snapshotEpoch !== state.epoch
  state.epoch = snapshotEpoch
  state.appliedSeq = Math.max(0, snapshotSeq)
  // 快照已覆盖的缓冲事件作废（它们可能已在快照里）
  for (const seq of [...state.buffer.keys()]) {
    if (seq <= state.appliedSeq) state.buffer.delete(seq)
  }
  return epochChanged ? 'epoch_changed' : 'ok'
}

/** 判断是否需要向服务端补齐。 */
export function needsResync(state: SeqState, serverSeq: number): boolean {
  return serverSeq > state.appliedSeq
}

/* ------------------------------ 快照分块攒批 ------------------------------ */

export interface SnapshotChunk<D = unknown> {
  readonly epoch: number
  readonly seq: number
  readonly chunk: number
  readonly total: number
  readonly items: readonly D[]
  readonly complete: boolean
}

export interface SnapshotAssembly<D = unknown> {
  epoch: number
  seq: number
  total: number
  received: number
  /** 已收到的 chunk 序号（去重用；允许乱序到达） */
  seen: Set<number>
  items: D[]
  /** 是否见到过带 complete:true 的块（乱序时最后一块可能先到） */
  sawComplete: boolean
  /** 用于检测「不同快照的块被混在一起」 */
  token: string
  startedAt: number
}

export function createAssembly<D = unknown>(): SnapshotAssembly<D> {
  return {
    epoch: 0,
    seq: 0,
    total: 0,
    received: 0,
    seen: new Set(),
    items: [],
    sawComplete: false,
    token: '',
    startedAt: 0,
  }
}

export type SnapshotResult<D> =
  | { readonly status: 'collecting'; readonly progress: number }
  | { readonly status: 'ready'; readonly epoch: number; readonly seq: number; readonly items: readonly D[] }
  | { readonly status: 'mismatch' }

/**
 * 收一块快照。
 *
 * 关键规则（PROTOCOL §4.2）：
 *  - **收齐（收到全部 `total` 块且最后一块 `complete:true`）才替换视图**，中途绝不半替换；
 *  - 每块 `seq` 相同，用于识别「这是同一次快照」；
 *  - 重复块不重复累加；乱序到达也允许。
 */
export function collectSnapshot<D>(
  assembly: SnapshotAssembly<D>,
  chunk: SnapshotChunk<D>,
  now = Date.now(),
): SnapshotResult<D> {
  const token = `${chunk.epoch}:${chunk.seq}:${chunk.total}`

  // 第一块：初始化
  if (assembly.received === 0 && assembly.seen.size === 0) {
    assembly.token = token
    assembly.epoch = chunk.epoch
    assembly.seq = chunk.seq
    assembly.total = chunk.total
    assembly.items = []
    assembly.sawComplete = false
    assembly.startedAt = now
  } else if (assembly.token !== token) {
    // 混杂了另一次快照的块：丢弃，不动已有进度
    return { status: 'mismatch' }
  }

  // chunk 越界视为脏数据
  if (
    !Number.isInteger(chunk.chunk) ||
    chunk.chunk < 0 ||
    chunk.chunk >= assembly.total ||
    assembly.total <= 0
  ) {
    return { status: 'mismatch' }
  }

  // 记录是否见过 complete 标记（乱序到达时最后一块可能最先到）
  if (chunk.complete) assembly.sawComplete = true

  if (assembly.seen.has(chunk.chunk)) {
    // 重复块（重连后重发）：不计入进度
    return { status: 'collecting', progress: assembly.received / assembly.total }
  }

  assembly.seen.add(chunk.chunk)
  assembly.received += 1
  assembly.items.push(...chunk.items)

  // 收齐 = 所有块都到了 且 见过 complete 标记（协议要求最后一块 complete:true）
  const allChunks = assembly.received >= assembly.total
  const lastChunkArrived = assembly.seen.has(assembly.total - 1)
  if (allChunks && assembly.sawComplete && lastChunkArrived) {
    return { status: 'ready', epoch: assembly.epoch, seq: assembly.seq, items: assembly.items }
  }

  return { status: 'collecting', progress: assembly.total > 0 ? assembly.received / assembly.total : 0 }
}

/** 快照攒批超时（协议 15s）→ 上层改走 REST 分页。 */
export const SNAPSHOT_TIMEOUT_MS = 15_000

export function isAssemblyStale<D>(assembly: SnapshotAssembly<D>, now = Date.now()): boolean {
  return assembly.seen.size > 0 && now - assembly.startedAt > SNAPSHOT_TIMEOUT_MS
}

/* ------------------------------- pending 队列 ------------------------------ */

export interface PendingOp {
  readonly opId: string
  /** 完整 `op.add` payload（重放时原样发送，复用原 opId） */
  readonly payload: unknown
  readonly createdAt: number
  /** 本地乐观渲染涉及的 shape id，回滚时按它删 */
  readonly shapeIds: readonly string[]
}

/** pending 有界队列（PROTOCOL §6.2 建议 2000 op）。溢出必须显式告知用户。 */
export const MAX_PENDING = 2000

export function createPendingQueue(initial: readonly PendingOp[] = []): PendingOp[] {
  // 恢复时按 createdAt 排序，保证重放顺序与本地生成顺序一致
  return [...initial].sort((a, b) => a.createdAt - b.createdAt)
}

export function enqueuePending(queue: PendingOp[], op: PendingOp): { queue: PendingOp[]; overflow: boolean } {
  const next = [...queue, op]
  if (next.length > MAX_PENDING) {
    // 丢最旧的：它最可能已被服务端接收（丢新的会让用户刚画的消失，更差）
    return { queue: next.slice(next.length - MAX_PENDING), overflow: true }
  }
  return { queue: next, overflow: false }
}

export function dequeuePending(queue: PendingOp[], opId: string): PendingOp[] {
  return queue.filter((entry) => entry.opId !== opId)
}

export function findPending(queue: readonly PendingOp[], opId: string): PendingOp | undefined {
  return queue.find((entry) => entry.opId === opId)
}

/** 重放顺序：**严格按 createdAt**（本地生成顺序），且复用原 opId。 */
export function replayOrder(queue: readonly PendingOp[]): readonly PendingOp[] {
  return [...queue].sort((a, b) => a.createdAt - b.createdAt)
}

/* ---------------------------- resync.request 节流 --------------------------- */

/**
 * `resync.request` 的本地节流器（trailing 合并，**不是丢弃**）。
 *
 * 为什么需要：服务端对同一连接限制 2 次/秒，超出回 `rate_limited`，
 * 客户端会因此多付一次退避。与其让服务端兜底，不如客户端自己管住。
 *
 * 语义（关键差别）：
 *  - **leading**：第一个请求立刻发（缺口要尽快补，不能等）；
 *  - **trailing**：窗口内后续请求合并为 1 次，在窗口结束时补发**最后一次**
 *    （丢弃会漏掉期间产生的新缺口，导致停在 `reconnecting` 不动）。
 *
 * 单测口径：1 秒内连续触发 5 次 → 只发 2 次（首次 + 末尾那次）。
 */
export const RESYNC_THROTTLE_MS = 1000

export interface ResyncThrottle {
  /**
   * 请求发送一次 resync。
   * @returns `'now'` 表示应立刻发；`'later'` 表示已排队，窗口结束时补发；
   *          `'suppressed'` 仅在窗口内已有排队请求时出现（无需动作）。
   */
  request: () => 'now' | 'later' | 'suppressed'
  /** 取消排队（连接断开/销毁时调用，避免向死连接补发） */
  cancel: () => void
  /** 是否有排队未发 */
  readonly pending: boolean
}

/**
 * @param send 实际发送函数
 * @param windowMs 节流窗口
 * @param schedule 定时器注入（测试用假时钟）
 */
export function createResyncThrottle(
  send: () => void,
  windowMs = RESYNC_THROTTLE_MS,
  now: () => number = () => Date.now(),
  schedule: (fn: () => void, ms: number) => ReturnType<typeof setTimeout> = (fn, ms) => setTimeout(fn, ms),
): ResyncThrottle {
  let lastSentAt = Number.NEGATIVE_INFINITY
  let timer: ReturnType<typeof setTimeout> | null = null
  let queued = false

  const fire = (): void => {
    lastSentAt = now()
    send()
  }

  const flush = (): void => {
    timer = null
    if (!queued) return
    queued = false
    fire()
  }

  return {
    request() {
      const elapsed = now() - lastSentAt
      if (timer === null && elapsed >= windowMs) {
        fire()
        return 'now'
      }
      if (queued) return 'suppressed'
      queued = true
      if (timer === null) {
        // 窗口剩余时间；至少 0（now 恰好到边界时立刻补发）
        timer = schedule(flush, Math.max(0, windowMs - elapsed))
      }
      return 'later'
    },
    cancel() {
      if (timer !== null) {
        clearTimeout(timer)
        timer = null
      }
      queued = false
    },
    get pending() {
      return queued
    },
  }
}
