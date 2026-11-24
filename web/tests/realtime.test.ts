import { describe, expect, it, vi } from 'vitest'
import {
  RealtimeClient,
  type Persistence,
  type RealtimeOptions,
  type ScenePort,
  type Transport,
} from '@/net/realtime'
import type { Envelope } from '@/net/envelope'
import type {
  ClientMessageType,
  ConnectionStatus,
  JoinedPayload,
  OpBroadcastPayload,
  PresenceUser,
  ResyncEvent,
  Role,
  RoomLimits,
  ServerMessage,
} from '@/net/types'
import type { Shape } from '@/canvas/shapes'
import { createRect } from '@/canvas/shapes'

/* ------------------------------ 测试替身 ------------------------------ */

class FakeTransport implements Transport {
  readonly sent: { type: string; payload: unknown }[] = []
  readonly envelopes: Envelope<unknown>[] = []
  connected = false

  connect(): void {
    this.connected = true
  }
  disconnect(): void {
    this.connected = false
  }
  send<T>(type: ClientMessageType, payload: T): boolean {
    if (!this.connected) return false
    this.sent.push({ type, payload })
    return true
  }
  sendEnvelope(envelope: Envelope<unknown>): boolean {
    if (!this.connected) return false
    this.envelopes.push(envelope)
    return true
  }
  checkAlive(): void {
    /* noop */
  }
  /** 取出所有 op.add 的 opId（断言重放顺序与幂等） */
  opIds(): string[] {
    return this.sent
      .filter((m) => m.type === 'op.add')
      .map((m) => (m.payload as { opId?: string }).opId ?? '')
  }
  reset(): void {
    this.sent.length = 0
    this.envelopes.length = 0
  }
}

/** 内存版场景：只记录「应用了哪些 op」，不依赖 Pinia/DOM。 */
class FakeScene implements ScenePort {
  readonly applied: string[] = []
  readonly local: string[] = []
  readonly rolledBack: string[] = []
  readonly confirmed: string[] = []
  shapes: Shape[] = []

  applyRemoteOp(payload: OpBroadcastPayload): void {
    this.applied.push(payload.opId)
  }
  replaceAll(shapes: readonly Shape[]): void {
    this.shapes = [...shapes]
  }
  applyLocalOp(payload: { opId: string }): void {
    this.local.push(payload.opId)
  }
  rollbackOp(_opId: string, shapeIds: readonly string[]): void {
    this.rolledBack.push(...shapeIds)
    this.shapes = this.shapes.filter((s) => !shapeIds.includes(s.id))
  }
  confirmOp(_opId: string, shapeIds: readonly string[]): void {
    this.confirmed.push(...shapeIds)
  }
  listShapeIds(): readonly string[] {
    return this.shapes.map((s) => s.id)
  }
  readonly memberRoles: { clientId: string; role: string }[] = []
  applyMemberRole(payload: { clientId: string; role: string }): void {
    this.memberRoles.push({ clientId: payload.clientId, role: payload.role })
  }
}

class MemoryPersistence implements Persistence {
  saved: { appliedSeq: number; epoch: number; pending: unknown[] } | null = null
  initialState: Awaited<ReturnType<Persistence['load']>> = null

  load(): Promise<Awaited<ReturnType<Persistence['load']>>> {
    return Promise.resolve(this.initialState)
  }
  save(state: { appliedSeq: number; epoch: number; pending: readonly unknown[] }): void {
    this.saved = { appliedSeq: state.appliedSeq, epoch: state.epoch, pending: [...state.pending] }
  }
}

const ME: PresenceUser = {
  clientId: 'c_me',
  userId: 'u_me',
  name: '我',
  color: '#3d5afe',
  role: 'editor',
}

