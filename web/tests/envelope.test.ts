import { describe, expect, it } from 'vitest'
import { buildEnvelope, parseFrame, serializeEnvelope, isServerType, read } from '@/net/envelope'
import { PROTOCOL_VERSION } from '@/net/types'

const ctx = { roomId: 'K7M2QX', clientId: 'c_test1234', now: () => 1_700_000_000_000 }

describe('出站信封（PROTOCOL §2.2）', () => {
  it('自动补齐 v/roomId/clientId/ts/payload', () => {
    const envelope = buildEnvelope(ctx, 'ping', { ts: 1 })
    expect(envelope).toEqual({
      v: 1,
      type: 'ping',
      roomId: 'K7M2QX',
      clientId: 'c_test1234',
      ts: 1_700_000_000_000,
      payload: { ts: 1 },
    })
  })

  it('payload 永不为 null（空对象兜底）', () => {
    const envelope = buildEnvelope(ctx, 'bye', undefined)
    expect(envelope.payload).toEqual({})
  })

  it('缺 roomId / clientId 时立即报错，而不是发出脏帧', () => {
    expect(() => buildEnvelope({ ...ctx, roomId: '' }, 'ping', {})).toThrow(/roomId/)
    expect(() => buildEnvelope({ ...ctx, clientId: '' }, 'ping', {})).toThrow(/clientId/)
  })

  it('出站不写 seq（客户端无权分配）', () => {
    const envelope = buildEnvelope(ctx, 'op.add', { opId: 'a1', action: 'create' })
    expect('seq' in envelope).toBe(false)
  })

  it('序列化按 UTF-8 字节计，超限抛错而不是发出去被服务端踢', () => {
    const envelope = buildEnvelope(ctx, 'op.add', { text: '中'.repeat(100) })
    // 100 个中文 = 300 字节 + 信封，限制 260 应触发
    expect(() => serializeEnvelope(envelope, 260)).toThrow(/字节/)
    expect(() => serializeEnvelope(envelope, 65_536)).not.toThrow()
  })
})

describe('入站白名单（PROTOCOL §2.3）', () => {
  const wrap = (type: string, payload: unknown = {}, extra: Record<string, unknown> = {}): string =>
    JSON.stringify({ v: PROTOCOL_VERSION, type, roomId: 'K7M2QX', clientId: 'c_x', ts: 1, payload, ...extra })

  it('已知 type 正常解析', () => {
    const result = parseFrame(wrap('joined', { you: {} }))
    expect(result.ok).toBe(true)
    if (result.ok) expect(result.message.type).toBe('joined')
  })

  it('未知 type 被标记为 unknown_type 且不抛异常（不断线）', () => {
    const result = parseFrame(wrap('future.message.type'))
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.reason).toBe('unknown_type')
  })

  it('客户端消息冒充服务端消息 → unknown_type（白名单不含 hello/op.add 上行）', () => {
    // 注意：op.add 同时在两侧消息表里，这是协议设计（广播与上行同 type）
    const result = parseFrame(wrap('hello'))
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.reason).toBe('unknown_type')
  })

  it('JSON 解析失败 → malformed，不崩', () => {
    const result = parseFrame('{not json')
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.reason).toBe('malformed')
  })

  it('payload 为 null / 非对象 → malformed（协议要求 payload 永不为 null）', () => {
    const result = parseFrame(wrap('pong', null))
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.reason).toBe('malformed')
  })

  it('缺 roomId → malformed', () => {
    const result = parseFrame(JSON.stringify({ v: 1, type: 'pong', clientId: 'c', ts: 1, payload: {} }))
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.reason).toBe('malformed')
  })

  it('版本不符 → bad_version（由上层决定提示刷新）', () => {
    const result = parseFrame(
      JSON.stringify({ v: 2, type: 'pong', roomId: 'K7M2QX', clientId: 'c', ts: 1, payload: {} }),
    )
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.reason).toBe('bad_version')
  })

  it('v 缺失按 1 处理（协议 §0）', () => {
    const result = parseFrame(
      JSON.stringify({ type: 'pong', roomId: 'K7M2QX', clientId: 'c', ts: 1, payload: {} }),
    )
    expect(result.ok).toBe(true)
  })

  it('未知字段被忽略（前向兼容）', () => {
    const result = parseFrame(wrap('pong', { ts: 1 }, { futureField: 'whatever', seq: 99 }))
    expect(result.ok).toBe(true)
  })

  it('信封 seq 被保留（持久消息缺口检测要用）', () => {
    const result = parseFrame(wrap('op.add', { opId: 'a', action: 'create' }, { seq: 42 }))
    expect(result.ok).toBe(true)
    if (result.ok) expect(result.message.seq).toBe(42)
  })

  it('服务端消息白名单覆盖 PROTOCOL §4 全部 11 种', () => {
    const expected = [
      'joined',
      'presence.join',
      'presence.leave',
      'presence.cursor',
      'op.add',
      'ack',
      'resync.snapshot',
      'resync.events',
      'room.meta',
      'error',
      'pong',
    ]
    for (const type of expected) expect(isServerType(type)).toBe(true)
    expect(isServerType('hello')).toBe(false)
    expect(isServerType('left')).toBe(false)
  })
})

describe('payload 取值助手', () => {
  it('类型不符时回落默认值（不抛异常）', () => {
    const payload = { name: 123, count: 'x', flag: 'yes', nested: 'no', list: 'no' }
    expect(read.str(payload, 'name', 'fallback')).toBe('fallback')
    expect(read.num(payload, 'count', 7)).toBe(7)
    expect(read.bool(payload, 'flag', false)).toBe(false)
    expect(read.record(payload, 'nested')).toBeNull()
    expect(read.array(payload, 'list')).toEqual([])
  })

  it('正确取值', () => {
    const payload = { name: '小林', count: 3, flag: true, nested: { a: 1 }, list: [1, 2] }
    expect(read.str(payload, 'name')).toBe('小林')
    expect(read.num(payload, 'count')).toBe(3)
    expect(read.bool(payload, 'flag')).toBe(true)
    expect(read.record(payload, 'nested')).toEqual({ a: 1 })
    expect(read.array(payload, 'list')).toEqual([1, 2])
  })

  it('NaN / Infinity 不被当作有效数字', () => {
    expect(read.num({ n: Number.NaN }, 'n', 0)).toBe(0)
    expect(read.num({ n: Number.POSITIVE_INFINITY }, 'n', 0)).toBe(0)
  })
})
