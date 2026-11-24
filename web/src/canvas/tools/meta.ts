import type { ToolId } from './types'

export const DRAW_TOOLS: readonly ToolId[] = ['pen', 'rect', 'arrow', 'note']

/**
 * 画完一个图形后自动回到选择态（UX-SPEC §7.4）。
 * 橡皮保持橡皮（连续擦除是主要用法），选择工具本身就是选择。
 */
export function autoReturnTool(tool: ToolId): ToolId {
  return tool === 'pen' || tool === 'rect' || tool === 'arrow' || tool === 'note' ? 'select' : tool
}