const PEER: PresenceUser = {
  clientId: 'c_peer',
  userId: 'u_peer',
  name: '小林',
  color: '#e8590c',
  role: 'editor',
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

interface Harness {
  client: RealtimeClient
  transport: FakeTransport
  scene: FakeScene
  persistence: MemoryPersistence
  statuses: { status: ConnectionStatus; pending: number }[]
  toasts: { message: string; tone: string }[]
  joined: JoinedPayload[]
}

function harness(overrides: Partial<RealtimeOptions> = {}): Harness {
  const transport = new FakeTransport()
  const scene = new FakeScene()
  const persistence = new MemoryPersistence()
  const statuses: { status: ConnectionStatus; pending: number }[] = []
  const toasts: { message: string; tone: string }[] = []
  const joined: JoinedPayload[] = []

  const client = new RealtimeClient({
    roomId: 'K7M2QX',
    clientId: ME.clientId,
    displayName: ME.name,
    transport,
    scene,
    persistence,
    listeners: {
      onStatus: (status, pending) => statuses.push({ status, pending }),
      onJoined: (payload) => joined.push(payload),
      onMembers: () => {},
      onCursor: () => {},
      onToast: (message, tone) => toasts.push({ message, tone }),
      onError: () => {},
      onRoomNotFound: () => {},
    },
    ...overrides,
  })

  return { client, transport, scene, persistence, statuses, toasts, joined }
}

function joinedPayload(seq: number, epoch = 1, role: Role = 'editor'): JoinedPayload {
  return {
    you: { ...ME, role, isOwner: role === 'owner' },
    room: { code: 'K7M2QX', title: '测试房间', epoch, seq },
    online: 2,
    instanceId: 'i_test',
    limits: LIMITS,
    ownerClientId: role === 'owner' ? ME.clientId : null,
  }
}

const msg = <T extends string, P>(type: T, payload: P, seq?: number): ServerMessage =>
  ({
    v: 1 as const,
    type,
    ...(seq !== undefined ? { seq } : {}),
    roomId: 'K7M2QX',
    clientId: 'srv',
    ts: Date.now(),
    payload,
  }) as ServerMessage

function opPayload(opId: string, shapeId: string, by: PresenceUser = PEER): OpBroadcastPayload {
  return {
    opId,
    action: 'create',
    shape: createRect({ x: 0, y: 0 }, { x: 10, y: 10 }, { id: shapeId }) as unknown as Shape,
    by,
  }
}

/* -------------------------------- 用例 -------------------------------- */

describe('RealtimeClient：握手与首帧时序（PROTOCOL §3）', () => {
  it('start() 会连接并先发 hello（不等 joined）', async () => {
    const h = harness()
    await h.client.start()
    expect(h.transport.connected).toBe(true)
    h.client.onOpen()
    expect(h.transport.envelopes).toHaveLength(1)
    const hello = h.transport.envelopes[0]
    expect(hello?.type).toBe('hello')
    expect((hello?.payload as { lastSeq: number }).lastSeq).toBe(0)
  })

  it('hello.lastSeq 从 0 开始，忽略持久化的 appliedSeq（页面重载后画布是空的）', async () => {
    // Why: appliedSeq is persisted, but the scene (shapes) is not. After a page
    // reload the canvas is empty while the stored seq claims "already synced to
    // N". Asking the server for events after N returns nothing and the user sees
    // a blank board. Resetting to 0 forces a full snapshot on every page load.
    const h = harness()
    h.persistence.initialState = {
      key: 'K7M2QX:c_me',
      roomId: 'K7M2QX',
      clientId: 'c_me',
      appliedSeq: 4820,
      epoch: 1,
      pending: [],
      updatedAt: 0,
    }
    await h.client.start()
    h.client.onOpen()
    const hello = h.transport.envelopes[0]
    expect((hello?.payload as { lastSeq: number }).lastSeq).toBe(0)
  })

  it('页面重载后不恢复 appliedSeq，但保留 pending 队列', async () => {
    const h = harness()
    h.persistence.initialState = {
      key: 'K7M2QX:c_me',
      roomId: 'K7M2QX',
      clientId: 'c_me',
      appliedSeq: 99,
      epoch: 1,
      pending: [
        {
          opId: 'op_keep',
          payload: { opId: 'op_keep', action: 'create', shape: { id: 'sh_keep' } },
          createdAt: 1,
          shapeIds: ['sh_keep'],
        },
      ],
      updatedAt: 0,
    }
    await h.client.start()
    h.client.onOpen()

    // seq 归零（强制全量快照）
    expect(h.client.debugState().appliedSeq).toBe(0)
    // pending 保留（本地未确认的编辑不能丢）
    expect(h.client.pendingCount).toBe(1)
  })

  it('joined 后 server.seq 领先 → 发 resync.request{fromSeq: appliedSeq+1}', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(100)))
    const resync = h.transport.sent.find((m) => m.type === 'resync.request')
    expect(resync).toBeDefined()
    expect((resync?.payload as { fromSeq: number }).fromSeq).toBe(1)
  })

  it('joined 且已追平 → 直接进入 online（不无谓 resync）', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))
    expect(h.client.currentStatus).toBe('online')
    expect(h.transport.sent.some((m) => m.type === 'resync.request')).toBe(false)
  })

  it('viewer 角色写入 UI 层（role 从 joined.payload.you.role 来）', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0, 1, 'viewer')))
    expect(h.joined[0]?.you.role).toBe('viewer')
  })
})

