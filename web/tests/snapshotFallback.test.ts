import { describe, expect, it, vi } from 'vitest'
import { fetchFullSnapshot, fetchSnapshotPage, ApiError } from '@/net/rest'
import type { SnapshotPage } from '@/net/rest'
import type { Shape } from '@/canvas/shapes'
import { RealtimeClient, type Persistence, type ScenePort, type Transport } from '@/net/realtime'
import type {
  ClientMessageType,
  ConnectionStatus,
  JoinedPayload,
  RoomLimits,
  ServerMessage,
} from '@/net/types'

/* ========================================================================== *
 * REST 分页拼接
 * ========================================================================== */

/** 造一页返回体；`next` 为 null 表示最后一页。 */
function page(shapes: readonly Partial<Shape>[], next: string | null, seq = 100, epoch = 1): SnapshotPage {
  return {
    seq,
    epoch,
    shapes: shapes.map((s, i) => ({
      id: `sh_${String(i).padStart(4, '0')}`,
      type: 'rect',
      geom: { x: 0, y: 0, w: 10, h: 10 },
      style: { stroke: '#16181d', fill: 'none', strokeWidth: 2, opacity: 1, dash: 'solid' },
      data: {},
      text: null,
      author: null,
      rev: 1,
      deleted: false,
      createdAt: 1,
      updatedAt: 1,
      pending: false,
      ...s,
    })) as unknown as Shape[],
    next,
    complete: next === null,
  }
}

describe('REST 分页快照拼接（协议 §1.5）', () => {
  it('按 next 逐页翻到底，拼出完整集合', async () => {
    const pages: Record<string, SnapshotPage> = {
      '': page([{ id: 'sh_a1' }, { id: 'sh_a2' }], 'sh_a2'),
      sh_a2: page([{ id: 'sh_b1' }, { id: 'sh_b2' }], 'sh_b2'),
      sh_b2: page([{ id: 'sh_c1' }], null),
    }
    const requested: string[] = []
    const originalFetch = globalThis.fetch
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      const url = new URL(String(input), 'http://localhost')
      const after = url.searchParams.get('after') ?? ''
      requested.push(after)
      const body = pages[after]
      if (!body) return new Response('{"error":{"code":"bad_request"}}', { status: 400 })
      return new Response(JSON.stringify(body), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    }) as typeof fetch

    try {
      const full = await fetchFullSnapshot('K7M2QX', 'key-abc')
      expect(full.shapes.map((s) => s.id)).toEqual(['sh_a1', 'sh_a2', 'sh_b1', 'sh_b2', 'sh_c1'])
      expect(full.seq).toBe(100)
      expect(full.epoch).toBe(1)
      // 第一页不带 after，后面两次按上一页最后一页的 id 翻
      expect(requested).toEqual(['', 'sh_a2', 'sh_b2'])
    } finally {
      globalThis.fetch = originalFetch
    }
  })

  it('单页就 complete 时不再翻页', async () => {
    const originalFetch = globalThis.fetch
    let calls = 0
    globalThis.fetch = (async () => {
      calls += 1
      return new Response(JSON.stringify(page([{ id: 'sh_only' }], null)), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    }) as typeof fetch

    try {
      const full = await fetchFullSnapshot('K7M2QX', 'k')
      expect(full.shapes).toHaveLength(1)
      expect(calls).toBe(1)
    } finally {
      globalThis.fetch = originalFetch
    }
  })

  it('超过页数上限时抛 snapshot_too_large（不死循环）', async () => {
    const originalFetch = globalThis.fetch
    globalThis.fetch = (async () => {
      // 永远有下一页：模拟异常服务端
      return new Response(JSON.stringify(page([{ id: 'sh_x' }], 'sh_next')), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    }) as typeof fetch

    try {
      await expect(fetchFullSnapshot('K7M2QX', 'k', undefined, 3)).rejects.toMatchObject({
        code: 'snapshot_too_large',
      })
    } finally {
      globalThis.fetch = originalFetch
    }
  })

  it('分页请求带上 key 与 limit（服务端鉴权与分页都靠它）', async () => {
    const originalFetch = globalThis.fetch
    let captured = ''
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      captured = String(input)
      return new Response(JSON.stringify(page([], null)), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    }) as typeof fetch

    try {
      await fetchSnapshotPage('K7M2QX', 'my-key', null, 250)
      expect(captured).toContain('/api/rooms/K7M2QX/snapshot')
      expect(captured).toContain('key=my-key')
      expect(captured).toContain('limit=250')
    } finally {
      globalThis.fetch = originalFetch
    }
  })

  it('HTTP 错误映射为带 code 的 ApiError', async () => {
    const originalFetch = globalThis.fetch
    globalThis.fetch = (async () =>
      new Response(JSON.stringify({ error: { code: 'forbidden', message: 'nope' } }), {
        status: 403,
        headers: { 'Content-Type': 'application/json' },
      })) as typeof fetch

    try {
      await expect(fetchSnapshotPage('K7M2QX', 'bad', null)).rejects.toBeInstanceOf(ApiError)
      await expect(fetchSnapshotPage('K7M2QX', 'bad', null)).rejects.toMatchObject({
        code: 'forbidden',
        status: 403,
      })
    } finally {
      globalThis.fetch = originalFetch
    }
  })
})

