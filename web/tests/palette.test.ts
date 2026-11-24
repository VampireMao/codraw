import { describe, expect, it } from 'vitest'
import { INK_COLORS, INK_PALETTE, DEFAULT_STROKE } from '@/canvas/shapes'

/**
 * 颜色相关的契约约束。
 *
 * 服务端按 PROTOCOL §5.3 校验 `style.stroke` 必须是 `#rrggbb` 小写，
 * 格式错误会直接拒掉整条 op。这些断言把该约束固化下来，
 * 避免有人往调色板里加一个 `#FFF` 或大写色值导致画不上。
 */
describe('画笔调色板', () => {
  it('所有色值都是 #rrggbb 小写（服务端硬性要求）', () => {
    for (const color of INK_COLORS) {
      expect(color, `非法色值: ${color}`).toMatch(/^#[0-9a-f]{6}$/)
    }
  })

  it('调色板与服务端头像色一致（8 色）', () => {
    expect(INK_PALETTE).toHaveLength(8)
    // 与 backend/internal/ws/ws.go 的 palette 保持同序同值
    expect([...INK_PALETTE]).toEqual([
      '#3d5afe',
      '#e8590c',
      '#12b886',
      '#7048e8',
      '#e64980',
      '#0ca678',
      '#f59f00',
      '#1c7ed6',
    ])
  })

  it('默认墨色在调色板选项里且可被选中', () => {
    expect(INK_COLORS[0]).toBe(DEFAULT_STROKE)
    expect(DEFAULT_STROKE).toMatch(/^#[0-9a-f]{6}$/)
  })

  it('无重复色值', () => {
    expect(new Set(INK_COLORS).size).toBe(INK_COLORS.length)
  })
})