describe('RealtimeClient：op 广播与缺口检测', () => {
  it('连续 seq 的远端 op 立即应用', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))
    h.client.onMessage(msg('op.add', opPayload('op1', 'sh_1'), 1), 1)
    expect(h.scene.applied).toEqual(['op1'])
  })

  it('seq 跳号 → 缓冲并发 resync.request，不应用该 op', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))
    h.transport.reset()
    // 服务端跳到 seq 5
    h.client.onMessage(msg('op.add', opPayload('op5', 'sh_5'), 5), 5)
    expect(h.scene.applied).toEqual([])
    const resync = h.transport.sent.find((m) => m.type === 'resync.request')
    expect((resync?.payload as { fromSeq: number }).fromSeq).toBe(1)
  })

  it('resync.events 补齐后，缓冲里的 op 一并应用（不重复请求）', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))
    h.client.onMessage(msg('op.add', opPayload('op3', 'sh_3'), 3), 3)
    expect(h.scene.applied).toEqual([])

    const events: ResyncEvent[] = [
      {
        seq: 1,
        opId: 'op1',
        action: 'create',
        shape: createRect({ x: 0, y: 0 }, { x: 1, y: 1 }, { id: 'sh_1' }) as unknown as Shape,
      },
      {
        seq: 2,
        opId: 'op2',
        action: 'create',
        shape: createRect({ x: 0, y: 0 }, { x: 1, y: 1 }, { id: 'sh_2' }) as unknown as Shape,
      },
    ]
    h.client.onMessage(msg('resync.events', { epoch: 1, fromSeq: 1, toSeq: 2, events, hasMore: false }))
    // 三个都应用了，且顺序为 1,2,3
    expect(h.scene.applied).toEqual(['op1', 'op2', 'op3'])
    expect(h.client.debugState().appliedSeq).toBe(3)
  })

  it('resync.events 不连续（服务端违约）→ 重新请求，不应用错位数据', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))
    h.transport.reset()
    // fromSeq=1 但第一条是 seq=5
    h.client.onMessage(
      msg('resync.events', {
        epoch: 1,
        fromSeq: 1,
        toSeq: 5,
        events: [{ seq: 5, opId: 'op5', action: 'create' }],
        hasMore: false,
      }),
    )
    expect(h.scene.applied).toEqual([])
    expect(h.transport.sent.some((m) => m.type === 'resync.request')).toBe(true)
  })

  it('重复 opId 只应用一次（零重复的最后一道闸）', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))
    h.client.onMessage(msg('op.add', opPayload('op1', 'sh_1'), 1), 1)
    // 同 opId 换了 seq（异常情况）也不能二次应用
    h.client.onMessage(msg('op.add', opPayload('op1', 'sh_1'), 2), 2)
    expect(h.scene.applied).toEqual(['op1'])
  })
})

