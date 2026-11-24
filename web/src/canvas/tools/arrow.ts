import { createArrow, type ArrowShape } from '../shapes'
import type { PointerSample, Tool, ToolHost } from './types'

/** 箭头工具：拖拽画线，Shift 吸附到 15° 网格。 */
export class ArrowTool implements Tool {
  readonly id = 'arrow' as const
  readonly cursor = 'crosshair' as const

  private active: ArrowShape | null = null
  private start = { x: 0, y: 0 }

  onPointerDown(sample: PointerSample, host: ToolHost): void {
    if (!host.canEdit) return
    this.start = sample.world
    this.active = createArrow(sample.world, sample.world, {
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
      const angle = Math.atan2(dy, dx)
      const step = Math.PI / 12
      const snapped = Math.round(angle / step) * step
      const len = Math.hypot(dx, dy)
      end = { x: this.start.x + Math.cos(snapped) * len, y: this.start.y + Math.sin(snapped) * len }
    }
    const next = createArrow(this.start, end, {
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
    if (Math.hypot(shape.x2 - shape.x1, shape.y2 - shape.y1) < 6) {
      host.endGesture()
      return
    }
    host.addShape(shape)
    host.endGesture()
    host.afterShapeCommitted('arrow')
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
