import { createRect, type RectShape } from '../shapes'
import type { PointerSample, Tool, ToolHost } from './types'

/** 矩形工具：拖拽生成，Shift 约束正方形。 */
export class RectTool implements Tool {
  readonly id = 'rect' as const
  readonly cursor = 'crosshair' as const

  private active: RectShape | null = null
  private start = { x: 0, y: 0 }

  onPointerDown(sample: PointerSample, host: ToolHost): void {
    if (!host.canEdit) return
    this.start = sample.world
    this.active = createRect(sample.world, sample.world, {
      style: { stroke: host.style.stroke, strokeWidth: host.style.strokeWidth },
    })
    host.beginGesture()
    host.setDraft(this.active)
  }

  onPointerMove(sample: PointerSample, host: ToolHost): void {
    const shape = this.active
    if (!shape) return
    let end = sample.world
    if (sample.shiftKey) {
      const dx = end.x - this.start.x
      const dy = end.y - this.start.y
      const size = Math.max(Math.abs(dx), Math.abs(dy))
      end = {
        x: this.start.x + (dx < 0 ? -size : size),
        y: this.start.y + (dy < 0 ? -size : size),
      }
    }
    const next = createRect(this.start, end, {
      id: shape.id,
      style: shape.style,
      author: shape.author,
    })
    next.createdAt = shape.createdAt
    next.rev = shape.rev + 1
    this.active = next
    host.setDraft(next)
  }

  onPointerUp(_sample: PointerSample, host: ToolHost): void {
    const shape = this.active
    this.active = null
    if (!shape) return
    host.setDraft(null)
    if (shape.geom.w < 2 && shape.geom.h < 2) {
      host.endGesture()
      return
    }
    host.addShape(shape)
    host.endGesture()
    host.afterShapeCommitted('rect')
  }

  onPointerCancel(host: ToolHost): void {
    if (this.active) {
      host.setDraft(null)
      host.endGesture()
    }
    this.active = null
  }

  onDeactivate(host: ToolHost): void {
    this.onPointerCancel(host)
  }
}
