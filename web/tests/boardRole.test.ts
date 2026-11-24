import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useBoardStore } from '@/stores/board'
import type { JoinedUser } from '@/net/types'

/**
 * store 层的 `canEdit` / `isOwner` / `setRole`（PERMISSIONS §1 / §6.2）。
 *
 * 这一层是「工具栏显隐」的唯一数据源：Toolbar、BoardCanvas、只读横幅
 * 全部依赖 `store.canEdit`，所以必须在这里钉死行为，而不是只测纯函数。
 */

function joinedUser(role: JoinedUser['role']): JoinedUser {
  return {
    clientId: 'c_me',
    userId: 'u_me',
    name: '我',
    color: '#3d5afe',
    role,
    isOwner: role === 'owner',
  }
}

const LIMITS = {
  maxMessageBytes: 65536,
  maxShapeBytes: 32768,
  maxShapesPerRoom: 20000,
  opsPerSecond: 60,
  opsBurst: 120,
  cursorPerSecond: 20,
  resumeMaxOps: 2000,
  resumeMaxBytes: 4194304,
}

beforeEach(() => {
  setActivePinia(createPinia())
})

describe('board store：canEdit / isOwner 由 role 驱动', () => {
  it('owner：canEdit=true、isOwner=true、readOnly=false', () => {
    const store = useBoardStore()
    store.applyJoined({
      you: joinedUser('owner'),
      room: { code: 'K7M2QX', title: 't', epoch: 1, seq: 0 },
      online: 1,
      limits: LIMITS,
      ownerClientId: 'c_me',
    })
    expect(store.role).toBe('owner')
    expect(store.canEdit).toBe(true)
    expect(store.isOwner).toBe(true)
    expect(store.readOnly).toBe(false)
    expect(store.ownerClientId).toBe('c_me')
  })

  it('editor：canEdit=true、isOwner=false', () => {
    const store = useBoardStore()
    store.applyJoined({
      you: joinedUser('editor'),
      room: { code: 'K7M2QX', title: 't', epoch: 1, seq: 0 },
      online: 1,
      limits: LIMITS,
      ownerClientId: 'c_other',
    })
    expect(store.canEdit).toBe(true)
    expect(store.isOwner).toBe(false)
    expect(store.readOnly).toBe(false)
  })

  it('viewer：canEdit=false、readOnly=true', () => {
    const store = useBoardStore()
    store.applyJoined({
      you: joinedUser('viewer'),
      room: { code: 'K7M2QX', title: 't', epoch: 1, seq: 0 },
      online: 1,
      limits: LIMITS,
      ownerClientId: 'c_other',
    })
    expect(store.canEdit).toBe(false)
    expect(store.isOwner).toBe(false)
    expect(store.readOnly).toBe(true)
  })

  it('setRole 立即翻转 canEdit（工具栏「立刻出现/消失」的数据前提）', () => {
    const store = useBoardStore()
    store.applyJoined({
      you: joinedUser('viewer'),
      room: { code: 'K7M2QX', title: 't', epoch: 1, seq: 0 },
      online: 1,
      limits: LIMITS,
      ownerClientId: 'c_other',
    })
    expect(store.canEdit).toBe(false)

    store.setRole('editor')
    expect(store.canEdit).toBe(true)
    expect(store.readOnly).toBe(false)

    store.setRole('viewer')
    expect(store.canEdit).toBe(false)
    expect(store.readOnly).toBe(true)
  })

  it('降级时清空选区并回到选择工具（不能继续拖一个已无权限的图形）', () => {
    const store = useBoardStore()
    store.applyJoined({
      you: joinedUser('editor'),
      room: { code: 'K7M2QX', title: 't', epoch: 1, seq: 0 },
      online: 1,
      limits: LIMITS,
      ownerClientId: 'c_other',
    })
    store.setTool('pen')
    store.setSelection(['sh_1'])
    expect(store.tool).toBe('pen')
    expect(store.selectionCount).toBe(1)

    store.setRole('viewer')
    expect(store.tool).toBe('select')
    expect(store.selectionCount).toBe(0)
  })

  it('viewer 调 setTool 非选择工具无效（画布之外的兜底）', () => {
    const store = useBoardStore()
    store.applyJoined({
      you: joinedUser('viewer'),
      room: { code: 'K7M2QX', title: 't', epoch: 1, seq: 0 },
      online: 1,
      limits: LIMITS,
      ownerClientId: 'c_other',
    })
    store.setTool('pen')
    expect(store.tool).toBe('select')
  })

  it('joined 缺 ownerClientId 时按 role 兜底推断房主', () => {
    const store = useBoardStore()
    store.applyJoined({
      you: joinedUser('owner'),
      room: { code: 'K7M2QX', title: 't', epoch: 1, seq: 0 },
      online: 1,
      limits: LIMITS,
    })
    expect(store.ownerClientId).toBe('c_me')
  })

  it('无主房间：ownerClientId=null，isOwner=false', () => {
    const store = useBoardStore()
    store.applyJoined({
      you: joinedUser('viewer'),
      room: { code: 'K7M2QX', title: 't', epoch: 1, seq: 0 },
      online: 1,
      limits: LIMITS,
      ownerClientId: null,
    })
    expect(store.ownerClientId).toBeNull()
    expect(store.isOwner).toBe(false)
  })
})

describe('board store：setMemberRole 的响应式语义', () => {
  it('替换数组引用（浅监听能收到），并同步 me', () => {
    const store = useBoardStore()
    store.applyJoined({
      you: joinedUser('owner'),
      room: { code: 'K7M2QX', title: 't', epoch: 1, seq: 0 },
      online: 2,
      limits: LIMITS,
      ownerClientId: 'c_me',
    })
    store.setMembers([
      { clientId: 'c_me', userId: 'u_me', name: '我', color: '#3d5afe', role: 'owner' },
      { clientId: 'c_peer', userId: 'u_peer', name: '小林', color: '#e8590c', role: 'viewer' },
    ])
    const before = store.members

    store.setMemberRole('c_peer', 'editor')

    expect(store.members).not.toBe(before) // 新引用：浅 watch 才会触发
    expect(store.members.find((m) => m.clientId === 'c_peer')?.role).toBe('editor')
  })

  it('角色没变化时不制造新数组（避免无意义重渲染）', () => {
    const store = useBoardStore()
    store.setMembers([{ clientId: 'c_me', userId: 'u_me', name: '我', color: '#3d5afe', role: 'viewer' }])
    const before = store.members
    store.setMemberRole('c_me', 'viewer')
    expect(store.members).toBe(before)
  })
})
