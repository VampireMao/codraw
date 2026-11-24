import type { ToolId, Tool } from './types'
import { SelectTool } from './select'
import { PenTool } from './pen'
import { RectTool } from './rect'
import { ArrowTool } from './arrow'
import { NoteTool } from './note'
import { EraserTool } from './eraser'

export type { Tool, ToolHost, ToolId, PointerSample, ToolCursor, ToolStyleSettings } from './types'
export { autoReturnTool, DRAW_TOOLS } from './meta'

/** 工具工厂：每次切换工具都新建实例，避免残留状态跨工具泄漏。 */
export function createTool(id: ToolId): Tool {
  switch (id) {
    case 'select':
      return new SelectTool()
    case 'pen':
      return new PenTool()
    case 'rect':
      return new RectTool()
    case 'arrow':
      return new ArrowTool()
    case 'note':
      return new NoteTool()
    case 'eraser':
      return new EraserTool()
    default: {
      const exhaustive: never = id
      throw new Error(`未知工具：${String(exhaustive)}`)
    }
  }
}
