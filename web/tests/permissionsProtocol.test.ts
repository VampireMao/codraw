import { describe, expect, it } from 'vitest'
import { CLIENT_TYPE_SET, buildEnvelope, parseFrame } from '@/net/envelope'
import { CLIENT_MESSAGE_TYPES, SERVER_MESSAGE_TYPES, canRoleEdit } from '@/net/types'

/**
 * 协议白名单与信封（PERMISSIONS §3.1 / §2.3）。
 *
 * 两个方向都要覆盖：
 *  - 出站：`room.grant` / `room.transfer` 必须被 `CLIENT_MESSAGE_TYPES` 认可，
 *    否则 `buildEnvelope` 的类型约束之外还有运行时白名单会拒发；
 *  - 入站：`member.role` 必须被 `SERVER_MESSAGE_TYPES` 认可，否则
 *    `parseFrame` 会把它当未知帧丢掉——那样角色变更永远不会到达 UI。
 */
describe('协议白名单：权限消息', () => {
  it('客户端可以发 room.grant / room.transfer', () => {
    expect(CLIENT_MESSAGE_TYPES).toContain('room.grant')
    expect(CLIENT_MESSAGE_TYPES).toContain('room.transfer')
    expect(CLIENT_TYPE_SET.has('room.grant')).toBe(true)
    expect(CLIENT_TYPE_SET.has('room.transfer')).toBe(true)
  })

  it('服务端可以发 member.role', () => {
    expect(SERVER_MESSAGE_TYPES).toContain('member.role')
  })

  it('member.role 帧能被 parseFrame 解析（不是未知类型）', () => {
    const raw = JSON.stringify({
      v: 1,
      type: 'member.role',
      seq: 42,
      roomId: 'K7M2QX',
      clientId: 'c_owner',
      ts: 1,
      payload: { clientId: 'c_abc', role: 'editor', grantedBy: 'c_owner' },
    })
    const parsed = parseFrame(raw)
    expect(parsed.ok).toBe(true)
    if (parsed.ok) {
      expect(parsed.message.type).toBe('member.role')
      expect(parsed.message.seq).toBe(42)
      const payload = parsed.message.payload as { clientId: string; role: string }
      expect(payload.clientId).toBe('c_abc')
      expect(payload.role).toBe('editor')
    }
  })

  it('room.grant 信封由 buildEnvelope 统一构造（不手写字段）', () => {
    const envelope = buildEnvelope({ roomId: 'K7M2QX', clientId: 'c_owner', now: () => 1000 }, 'room.grant', {
      targetClientId: 'c_abc',
      role: 'editor',
    })
    expect(envelope).toEqual({
      v: 1,
      type: 'room.grant',
      roomId: 'K7M2QX',
      clientId: 'c_owner',
      ts: 1000,
      payload: { targetClientId: 'c_abc', role: 'editor' },
    })
  })

  it('room.transfer 只带 targetClientId', () => {
    const envelope = buildEnvelope({ roomId: 'K7M2QX', clientId: 'c_owner' }, 'room.transfer', {
      targetClientId: 'c_abc',
    })
    expect(envelope.payload).toEqual({ targetClientId: 'c_abc' })
  })
})

describe('角色判定口径（PERMISSIONS §1）', () => {
  it('canWrite = owner || editor（服务端 §5 与前端一致）', () => {
    expect(canRoleEdit('owner')).toBe(true)
    expect(canRoleEdit('editor')).toBe(true)
    expect(canRoleEdit('viewer')).toBe(false)
  })
})
