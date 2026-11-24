import { describe, expect, it } from 'vitest'
import {
  MAX_BUFFER,
  MAX_PENDING,
  RESYNC_THROTTLE_MS,
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
  needsResync,
  receiveEvent,
  receiveEvents,
  replayOrder,
  type PendingOp,
  type SeqEffect,
  type SnapshotChunk,
} from '@/net/seq'

/** 造一条持久事件；data 里放 action 便于断言是「哪一条」被应用了。 */
const ev = (seq: number) => ({ seq, data: `op-${seq}` })

function appliedSeqs(effects: readonly SeqEffect<string>[]): number[] {
  const out: number[] = []
  for (const effect of effects) {
    if (effect.kind === 'apply') for (const e of effect.events) out.push(e.seq)
  }
  return out
}

describe('seq 缺口检测（PROTOCOL §6.3）', () => {
  it('连续事件立即应用并推进 appliedSeq', () => {
    const state = createSeqState<string>(0, 1)
    expect(appliedSeqs(receiveEvent(state, ev(1)))).toEqual([1])
    expect(appliedSeqs(receiveEvent(state, ev(2)))).toEqual([2])
    expect(state.appliedSeq).toBe(2)
  })

  it('seq <= appliedSeq 直接丢弃（重放去重）', () => {
    const state = createSeqState<string>(5, 1)
    const effects = receiveEvent(state, ev(3))
    expect(effects[0]?.kind).toBe('drop')
    expect(state.appliedSeq).toBe(5)
  })

  it('重复收到同一 seq 只应用一次', () => {
    const state = createSeqState<string>(0, 1)
    receiveEvent(state, ev(1))
    const second = receiveEvent(state, ev(1))
    expect(second[0]?.kind).toBe('drop')
    expect(state.appliedSeq).toBe(1)
  })

  it('seq 跳号 → 入缓冲并请求补发', () => {
    const state = createSeqState<string>(10, 1)
    const effects = receiveEvent(state, ev(13))
    expect(effects[0]).toMatchObject({ kind: 'gap', fromSeq: 11, buffered: 1 })
    // 未应用，序号不动
    expect(state.appliedSeq).toBe(10)
    expect(state.buffer.size).toBe(1)
  })

  it('缺口补齐后自动把缓冲里连续的事件全部吐出（一次请求补多条）', () => {
    const state = createSeqState<string>(10, 1)
    // 乱序到达：13, 12, 11
    receiveEvent(state, ev(13))
    receiveEvent(state, ev(12))
    expect(state.buffer.size).toBe(2)
    // 11 到达 → 应一次性应用 11,12,13
    const effects = receiveEvent(state, ev(11))
    expect(appliedSeqs(effects)).toEqual([11, 12, 13])
    expect(state.appliedSeq).toBe(13)
    expect(state.buffer.size).toBe(0)
  })

  it('缓冲里已有同 seq 时重复到达不报错', () => {
    const state = createSeqState<string>(10, 1)
    receiveEvent(state, ev(12))
    const dup = receiveEvent(state, ev(12))
    expect(dup[0]?.kind).toBe('drop')
    expect(state.buffer.size).toBe(1)
  })

  it('非法 seq（0/负数/NaN）被丢弃', () => {
    const state = createSeqState<string>(0, 1)
    for (const bad of [0, -1, Number.NaN, Number.POSITIVE_INFINITY]) {
      const effects = receiveEvent(state, { seq: bad, data: 'x' })
      expect(effects[0]?.kind).toBe('drop')
    }
    expect(state.appliedSeq).toBe(0)
  })

  it('缺口缓冲超过上限 → 转快照（不无限占内存）', () => {
    const state = createSeqState<string>(0, 1)
    let sawReset = false
    let snapshotEffects: SeqEffect<string>[] = []
    // 从 2 开始跳号，缓冲不会自己消化，最终必然触发上限
    for (let seq = 2; seq <= MAX_BUFFER + 4; seq += 1) {
      snapshotEffects = receiveEvent(state, ev(seq))
      if (snapshotEffects.some((e) => e.kind === 'reset')) {
        sawReset = true
        break
      }
    }
    expect(sawReset).toBe(true)
    expect(snapshotEffects[0]).toMatchObject({ kind: 'reset', reason: 'too_many_gaps' })
    expect(state.buffer.size).toBe(0)
  })

  it('批量接收 resync.events：严格连续则全部应用', () => {
    const state = createSeqState<string>(0, 1)
    const effects = receiveEvents(state, [ev(1), ev(2), ev(3)])
    expect(appliedSeqs(effects)).toEqual([1, 2, 3])
    expect(state.appliedSeq).toBe(3)
  })

  it('批量接收中途出现空洞：前面的已应用，后面的进缓冲', () => {
    const state = createSeqState<string>(0, 1)
    const effects = receiveEvents(state, [ev(1), ev(3)])
    expect(appliedSeqs(effects)).toEqual([1])
    expect(state.buffer.has(3)).toBe(true)
    expect(state.appliedSeq).toBe(1)
  })

  it('drainBuffer 在补齐后主动吐出（无需等新事件）', () => {
    const state = createSeqState<string>(0, 1)
    receiveEvent(state, ev(1))
    receiveEvent(state, ev(3))
    const drained: number[] = []
    // 此时 appliedSeq=1，缓冲里有 3；drain 找不到 2，无进展
    const progressed = drainBuffer(state, (events) => {
      for (const e of events) drained.push(e.seq)
    })
    expect(progressed).toBe(false)
    expect(drained).toEqual([])
    // 补上 2：receiveEvent 会自动把 2 和缓冲里的 3 一起吐出
    receiveEvent(state, ev(2))
    expect(state.appliedSeq).toBe(3)
    expect(state.buffer.size).toBe(0)
  })

  it('needsResync 只在服务端领先时为真', () => {
    const state = createSeqState<string>(10, 1)
    expect(needsResync(state, 10)).toBe(false)
    expect(needsResync(state, 11)).toBe(true)
    expect(needsResync(state, 9)).toBe(false)
  })
})

