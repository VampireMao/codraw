import { createNote, type TextShape } from '../shapes'
import { pickShape } from './types'
import { hitTest } from '../shapes'
import type { PointerSample, Tool, ToolHost } from './types'

/**
 * 便签工具：点一下即创建（位置以点击点为左上角），随后弹出 textarea 浮层。
 * 双击已有便签/文字图形 → 直接编辑（不改类型）。
 */
export class NoteTool implements Tool {
  readonly id = 'note' as const
  readonly cursor = 'text' as const

  /** 明确由「双击画布」触发时置 true：此时不自动切回选择工具（用户没选便签工具） */
  constructor(private readonly transient = false) {}

  onPointerDown(sample: PointerSample, host: ToolHost): void {
    if (!host.canEdit) return
    const hit = pickShape(host.shapes, sample.world, host.tolerance(6), hitTest)
    if (hit && hit.type === 'text') {
      host.openNoteEditor(hit)
      return
    }
    const shape: TextShape = createNote({ x: sample.world.x - 8, y: sample.world.y - 8 })
    host.beginGesture()
    host.addShape(shape)
    host.setSelection([shape.id])
    host.endGesture()
    host.openNoteEditor(shape)
    if (!this.transient) host.afterShapeCommitted('note')
  }

  onPointerMove(): void {
    /* noop */
  }

  onPointerUp(): void {
    /* 单击即完成，不做拖拽 */
  }
}
