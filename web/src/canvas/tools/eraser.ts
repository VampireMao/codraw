import { hitTest, type Shape } from '../shapes'
import { pickShape, type PointerSample, type Tool, type ToolHost } from './types'

/**
 * 橡皮：整条擦除（UX-SPEC §7.7 明确「不做像素擦除」）。
 * 拖动过程中连续命中，被擦对象立刻从视图移除；松手合并为一笔撤销记录。
 */
export class EraserTool implements Tool {
  readonly id = 'eraser' as const
  readonly cursor = 'crosshair' as const

  private active = false
  private removed = new Set<string>()
  private lastErased: Shape[] = []

  onPointerDown(sample: PointerSample, host: ToolHost): void {
    if (!host.canEdit) return
    this.active = true
    this.removed.clear()
    this.lastErased = []
    host.beginGesture()
    this.eraseAt(sample, host)
  }

  onPointerMove(sample: PointerSample, host: ToolHost): void {
    if (!this.active) return
    this.eraseAt(sample, host)
  }

  onPointerUp(_sample: PointerSample, host: ToolHost): void {
    if (!this.active) return
    this.active = false
    host.endGesture()
    if (this.lastErased.length > 0) host.afterShapeCommitted('eraser')
  }

  onPointerCancel(host: ToolHost): void {
    if (this.active) {
      this.active = false
      host.endGesture()
    }
  }

  onDeactivate(host: ToolHost): void {
    this.onPointerCancel(host)
  }

  private eraseAt(sample: PointerSample, host: ToolHost): void {
    const hit = pickShape(
      host.shapes.filter((s) => !this.removed.has(s.id)),
      sample.world,
      host.tolerance(10),
      hitTest,
    )
    if (!hit) return
    this.removed.add(hit.id)
    this.lastErased.push(hit)
    host.removeShapes([hit.id])
  }
}
