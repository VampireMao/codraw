import { describe, expect, it } from 'vitest'
import { canRoleEdit, type Role } from '@/net/types'

/**
 * `canEdit` 的判定口径（PERMISSIONS §1）：
 *
 *   canEdit = owner || editor
 *
 * 这条规则看起来简单，但历史上正是因为前端写成了 `role === 'editor'`，
 * 房主创建房间后自己反而不能画（第一个连接的 role 是 owner）。
 * 所以这里把三种角色逐一钉死。
 */
describe('canRoleEdit：owner 隐含 editor 能力', () => {
  it('owner 可以编辑', () => {
    expect(canRoleEdit('owner')).toBe(true)
  })

  it('editor 可以编辑', () => {
    expect(canRoleEdit('editor')).toBe(true)
  })

  it('viewer 不可以编辑', () => {
    expect(canRoleEdit('viewer')).toBe(false)
  })

  it('所有角色都在闭集内（防止将来加了新角色忘了判断）', () => {
    const roles: Role[] = ['owner', 'editor', 'viewer']
    expect(roles.map(canRoleEdit)).toEqual([true, true, false])
  })
  it('旧实现（role === "editor"）在 owner 下会误判——这条是回归断言', () => {
    const legacy = (role: Role): boolean => role === 'editor'
    expect(legacy('owner')).toBe(false)
    expect(canRoleEdit('owner')).toBe(true)
  })
})