describe('RealtimeClient：乐观渲染与 ack（PROTOCOL §4.5）', () => {
  function onlineHarness(): Harness {
    const h = harness()
    return h
  }

  async function setupOnline(): Promise<Harness> {
    const h = onlineHarness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))
    h.transport.reset()
    return h
  }

  it('submitOp 先本地乐观上屏并标记 pending，然后立即发网络', async () => {
    const h = await setupOnline()
    h.client.submitOp({ opId: 'local1', action: 'create', shape: {} as Shape }, ['sh_local1'])

    expect(h.scene.local).toEqual(['local1'])
    expect(h.transport.opIds()).toEqual(['local1'])
    expect(h.client.pendingCount).toBe(1)
  })

  it('ack applied → 移除 pending 并转正（不再 60% 透明）', async () => {
    const h = await setupOnline()
    h.client.submitOp({ opId: 'local1', action: 'create', shape: {} as Shape }, ['sh_local1'])
    h.client.onMessage(msg('ack', { opId: 'local1', status: 'applied', seq: 1 }))
    expect(h.client.pendingCount).toBe(0)
    expect(h.scene.confirmed).toContain('sh_local1')
  })

  it('ack duplicate → 也移除 pending（服务端已收过，不重放）', async () => {
    const h = await setupOnline()
    h.client.submitOp({ opId: 'local1', action: 'create', shape: {} as Shape }, ['sh_local1'])
    h.client.onMessage(msg('ack', { opId: 'local1', status: 'duplicate', seq: 1 }))
    expect(h.client.pendingCount).toBe(0)
  })

  it('ack rejected → 回滚乐观渲染 + 移除 pending + toast（禁止静默）', async () => {
    const h = await setupOnline()
    h.client.submitOp({ opId: 'local1', action: 'create', shape: {} as Shape }, ['sh_local1'])
    h.client.onMessage(
      msg('ack', { opId: 'local1', status: 'rejected', code: 'forbidden', message: 'viewer cannot write' }),
    )

    expect(h.scene.rolledBack).toEqual(['sh_local1'])
    expect(h.client.pendingCount).toBe(0)
    expect(h.toasts.some((t) => t.tone === 'warn')).toBe(true)
  })

  it('未连接时 submitOp 只入 pending，不发网络（不静默丢弃）', async () => {
    const h = harness()
    await h.client.start()
    // 还没 onOpen / joined
    h.client.submitOp({ opId: 'local1', action: 'create', shape: {} as Shape }, ['sh_local1'])
    expect(h.scene.local).toEqual(['local1'])
    expect(h.transport.opIds()).toEqual([])
    expect(h.client.pendingCount).toBe(1)
    expect(h.persistence.saved?.pending).toHaveLength(1)
  })
})