/* ========================================================================== *
 * snapshot_too_large → REST 分页 → 与 WS 快照共用同一条落地路径
 * ========================================================================== */

class FakeTransport implements Transport {
  readonly sent: { type: string; payload: unknown }[] = []
  send<T>(type: ClientMessageType, payload: T): boolean {
    this.sent.push({ type, payload })
    return true
  }
  sendEnvelope(): boolean {
    return true
  }
  connect(): void {}
  disconnect(): void {}
  checkAlive(): void {}
  reset(): void {
    this.sent.length = 0
  }
}

class FakeScene implements ScenePort {
  replacements: { count: number; seq: number }[] = []
  applied: string[] = []
  applyRemoteOp(): void {}
  replaceAll(shapes: readonly unknown[], seq: number, _epoch: number): void {
    this.replacements.push({ count: shapes.length, seq })
  }
  applyLocalOp(): void {}
  rollbackOp(): void {}
  confirmOp(): void {}
  listShapeIds(): readonly string[] {
    return []
  }
  applyMemberRole(): void {}
}

const ME = {
  clientId: 'c_me',
  userId: 'u_me',
  name: '我',
  color: '#3d5afe',
  role: 'editor' as const,
  isOwner: false,
}

const LIMITS: RoomLimits = {
  maxMessageBytes: 65536,
  maxShapeBytes: 32768,
  maxShapesPerRoom: 20000,
  opsPerSecond: 60,
  opsBurst: 120,
  cursorPerSecond: 20,
  resumeMaxOps: 2000,
  resumeMaxBytes: 4194304,
}

function joined(seq: number): JoinedPayload {
  return {
    you: ME,
    room: { code: 'K7M2QX', title: 't', epoch: 1, seq },
    online: 1,
    instanceId: 'i',
    limits: LIMITS,
  }
}

function serverMsg<T extends string, P>(type: T, payload: P, seq?: number): ServerMessage {
  return {
    v: 1,
    type,
    ...(seq !== undefined ? { seq } : {}),
    roomId: 'K7M2QX',
    clientId: 'srv',
    ts: 1,
    payload,
  } as ServerMessage
}

interface Harness {
  client: RealtimeClient
  scene: FakeScene
  transport: FakeTransport
  toasts: { message: string; tone: string }[]
  loadCalls: number
}

function harness(
  loadSnapshot?: () => Promise<{ shapes: readonly unknown[]; seq: number; epoch: number }>,
): Harness {
  const scene = new FakeScene()
  const transport = new FakeTransport()
  const toasts: { message: string; tone: string }[] = []
  const state = { loadCalls: 0 }

  const persistence: Persistence = {
    load: () => Promise.resolve(null),
    save: () => {},
  }

  const client = new RealtimeClient({
    roomId: 'K7M2QX',
    clientId: ME.clientId,
    displayName: ME.name,
    transport,
    scene,
    persistence,
    ...(loadSnapshot
      ? {
          loadSnapshot: async () => {
            state.loadCalls += 1
            return loadSnapshot()
          },
        }
      : {}),
    listeners: {
      onStatus: (_s: ConnectionStatus) => {},
      onJoined: () => {},
      onMembers: () => {},
      onCursor: () => {},
      onToast: (message, tone) => toasts.push({ message, tone }),
      onError: () => {},
      onRoomNotFound: () => {},
    },
  })

  return {
    client,
    scene,
    transport,
    toasts,
    get loadCalls() {
      return state.loadCalls
    },
  } as Harness
}

