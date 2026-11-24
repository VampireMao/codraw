import { describe, expect, it } from 'vitest'
import { applyMemberRoleToStore, selfRoleToast, type MemberRoleStore } from '@/stores/scenePort'
import type { MemberRolePayload, PresenceUser, Role } from '@/net/types'

/**
 * `member.role` 的状态迁移（PERMISSIONS §3.1 / §6.2）。
 *
 * 这一层是「授权后工具栏是否真的出现」的唯一决定点，因此必须离开 DOM/Pinia
 * 单独测：任何一次重构把 `setRole` 漏掉，U 就永远看不到工具栏。
 */

interface FakeState {
  cid: string
  role: Role
  ownerClientId: string | null
  members: PresenceUser[]
  toasts: { message: string; tone: string }[]
}

function makeStore(initial: Partial<FakeState> = {}): MemberRoleStore & FakeState {
  const state: FakeState = {
    cid: 'c_me',
    role: 'viewer',
    ownerClientId: null,
    members: [
      { clientId: 'c_me', userId: 'u_me', name: '我', color: '#3d5afe', role: 'viewer' },
      { clientId: 'c_peer', userId: 'u_peer', name: '小林', color: '#e8590c', role: 'viewer' },
    ],
    toasts: [],
    ...initial,
  }
  return Object.assign(state, {
    setRole(role: Role) {
      state.role = role
    },
    setOwnerClientId(ownerClientId: string | null) {
      state.ownerClientId = ownerClientId
    },
    setMemberRole(clientId: string, role: Role) {
      state.members = state.members.map((m) => (m.clientId === clientId ? { ...m, role } : m))
    },
    notify(message: string, tone: 'info' | 'ok' | 'warn') {
      state.toasts.push({ message, tone })
    },
  })
}

const grant = (clientId: string, role: Role, grantedBy?: string): MemberRolePayload => ({
  clientId,
  role,
  ...(grantedBy ? { grantedBy } : {}),
})

describe('applyMemberRole：自己被提升/降级', () => {
  it('viewer → editor：自己的 role 立即变，成员列表同步，toast「你已获得编辑权限」', () => {
    const store = makeStore()
    applyMemberRoleToStore(store, grant('c_me', 'editor', 'c_owner'))

    expect(store.role).toBe('editor')
    expect(store.members.find((m) => m.clientId === 'c_me')?.role).toBe('editor')
    expect(store.toasts).toEqual([{ message: '你已获得编辑权限', tone: 'ok' }])
  })

  it('editor → viewer：降级为只读，提示文案正确', () => {
    const store = makeStore({ role: 'editor' })
    store.members = store.members.map((m) => (m.clientId === 'c_me' ? { ...m, role: 'editor' } : m))

    applyMemberRoleToStore(store, grant('c_me', 'viewer', 'c_owner'))

    expect(store.role).toBe('viewer')
    expect(store.toasts).toEqual([{ message: '你已被设为只读', tone: 'warn' }])
  })

  it('重复收到同一个角色：幂等，不重复 toast', () => {
    const store = makeStore()
    applyMemberRoleToStore(store, grant('c_me', 'viewer'))
    applyMemberRoleToStore(store, grant('c_me', 'viewer'))
    expect(store.role).toBe('viewer')
    expect(store.toasts).toEqual([])
  })

  it('自己成为 owner：role + ownerClientId + toast「你已成为房主」', () => {
    const store = makeStore({ ownerClientId: 'c_old' })
    store.members.push({ clientId: 'c_old', userId: 'u_old', name: '老王', color: '#12b886', role: 'owner' })

    applyMemberRoleToStore(store, grant('c_me', 'owner', 'c_old'))

    expect(store.role).toBe('owner')
    expect(store.ownerClientId).toBe('c_me')
    expect(store.toasts.some((t) => t.message === '你已成为房主')).toBe(true)
  })

  it('自己从 owner 被降为 editor（移交）：提示「房主权限已移交」', () => {
    const store = makeStore({ role: 'owner', ownerClientId: 'c_me' })
    store.members = store.members.map((m) => (m.clientId === 'c_me' ? { ...m, role: 'owner' } : m))

    applyMemberRoleToStore(store, grant('c_me', 'editor', 'c_peer'))

    expect(store.role).toBe('editor')
    expect(store.toasts).toEqual([{ message: '房主权限已移交给其他成员', tone: 'info' }])
  })
})