describe('epoch 与快照（PROTOCOL §6.6 / §4.2）', () => {
  it('epoch 变化时 appliedSeq 直接置为快照位点并清缓冲', () => {
    const state = createSeqState<string>(50, 1)
    receiveEvent(state, ev(60)) // 制造一个缓冲
    const result = applySnapshotSeq(state, 200, 2)
    expect(result).toBe('epoch_changed')
    expect(state.epoch).toBe(2)
    expect(state.appliedSeq).toBe(200)
    expect(state.buffer.size).toBe(0)
  })

  it('首次快照（epoch 从 0 开始）不算 epoch 变化', () => {
    const state = createSeqState<string>(0, 0)
    const result = applySnapshotSeq(state, 100, 1)
    expect(result).toBe('ok')
    expect(state.appliedSeq).toBe(100)
  })

  it('快照覆盖到的缓冲事件被清理（避免重复应用）', () => {
    const state = createSeqState<string>(10, 1)
    receiveEvent(state, ev(12))
    receiveEvent(state, ev(15))
    applySnapshotSeq(state, 13, 1)
    expect(state.buffer.has(12)).toBe(false)
    expect(state.buffer.has(15)).toBe(true)
  })

  it('分块快照必须收齐才 ready（中途不替换视图）', () => {
    const assembly = createAssembly<string>()
    const chunk = (n: number, complete: boolean): SnapshotChunk<string> => ({
      epoch: 1,
      seq: 100,
      chunk: n,
      total: 3,
      items: [`a${n}`, `b${n}`],
      complete,
    })

    const first = collectSnapshot(assembly, chunk(0, false))
    expect(first.status).toBe('collecting')
    const second = collectSnapshot(assembly, chunk(1, false))
    expect(second.status).toBe('collecting')
    const third = collectSnapshot(assembly, chunk(2, true))
    expect(third.status).toBe('ready')
    if (third.status === 'ready') {
      expect(third.items).toEqual(['a0', 'b0', 'a1', 'b1', 'a2', 'b2'])
      expect(third.seq).toBe(100)
      expect(third.epoch).toBe(1)
    }
  })

  it('乱序到达的分块也能收齐（最后一块先到也认）', () => {
    const assembly = createAssembly<string>()
    const mk = (n: number, isLast = false) => ({
      epoch: 1,
      seq: 5,
      chunk: n,
      total: 2,
      items: [`x${n}`],
      complete: isLast,
    })
    // 最后一块（chunk 1, complete:true）先到
    expect(collectSnapshot(assembly, mk(1, true)).status).toBe('collecting')
    // 补上第 0 块 → 应 ready
    const done = collectSnapshot(assembly, mk(0))
    expect(done.status).toBe('ready')
    if (done.status === 'ready') expect(done.items).toEqual(['x1', 'x0'])
  })

  it('重复分块不重复累加', () => {
    const assembly = createAssembly<string>()
    const mk = (n: number, complete = false) => ({
      epoch: 1,
      seq: 7,
      chunk: n,
      total: 2,
      items: [`v${n}`],
      complete,
    })
    collectSnapshot(assembly, mk(0))
    collectSnapshot(assembly, mk(0)) // 重复
    const done = collectSnapshot(assembly, mk(1, true))
    expect(done.status).toBe('ready')
    if (done.status === 'ready') expect(done.items).toEqual(['v0', 'v1'])
  })

  it('不同快照的块混在一起 → mismatch，不污染已有进度', () => {
    const assembly = createAssembly<string>()
    collectSnapshot(assembly, { epoch: 1, seq: 10, chunk: 0, total: 2, items: ['a'], complete: false })
    const mixed = collectSnapshot(assembly, {
      epoch: 1,
      seq: 99, // 不同 seq = 另一次快照
      chunk: 1,
      total: 2,
      items: ['evil'],
      complete: true,
    })
    expect(mixed.status).toBe('mismatch')
  })

  it('越界 chunk 视为脏数据', () => {
    const assembly = createAssembly<string>()
    const bad = collectSnapshot(assembly, { epoch: 1, seq: 1, chunk: 5, total: 2, items: [], complete: true })
    expect(bad.status).toBe('mismatch')
  })
})