describe('RealtimeClient：断线重连补齐（F6 剧本）', () => {
  it('页面重载后先补齐再重放 pending，顺序为本地生成顺序', async () => {
    const h = harness()
    h.persistence.initialState = {
      key: 'K7M2QX:c_me',
      roomId: 'K7M2QX',
      clientId: 'c_me',
      appliedSeq: 10,
      epoch: 1,
      pending: [
        { opId: 'op-b', payload: { opId: 'op-b', action: 'create' }, createdAt: 20, shapeIds: ['sh_b'] },
        { opId: 'op-a', payload: { opId: 'op-a', action: 'create' }, createdAt: 10, shapeIds: ['sh_a'] },
      ],
      updatedAt: 0,
    }
    await h.client.start()
    h.client.onOpen()
    // appliedSeq 被重置为 0，服务端 seq=10 领先 → 必须先补齐
    h.client.onMessage(msg('joined', joinedPayload(10)))
    // 补齐前不重放
    expect(h.transport.opIds()).toEqual([])

    // 服务端补发 1..10
    h.client.onMessage(
      msg('resync.events', {
        fromSeq: 1,
        toSeq: 10,
        hasMore: false,
        events: Array.from({ length: 10 }, (_, i) => ({ seq: i + 1, body: { opId: `srv_${i + 1}` } })),
      }),
    )

    expect(h.client.currentStatus).toBe('online')
    // 重放顺序：a 先 b 后（createdAt 升序），且复用原 opId
    expect(h.transport.opIds()).toEqual(['op-a', 'op-b'])
  })

  it('重连时 server.seq 领先 → 先补齐再重放（不能先发 op 否则顺序错乱）', async () => {
    const h = harness()
    h.persistence.initialState = {
      key: 'K7M2QX:c_me',
      roomId: 'K7M2QX',
      clientId: 'c_me',
      appliedSeq: 10,
      epoch: 1,
      pending: [
        { opId: 'op-a', payload: { opId: 'op-a', action: 'create' }, createdAt: 10, shapeIds: ['sh_a'] },
      ],
      updatedAt: 0,
    }
    await h.client.start()
    h.client.onOpen()
    h.transport.reset()
    h.client.onMessage(msg('joined', joinedPayload(12)))

    // 此时应只发了 resync.request，还没重放 op
    expect(h.transport.opIds()).toEqual([])
    expect(h.transport.sent.some((m) => m.type === 'resync.request')).toBe(true)

    // 页面重载后 appliedSeq 归零，服务端从 1 补到 12
    h.client.onMessage(
      msg('resync.events', {
        epoch: 1,
        fromSeq: 1,
        toSeq: 12,
        events: Array.from({ length: 12 }, (_, i) => ({
          seq: i + 1,
          opId: `r${i + 1}`,
          action: 'create',
          shape: createRect({ x: 0, y: 0 }, { x: 1, y: 1 }) as unknown as Shape,
          by: PEER,
        })),
        hasMore: false,
      }),
    )
    // 补齐完成后进入 online 并重放 pending
    expect(h.transport.opIds()).toEqual(['op-a'])
    expect(h.client.currentStatus).toBe('online')
  })

  it('快照收齐才替换视图；中途只 collect', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))

    const shape = createRect({ x: 0, y: 0 }, { x: 10, y: 10 }, { id: 'sh_1' }) as unknown as Shape
    h.client.onMessage(
      msg('resync.snapshot', { epoch: 1, seq: 5, chunk: 0, total: 2, shapes: [shape], complete: false }),
    )
    expect(h.scene.shapes).toHaveLength(0) // 还没替换

    h.client.onMessage(
      msg('resync.snapshot', { epoch: 1, seq: 5, chunk: 1, total: 2, shapes: [], complete: true }),
    )
    expect(h.scene.shapes).toHaveLength(1)
    expect(h.client.debugState().appliedSeq).toBe(5)
    expect(h.client.currentStatus).toBe('online')
  })

  it('快照覆盖了 pending 的效果 → pending 不再重放（避免叠加）', async () => {
    const h = harness()
    h.persistence.initialState = {
      key: 'K7M2QX:c_me',
      roomId: 'K7M2QX',
      clientId: 'c_me',
      appliedSeq: 0,
      epoch: 1,
      pending: [
        { opId: 'op-a', payload: { opId: 'op-a', action: 'create' }, createdAt: 1, shapeIds: ['sh_a'] },
      ],
      updatedAt: 0,
    }
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))

    // 快照里已经包含 sh_a（说明服务端已收下这条 op）
    const shape = createRect({ x: 0, y: 0 }, { x: 1, y: 1 }, { id: 'sh_a' }) as unknown as Shape
    h.client.onMessage(
      msg('resync.snapshot', { epoch: 1, seq: 3, chunk: 0, total: 1, shapes: [shape], complete: true }),
    )
    h.transport.reset()
    // 触发一次 pending 重放的时机（进入 online）
    expect(h.client.currentStatus).toBe('online')
    expect(h.client.pendingCount).toBe(0)
    expect(h.transport.opIds()).toEqual([])
  })

  it('epoch 变化 → 回滚全部 pending 并等新快照（协议 §6.6）', async () => {
    const h = harness()
    h.persistence.initialState = {
      key: 'K7M2QX:c_me',
      roomId: 'K7M2QX',
      clientId: 'c_me',
      appliedSeq: 50,
      epoch: 1,
      pending: [
        { opId: 'op-a', payload: { opId: 'op-a', action: 'create' }, createdAt: 1, shapeIds: ['sh_a'] },
      ],
      updatedAt: 0,
    }
    await h.client.start()
    h.client.onOpen()
    // 服务端 epoch 变成 2
    h.client.onMessage(msg('joined', joinedPayload(0, 2)))

    expect(h.scene.rolledBack).toEqual(['sh_a'])
    expect(h.client.pendingCount).toBe(0)
    expect(h.client.currentEpoch).toBe(2)
  })
})