describe('applyMemberRole：他人角色变化', () => {
  it('别人的授权只改成员列表，不动自己的 role', () => {
    const store = makeStore({ role: 'owner', ownerClientId: 'c_me' })
    store.members = store.members.map((m) => (m.clientId === 'c_me' ? { ...m, role: 'owner' } : m))

    applyMemberRoleToStore(store, grant('c_peer', 'editor', 'c_me'))

    expect(store.role).toBe('owner')
    expect(store.members.find((m) => m.clientId === 'c_peer')?.role).toBe('editor')
    expect(store.toasts).toEqual([])
  })

  it('目标不在成员列表时也不崩（可能刚离线）', () => {
    const store = makeStore()
    expect(() => applyMemberRoleToStore(store, grant('c_ghost', 'editor'))).not.toThrow()
    expect(store.role).toBe('viewer')
  })
})

describe('applyMemberRole：房主归属迁移', () => {
  it('别人成为 owner：ownerClientId 更新，原房主（自己）降为 editor', () => {
    const store = makeStore({ role: 'owner', ownerClientId: 'c_me' })
    store.members = store.members.map((m) => (m.clientId === 'c_me' ? { ...m, role: 'owner' } : m))

    applyMemberRoleToStore(store, grant('c_peer', 'owner', 'c_me'))

    expect(store.ownerClientId).toBe('c_peer')
    expect(store.members.find((m) => m.clientId === 'c_peer')?.role).toBe('owner')
    // 原房主降为 editor，不会出现两个房主徽标
    expect(store.members.find((m) => m.clientId === 'c_me')?.role).toBe('editor')
    expect(store.role).toBe('editor')
  })

  it('当前 owner 被降为 viewer：房间回到无主状态', () => {
    const store = makeStore({ role: 'owner', ownerClientId: 'c_me' })

    applyMemberRoleToStore(store, grant('c_me', 'viewer'))

    expect(store.ownerClientId).toBeNull()
    expect(store.role).toBe('viewer')
  })

  it('无主房间收到别人的 owner 授予：ownerClientId 落位', () => {
    const store = makeStore({ ownerClientId: null })
    applyMemberRoleToStore(store, grant('c_peer', 'owner'))
    expect(store.ownerClientId).toBe('c_peer')
    expect(store.role).toBe('viewer')
  })
})

describe('applyMemberRole：脏数据容错', () => {
  it('非法 role 被忽略', () => {
    const store = makeStore()
    applyMemberRoleToStore(store, { clientId: 'c_me', role: 'admin' as unknown as Role })
    expect(store.role).toBe('viewer')
    expect(store.toasts).toEqual([])
  })

  it('缺 clientId 被忽略', () => {
    const store = makeStore()
    applyMemberRoleToStore(store, { clientId: '', role: 'editor' })
    expect(store.role).toBe('viewer')
  })
})

describe('selfRoleToast：文案映射（PERMISSIONS §6.2）', () => {
  it('三种升级/降级各有确定文案', () => {
    expect(selfRoleToast('viewer', 'editor')?.message).toBe('你已获得编辑权限')
    expect(selfRoleToast('viewer', 'owner')?.message).toBe('你已成为房主')
    expect(selfRoleToast('editor', 'viewer')?.message).toBe('你已被设为只读')
    expect(selfRoleToast('owner', 'viewer')?.message).toBe('你已被设为只读')
  })

  it('角色未变化返回 null（不产生噪音提示）', () => {
    expect(selfRoleToast('editor', 'editor')).toBeNull()
  })
})