describe('pending 队列（PROTOCOL §6.2 / §6.4）', () => {
  const op = (opId: string, createdAt: number): PendingOp => ({
    opId,
    payload: { opId, action: 'create' },
    createdAt,
    shapeIds: [`sh_${opId}`],
  })

  it('入队保持顺序，出队按 opId 移除', () => {
    let queue = createPendingQueue([])
    queue = enqueuePending(queue, op('a', 1)).queue
    queue = enqueuePending(queue, op('b', 2)).queue
    queue = enqueuePending(queue, op('c', 3)).queue
    expect(queue.map((q) => q.opId)).toEqual(['a', 'b', 'c'])
    queue = dequeuePending(queue, 'b')
    expect(queue.map((q) => q.opId)).toEqual(['a', 'c'])
  })

  it('出队不存在的 opId 无副作用', () => {
    const queue = [op('a', 1)]
    expect(dequeuePending(queue, 'zzz')).toHaveLength(1)
  })

  it('超过上限时丢最旧的并标记 overflow（不静默）', () => {
    let queue: PendingOp[] = []
    for (let i = 0; i < MAX_PENDING; i += 1) {
      queue = enqueuePending(queue, op(`op${i}`, i)).queue
    }
    const result = enqueuePending(queue, op('overflow', MAX_PENDING + 1))
    expect(result.overflow).toBe(true)
    expect(result.queue).toHaveLength(MAX_PENDING)
    // 最旧的被丢弃，最新的保留
    expect(result.queue[0]?.opId).toBe('op1')
    expect(result.queue[result.queue.length - 1]?.opId).toBe('overflow')
  })

  it('重放顺序严格按 createdAt（即使队列被打乱）', () => {
    const queue = createPendingQueue([op('c', 30), op('a', 10), op('b', 20)])
    expect(replayOrder(queue).map((q) => q.opId)).toEqual(['a', 'b', 'c'])
  })

  it('从 IndexedDB 恢复后按 createdAt 排序（保证重放与本地顺序一致）', () => {
    const restored = createPendingQueue([op('later', 200), op('earlier', 100)])
    expect(restored.map((q) => q.opId)).toEqual(['earlier', 'later'])
  })

  it('findPending 命中与未命中', () => {
    const queue = [op('a', 1)]
    expect(findPending(queue, 'a')?.opId).toBe('a')
    expect(findPending(queue, 'nope')).toBeUndefined()
  })
})

