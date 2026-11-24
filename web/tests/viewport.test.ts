import { describe, expect, it } from 'vitest'
import { Viewport, MIN_SCALE, MAX_SCALE, isMobileWidth } from '@/canvas/viewport'

const screen = (x: number, y: number) => ({ x, y })

function makeViewport(w = 1000, h = 700): Viewport {
  const vp = new Viewport()
  vp.resize(w, h)
  vp.reset()
  return vp
}

describe('Viewport 坐标变换', () => {
  it('世界↔屏幕往返无损', () => {
    const vp = makeViewport()
    vp.set({ scale: 2.5, x: -143.7, y: 88.25 })
    const points = [
      { x: 0, y: 0 },
      { x: 120.5, y: -30 },
      { x: -999.25, y: 4321.125 },
    ]
    for (const p of points) {
      const round = vp.screenToWorld(vp.worldToScreen(p))
      expect(round.x).toBeCloseTo(p.x, 9)
      expect(round.y).toBeCloseTo(p.y, 9)
    }
  })

  it('reset 让世界原点落在视口中心', () => {
    const vp = makeViewport(800, 600)
    vp.reset()
    const center = vp.worldToScreen({ x: 0, y: 0 })
    expect(center.x).toBeCloseTo(400, 9)
    expect(center.y).toBeCloseTo(300, 9)
    expect(vp.scale).toBe(1)
  })

  it('锚点缩放时锚点下的世界坐标保持不动', () => {
    const vp = makeViewport()
    vp.set({ scale: 1, x: 50, y: 30 })
    const anchor = screen(240, 180)
    const before = vp.screenToWorld(anchor)

    vp.zoomAt(anchor, 3.2)
    const after = vp.screenToWorld(anchor)

    expect(after.x).toBeCloseTo(before.x, 9)
    expect(after.y).toBeCloseTo(before.y, 9)
    expect(vp.scale).toBeCloseTo(3.2, 9)
  })

  it('缩放被钳制在 10%–800%', () => {
    const vp = makeViewport()
    vp.zoomAt(screen(0, 0), 0.001)
    expect(vp.scale).toBe(MIN_SCALE)
    vp.zoomAt(screen(0, 0), 100)
    expect(vp.scale).toBe(MAX_SCALE)
  })

  it('平移不改变缩放，网格与图形同步移动', () => {
    const vp = makeViewport()
    vp.set({ scale: 1.5, x: 0, y: 0 })
    const world = { x: 100, y: 100 }
    const before = vp.worldToScreen(world)
    vp.panBy(40, -20)
    const after = vp.worldToScreen(world)
    expect(after.x - before.x).toBeCloseTo(40, 9)
    expect(after.y - before.y).toBeCloseTo(-20, 9)
    expect(vp.scale).toBeCloseTo(1.5, 9)
  })

  it('屏幕长度换算为世界长度时随缩放倒数变化', () => {
    const vp = makeViewport()
    vp.set({ scale: 4 })
    expect(vp.screenToWorldLength(8)).toBeCloseTo(2, 9)
    vp.set({ scale: 0.5 })
    expect(vp.screenToWorldLength(8)).toBeCloseTo(16, 9)
  })

  it('fit 把内容居中且不超出边界', () => {
    const vp = makeViewport(1200, 800)
    // 远远偏离原点的内容
    vp.fit({ x: 5000, y: -3000, w: 400, h: 200 }, 64)
    const center = vp.worldToScreen({ x: 5200, y: -2900 })
    expect(center.x).toBeCloseTo(600, 6)
    expect(center.y).toBeCloseTo(400, 6)
    // 400 宽在 1200-128 可用宽度下，缩放应 > 1
    expect(vp.scale).toBeGreaterThan(1)
    expect(vp.scale).toBeLessThanOrEqual(MAX_SCALE)
  })

  it('fit 空内容回到原点 100%', () => {
    const vp = makeViewport(900, 600)
    vp.set({ scale: 3, x: 120, y: 240 })
    vp.fit(null)
    expect(vp.scale).toBe(1)
    const origin = vp.worldToScreen({ x: 0, y: 0 })
    expect(origin.x).toBeCloseTo(450, 9)
    expect(origin.y).toBeCloseTo(300, 9)
  })

  it('resize 保持视口中心所见内容不变', () => {
    const vp = makeViewport(800, 600)
    vp.set({ scale: 2, x: -100, y: -60 })
    const before = vp.screenToWorld(screen(400, 300))
    vp.resize(1200, 900)
    const after = vp.screenToWorld(screen(600, 450))
    expect(after.x).toBeCloseTo(before.x, 6)
    expect(after.y).toBeCloseTo(before.y, 6)
  })

  it('visibleWorldRect 覆盖整个视口', () => {
    const vp = makeViewport(1000, 500)
    vp.set({ scale: 0.5, x: 100, y: 50 })
    const rect = vp.visibleWorldRect()
    expect(rect.x).toBeCloseTo(-200, 9)
    expect(rect.y).toBeCloseTo(-100, 9)
    expect(rect.w).toBeCloseTo(2000, 9)
    expect(rect.h).toBeCloseTo(1000, 9)
  })

  it('断点判断与 UX-SPEC 一致（768px）', () => {
    expect(isMobileWidth(375)).toBe(true)
    expect(isMobileWidth(767)).toBe(true)
    expect(isMobileWidth(768)).toBe(false)
    expect(isMobileWidth(1440)).toBe(false)
  })
})