describe('RealtimeClient：光标节流与错误处理', () => {
  it('光标超过 20Hz 被丢弃', async () => {
    let clock = 0
    const h = harness({ now: () => clock })
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))
    h.transport.reset()

    clock = 1000
    h.client.sendCursor(1, 1)
    clock = 1010 // 10ms 后：太频繁
    h.client.sendCursor(2, 2)
    clock = 1060 // 60ms 后：允许
    h.client.sendCursor(3, 3)

    const cursors = h.transport.sent.filter((m) => m.type === 'presence.cursor')
    expect(cursors).toHaveLength(2)
  })

  it('未 online 时不上报光标（避免无效流量）', async () => {
    const h = harness()
    await h.client.start()
    h.client.sendCursor(1, 1)
    expect(h.transport.sent.filter((m) => m.type === 'presence.cursor')).toHaveLength(0)
  })

  it('forbidden 错误 → 明确提示无权限（不是静默失败）', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))
    h.client.handleError('forbidden', 'viewer cannot write', false)
    expect(h.toasts.some((t) => t.message.includes('权限'))).toBe(true)
  })

  it('未知错误码不崩（unknown code 走兜底）', async () => {
    const h = harness()
    await h.client.start()
    expect(() => h.client.handleError('future_error_xyz', 'whatever', false)).not.toThrow()
  })

  it('room.grant / room.transfer 走统一 send（不手写信封）', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))

    expect(h.client.grantRole('c_peer', 'editor')).toBe(true)
    expect(h.client.transferOwner('c_peer')).toBe(true)

    expect(h.transport.sent).toContainEqual({
      type: 'room.grant',
      payload: { targetClientId: 'c_peer', role: 'editor' },
    })
    expect(h.transport.sent).toContainEqual({
      type: 'room.transfer',
      payload: { targetClientId: 'c_peer' },
    })
  })

  it('未连接时 grant 返回 false 并提示，而不是静默丢弃', async () => {
    const h = harness()
    // 没 start / 没 online
    expect(h.client.grantRole('c_peer', 'viewer')).toBe(false)
    expect(h.toasts.some((t) => t.message.includes('连接'))).toBe(true)
  })
})

describe('RealtimeClient：member.role 走既有 seq 通道（PERMISSIONS §3.1 / §10）', () => {
  async function online(): Promise<Harness> {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))
    h.transport.reset()
    return h
  }

  it('连续 seq 的 member.role 直接应用（走 applyMemberRole）', async () => {
    const h = await online()
    h.client.onMessage(msg('member.role', { clientId: 'c_peer', role: 'editor' }, 1), 1)
    expect(h.scene.memberRoles).toEqual([{ clientId: 'c_peer', role: 'editor' }])
    expect(h.client.appliedSeq).toBe(1)
  })

  it('seq 跳号 → 入缓冲并请求 resync，不提前应用', async () => {
    const h = await online()
    h.client.onMessage(msg('member.role', { clientId: 'c_peer', role: 'editor' }, 5), 5)
    expect(h.scene.memberRoles).toEqual([])
    expect(h.transport.sent.some((m) => m.type === 'resync.request')).toBe(true)
  })

  it('resync.events 里的 member.role 能被补齐并应用（同一通道）', async () => {
    const h = await online()
    h.client.onMessage(
      msg('resync.events', {
        epoch: 1,
        fromSeq: 1,
        toSeq: 2,
        events: [
          { seq: 1, type: 'member.role', clientId: 'c_peer', role: 'editor', grantedBy: 'c_me' },
          { seq: 2, type: 'member.role', clientId: 'c_me', role: 'owner' },
        ],
        hasMore: false,
      }),
    )
    expect(h.scene.memberRoles).toEqual([
      { clientId: 'c_peer', role: 'editor' },
      { clientId: 'c_me', role: 'owner' },
    ])
    expect(h.client.appliedSeq).toBe(2)
  })

  it('重复 seq 的 member.role 只应用一次（零重复）', async () => {
    const h = await online()
    h.client.onMessage(msg('member.role', { clientId: 'c_peer', role: 'editor' }, 1), 1)
    h.client.onMessage(msg('member.role', { clientId: 'c_peer', role: 'editor' }, 1), 1)
    expect(h.scene.memberRoles).toHaveLength(1)
  })

  it('op 与 member.role 共享同一条单调 seq 流', async () => {
    const h = await online()
    h.client.onMessage(msg('op.add', opPayload('op1', 'sh_1'), 1), 1)
    h.client.onMessage(msg('member.role', { clientId: 'c_peer', role: 'viewer' }, 2), 2)
    h.client.onMessage(msg('op.add', opPayload('op3', 'sh_3'), 3), 3)

    expect(h.scene.applied).toEqual(['op1', 'op3'])
    expect(h.scene.memberRoles).toEqual([{ clientId: 'c_peer', role: 'viewer' }])
    expect(h.client.appliedSeq).toBe(3)
  })

  it('member.role 造成的缺口同样触发 resync（角色不会被跳过）', async () => {
    const h = await online()
    h.client.onMessage(msg('op.add', opPayload('op1', 'sh_1'), 1), 1)
    // seq 2 的 member.role 丢了，直接来 3
    h.client.onMessage(msg('op.add', opPayload('op3', 'sh_3'), 3), 3)
    expect(h.scene.applied).toEqual(['op1'])
    expect(h.transport.sent.some((m) => m.type === 'resync.request')).toBe(true)
  })

  it('member.role 缺 seq（服务端违约）时退化为直接应用，不静默丢弃', async () => {
    const h = await online()
    h.client.onMessage(msg('member.role', { clientId: 'c_me', role: 'editor' }))
    expect(h.scene.memberRoles).toEqual([{ clientId: 'c_me', role: 'editor' }])
  })

  it('非法 role 被丢弃，不推进 seq', async () => {
    const h = await online()
    h.client.onMessage(msg('member.role', { clientId: 'c_peer', role: 'admin' }, 1), 1)
    expect(h.scene.memberRoles).toEqual([])
    expect(h.client.appliedSeq).toBe(0)
  })

  it('presence.join 用全量 users 直接替换（不做增量对账）', async () => {
    const h = harness()
    const members: PresenceUser[][] = []
    const client = new RealtimeClient({
      roomId: 'K7M2QX',
      clientId: ME.clientId,
      displayName: ME.name,
      transport: h.transport,
      scene: h.scene,
      listeners: {
        onStatus: () => {},
        onJoined: () => {},
        onMembers: (users) => members.push([...users]),
        onCursor: () => {},
        onToast: () => {},
        onError: () => {},
        onRoomNotFound: () => {},
      },
    })
    await client.start()
    client.onOpen()
    client.onMessage(msg('presence.join', { user: PEER, users: [ME, PEER], online: 2 }))
    expect(members[0]).toHaveLength(2)
  })
})