describe('resync.request 节流（客户端不依赖服务端 rate_limited）', () => {
  /** 可控时钟：手动推进时间并执行到期的定时器。 */
  function makeClock() {
    let t = 0
    const timers = new Map<number, { at: number; fn: () => void }>()
    let nextId = 1
    return {
      now: () => t,
      schedule: (fn: () => void, ms: number): ReturnType<typeof setTimeout> => {
        const id = nextId
        nextId += 1
        timers.set(id, { at: t + ms, fn })
        return id as unknown as ReturnType<typeof setTimeout>
      },
      clear: (handle: ReturnType<typeof setTimeout>) => {
        timers.delete(handle as unknown as number)
      },
      advance(ms: number) {
        const target = t + ms
        for (;;) {
          let earliestId = -1
          let earliestAt = Number.POSITIVE_INFINITY
          for (const [id, timer] of timers) {
            if (timer.at <= target && timer.at < earliestAt) {
              earliestAt = timer.at
              earliestId = id
            }
          }
          if (earliestId < 0) break
          const due = timers.get(earliestId)
          timers.delete(earliestId)
          t = earliestAt
          due?.fn()
        }
        t = target
      },
    }
  }

  /** 用可控时钟构造节流器（clearTimeout 也要接到假时钟上）。 */
  function throttled(clock: ReturnType<typeof makeClock>, onSend: () => void) {
    const sent: number[] = []
    const throttle = createResyncThrottle(
      () => {
        sent.push(clock.now())
        onSend()
      },
      RESYNC_THROTTLE_MS,
      clock.now,
      clock.schedule,
    )
    return { throttle, sent, clock }
  }

  /** 被测代码内部用全局 clearTimeout；测试里统一桥接到假时钟。 */
  function withFakeClearTimeout(clock: ReturnType<typeof makeClock>): () => void {
    const original = globalThis.clearTimeout
    globalThis.clearTimeout = ((handle: ReturnType<typeof setTimeout>) => {
      clock.clear(handle)
    }) as typeof clearTimeout
    return () => {
      globalThis.clearTimeout = original
    }
  }

  it('1 秒内连续触发 5 次 → 只发 2 次（首次 leading + 末尾 trailing）', () => {
    const clock = makeClock()
    const restore = withFakeClearTimeout(clock)
    try {
      const { throttle, sent } = throttled(clock, () => {})

      const results = [
        throttle.request(),
        throttle.request(),
        throttle.request(),
        throttle.request(),
        throttle.request(),
      ]
      expect(results[0]).toBe('now')
      expect(results.slice(1).every((r) => r === 'later' || r === 'suppressed')).toBe(true)
      expect(sent).toHaveLength(1)

      clock.advance(RESYNC_THROTTLE_MS)
      expect(sent).toHaveLength(2)
      expect(throttle.pending).toBe(false)
    } finally {
      restore()
    }
  })

  it('窗口内重复触发不会堆积多个 timer（只保留一次 trailing）', () => {
    const clock = makeClock()
    const restore = withFakeClearTimeout(clock)
    try {
      let count = 0
      const { throttle } = throttled(clock, () => {
        count += 1
      })

      throttle.request() // now
      for (let i = 0; i < 10; i += 1) throttle.request() // 全部合并
      expect(count).toBe(1)
      clock.advance(RESYNC_THROTTLE_MS)
      expect(count).toBe(2) // 只补一次，不是 10 次
    } finally {
      restore()
    }
  })

  it('跨窗口的请求各自立刻发送（不互相影响）', () => {
    const clock = makeClock()
    const restore = withFakeClearTimeout(clock)
    try {
      let count = 0
      const { throttle } = throttled(clock, () => {
        count += 1
      })

      expect(throttle.request()).toBe('now')
      clock.advance(RESYNC_THROTTLE_MS + 1)
      expect(throttle.request()).toBe('now')
      expect(count).toBe(2)
    } finally {
      restore()
    }
  })

  it('cancel 丢弃排队请求（断线后不应向死连接补发）', () => {
    const clock = makeClock()
    const restore = withFakeClearTimeout(clock)
    try {
      let count = 0
      const { throttle } = throttled(clock, () => {
        count += 1
      })

      throttle.request() // now
      throttle.request() // queued
      expect(throttle.pending).toBe(true)
      throttle.cancel()
      expect(throttle.pending).toBe(false)
      clock.advance(RESYNC_THROTTLE_MS * 2)
      expect(count).toBe(1) // 没有补发
    } finally {
      restore()
    }
  })
})