describe('snapshot_too_large → REST 分页兜底', () => {
  it('收到 snapshot_too_large 后自动拉取分页并整体替换视图', async () => {
    const shapes = [{ id: 'sh_1' }, { id: 'sh_2' }, { id: 'sh_3' }]
    const h = harness(async () => ({ shapes, seq: 500, epoch: 1 }))
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(serverMsg('joined', joined(300)))

    // 服务端拒绝走 WS 快照
    h.client.handleError('snapshot_too_large', 'too many shapes', false)
    // 等异步分页完成
    await vi.waitFor(() => {
      expect(h.scene.replacements).toHaveLength(1)
    })

    expect(h.loadCalls).toBe(1)
    // 与 WS 快照同一条路径：收齐后一次性替换，seq 用分页返回的位点
    expect(h.scene.replacements[0]).toEqual({ count: 3, seq: 500 })
    expect(h.client.appliedSeq).toBe(500)
    expect(h.toasts.some((t) => t.tone === 'info' && t.message.includes('分页'))).toBe(true)
    expect(h.toasts.some((t) => t.tone === 'ok')).toBe(true)
  })

  it('分页失败时给明确提示，不静默（视图保持原状）', async () => {
    const h = harness(async () => {
      throw new ApiError('internal', 'boom', 500)
    })
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(serverMsg('joined', joined(10)))

    h.client.handleError('snapshot_too_large', 'too many', false)
    await vi.waitFor(() => {
      expect(h.toasts.some((t) => t.tone === 'warn' && t.message.includes('失败'))).toBe(true)
    })
    expect(h.scene.replacements).toHaveLength(0)
  })

  it('并发重入只拉一次（多次 snapshot_too_large 不放大请求）', async () => {
    let resolveLoad: ((v: { shapes: readonly unknown[]; seq: number; epoch: number }) => void) | undefined
    const h = harness(
      () =>
        new Promise((resolve) => {
          resolveLoad = resolve
        }),
    )
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(serverMsg('joined', joined(0)))

    h.client.handleError('snapshot_too_large', 'x', false)
    h.client.handleError('snapshot_too_large', 'x', false)
    h.client.handleError('snapshot_too_large', 'x', false)
    await vi.waitFor(() => {
      expect(h.loadCalls).toBe(1)
    })

    if (resolveLoad) resolveLoad({ shapes: [{ id: 'sh_1' }], seq: 7, epoch: 1 })
    await vi.waitFor(() => {
      expect(h.scene.replacements).toHaveLength(1)
    })
    expect(h.loadCalls).toBe(1)
  })

  it('未注入 loadSnapshot 时给出可操作的提示（不要静默卡住）', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(serverMsg('joined', joined(0)))

    h.client.handleError('snapshot_too_large', 'x', false)
    await vi.waitFor(() => {
      expect(h.toasts.some((t) => t.tone === 'warn' && t.message.includes('刷新'))).toBe(true)
    })
  })

  it('分页返回的 seq 参与后续缺口判定（停在正确位点）', async () => {
    const h = harness(async () => ({ shapes: [{ id: 'sh_1' }], seq: 42, epoch: 1 }))
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(serverMsg('joined', joined(40)))

    h.client.handleError('snapshot_too_large', 'x', false)
    await vi.waitFor(() => {
      expect(h.scene.replacements).toHaveLength(1)
    })

    // 分页把 appliedSeq 推到 42，之后的 43 应视为连续（应用，不再请求 resync）
    h.transport.reset()
    h.client.onPersistent(43, {
      opId: 'op43',
      action: 'create',
      shape: { id: 'sh_43' } as unknown as Shape,
    })
    expect(h.client.appliedSeq).toBe(43)
    expect(h.transport.sent.filter((m) => m.type === 'resync.request')).toHaveLength(0)
  })
})
