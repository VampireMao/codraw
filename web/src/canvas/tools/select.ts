import { hitTest, moveShape } from '../shapes'
import { normalizeRect, rectFromPoints, type Rect } from '../geometry'
import { pickShape, type PointerSample, type Tool, type ToolHost } from './types'

type Mode = 'idle' | 'press' | 'move' | 'marquee'

/**
 * 选择工具：点选（Shift 多选）、拖动、框选。
 * 拖动时对整组图形做同一位移，松手才提交历史（beginGesture/endGesture）。
 */
export class SelectTool implements Tool {
  readonly id = 'select' as const
  readonly cursor = 'default' as const

  private mode: Mode = 'idle'
  private startWorld = { x: 0, y: 0 }
  private lastWorld = { x: 0, y: 0 }
  private moved = false
  private dragIds: string[] = []

  onPointerDown(sample: PointerSample, host: ToolHost): void {
    if (!host.canEdit) return
    this.startWorld = sample.world
    this.lastWorld = sample.world
    this.moved = false
    this.mode = 'press'

    const hit = pickShape(host.shapes, sample.world, host.tolerance(6), hitTest)

    if (!hit) {
      if (!sample.shiftKey) host.clearSelection()
      this.mode = 'marquee'
      host.setMarquee({ x: sample.world.x, y: sample.world.y, w: 0, h: 0 })
      return
    }

    const alreadySelected = host.selection.has(hit.id)
    if (sample.shiftKey) {
      const next = new Set(host.selection)
      if (alreadySelected) next.delete(hit.id)
      else next.add(hit.id)
      host.setSelection([...next])
      if (!next.has(hit.id)) {
        this.mode = 'idle'
        return
      }
    } else if (!alreadySelected) {
      host.setSelection([hit.id])
    }

    this.dragIds = [...host.selection]
    host.beginGesture()
  }

  onPointerMove(sample: PointerSample, host: ToolHost): void {
    if (this.mode === 'idle') return

    if (this.mode === 'press') {
      const dx = sample.world.x - this.startWorld.x
      const dy = sample.world.y - this.startWorld.y
      if (Math.hypot(dx, dy) > host.tolerance(2)) {
        this.mode = this.dragIds.length > 0 ? 'move' : 'marquee'
      }
    }

    if (this.mode === 'move') {
      const dx = sample.world.x - this.lastWorld.x
      const dy = sample.world.y - this.lastWorld.y
      if (dx !== 0 || dy !== 0) {
        const moving = new Set(this.dragIds)
        for (const shape of host.shapes) {
          if (!moving.has(shape.id)) continue
          moveShape(shape, dx, dy)
          host.touchShape(shape)
        }
        this.lastWorld = sample.world
        this.moved = true
      }
      return
    }

    if (this.mode === 'marquee') {
      const rect: Rect = normalizeRect(rectFromPoints(this.startWorld, sample.world))
      host.setMarquee(rect)
      const inside = host.shapes.filter((shape) => {
        if (shape.deleted) return false
        const g = shape.geom
        return (
          g.x >= rect.x - 1 &&
          g.y >= rect.y - 1 &&
          g.x + g.w <= rect.x + rect.w + 1 &&
          g.y + g.h <= rect.y + rect.h + 1
        )
      })
      host.setSelection(inside.map((s) => s.id))
      this.moved = true
    }
  }

  onPointerUp(sample: PointerSample, host: ToolHost): void {
    void sample
    if (this.mode === 'marquee') {
      host.setMarquee(null)
      if (!this.moved) host.clearSelection()
    }
    if (this.dragIds.length > 0) host.endGesture()
    this.dragIds = []
    this.moved = false
    this.mode = 'idle'
  }

  onPointerCancel(host: ToolHost): void {
    host.setMarquee(null)
    if (this.dragIds.length > 0) host.endGesture()
    this.dragIds = []
    this.mode = 'idle'
  }

  onDeactivate(host: ToolHost): void {
    host.setMarquee(null)
    if (this.dragIds.length > 0) host.endGesture()
    this.dragIds = []
    this.mode = 'idle'
  }
}