describe('RealtimeClient：错误分支不崩', () => {
  it('脏 payload 被忽略而非抛异常', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', { you: null } as unknown as JoinedPayload))
    // 没有崩溃，仍能继续处理后续消息
    expect(() => h.client.onMessage(msg('pong', { ts: 1 }))).not.toThrow()
  })

  it('ack 缺 opId 被忽略', async () => {
    const h = harness()
    await h.client.start()
    expect(() => h.client.onMessage(msg('ack', { status: 'applied' } as never))).not.toThrow()
  })

  it('snapshot 缺字段被忽略', async () => {
    const h = harness()
    await h.client.start()
    expect(() => h.client.onMessage(msg('resync.snapshot', { seq: 1 } as never))).not.toThrow()
  })
})

describe('RealtimeClient：持久化', () => {
  it('每次 ack / submit 都落盘 appliedSeq 与 pending', async () => {
    const h = harness()
    await h.client.start()
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))
    h.client.submitOp({ opId: 'local1', action: 'create', shape: {} as Shape }, ['sh_1'])
    expect(h.persistence.saved?.pending).toHaveLength(1)
    h.client.onMessage(msg('ack', { opId: 'local1', status: 'applied', seq: 1 }))
    expect(h.persistence.saved?.pending).toHaveLength(0)
  })

  it('dispose 后不再重连', async () => {
    const h = harness()
    await h.client.start()
    h.client.stop()
    h.client.onClose(1006, 'gone', true)
    expect(h.transport.connected).toBe(false)
  })

  it('vi.spyOn：onStatus 只在真正变化时触发', async () => {
    const h = harness()
    await h.client.start()
    const spy = vi.fn()
    void spy
    const before = h.statuses.length
    h.client.onOpen()
    h.client.onMessage(msg('joined', joinedPayload(0)))
    const after = h.statuses.length
    expect(after - before).toBeGreaterThan(0)
    // 再收一次相同状态不应新增
    h.client.onMessage(msg('joined', joinedPayload(0)))
    expect(h.statuses.length).toBe(after)
  })
})