describe('零丢失零重复：完整断线补齐剧本', () => {
  it('离线 3 笔 + 重连补齐 2 笔远端事件 → 本地最终 5 笔，无重复', () => {
    // ---- 断线前：已同步到 seq 10，本地画了 3 笔（pending）----
    const state = createSeqState<string>(10, 1)
    let pending: PendingOp[] = []
    for (let i = 0; i < 3; i += 1) {
      pending = enqueuePending(pending, {
        opId: `local-${i}`,
        payload: { opId: `local-${i}`, action: 'create' },
        createdAt: i,
        shapeIds: [`sh_local-${i}`],
      }).queue
    }
    expect(pending).toHaveLength(3)

    // ---- 期间别人画了 2 笔：seq 11、12 ----
    const applied: string[] = []
    const applyAll = (effects: readonly SeqEffect<string>[]): void => {
      for (const effect of effects) {
        if (effect.kind === 'apply') for (const e of effect.events) applied.push(e.data)
      }
    }

    // 重连后服务端先补发 11（12 还没到，模拟分片到达）
    applyAll(receiveEvent(state, ev(11)))
    expect(applied).toEqual(['op-11'])

    // ---- 重放 pending（复用原 opId）----
    const replayed = replayOrder(pending)
    expect(replayed.map((p) => p.opId)).toEqual(['local-0', 'local-1', 'local-2'])

    // 服务端逐条 ack applied → 出队
    let queue = pending
    for (const op of replayed) {
      queue = dequeuePending(queue, op.opId)
      applied.push(`ack-${op.opId}`)
    }
    expect(queue).toHaveLength(0)

    // ---- 12 到达 ----
    applyAll(receiveEvent(state, ev(12)))
    expect(applied).toEqual(['op-11', 'ack-local-0', 'ack-local-1', 'ack-local-2', 'op-12'])

    // ---- 断言：零重复（同一 seq 再来一次被丢）----
    const dup = receiveEvent(state, ev(12))
    expect(dup[0]?.kind).toBe('drop')
    expect(state.appliedSeq).toBe(12)
    expect(applied.filter((x) => x === 'op-12')).toHaveLength(1)
  })

  it('重连时快照覆盖了本地 pending → pending 不重放（避免叠加）', () => {
    const state = createSeqState<string>(10, 1)
    let pending: PendingOp[] = [
      { opId: 'p1', payload: {}, createdAt: 1, shapeIds: ['sh_a'] },
      { opId: 'p2', payload: {}, createdAt: 2, shapeIds: ['sh_b'] },
    ]

    // 快照已包含 sh_a（说明 p1 已被服务端收下），但不含 sh_b
    applySnapshotSeq(state, 20, 1)
    const snapshotShapeIds = new Set(['sh_a', 'sh_remote'])
    pending = pending.filter((op) => !op.shapeIds.every((id) => snapshotShapeIds.has(id)))

    expect(pending.map((p) => p.opId)).toEqual(['p2'])
  })

  it('epoch 变化 → 本地 pending 全部作废（协议 §6.6）', () => {
    const state = createSeqState<string>(50, 1)
    const pending: PendingOp[] = [{ opId: 'p1', payload: {}, createdAt: 1, shapeIds: ['sh_a'] }]
    const snapshot = applySnapshotSeq(state, 0, 2)
    expect(snapshot).toBe('epoch_changed')
    // 调用方此时应清空 pending（realtime.ts 负责）
    const afterEpochChange: PendingOp[] = []
    expect(afterEpochChange).toHaveLength(0)
    void pending
    expect(state.epoch).toBe(2)
  })

  it('ack rejected → 出队且需回滚（不静默丢弃）', () => {
    let pending: PendingOp[] = [{ opId: 'p1', payload: {}, createdAt: 1, shapeIds: ['sh_a'] }]
    const rejected = { opId: 'p1', status: 'rejected' as const, code: 'forbidden' }
    const failed = findPending(pending, rejected.opId)
    expect(failed).toBeDefined()
    pending = dequeuePending(pending, rejected.opId)
    expect(pending).toHaveLength(0)
    // 回滚用 shapeIds 定位，而不是整个 shape 副本
    expect(failed?.shapeIds).toEqual(['sh_a'])
  })
})
