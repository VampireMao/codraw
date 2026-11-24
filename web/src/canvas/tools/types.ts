import type { Point, Rect } from '../geometry'
import type { Shape } from '../shapes'

export type ToolId = 'select' | 'pen' | 'rect' | 'arrow' | 'note' | 'eraser'

export type ToolCursor = 'default' | 'crosshair' | 'grab' | 'grabbing' | 'move' | 'text' | 'not-allowed'

export interface PointerSample {
  /** 世界坐标 */
  world: Point
  /** CSS 像素坐标（相对画布容器左上角） */
  screen: Point
  pressure: number
  pointerType: 'mouse' | 'pen' | 'touch'
  shiftKey: boolean
  altKey: boolean
  /** 事件原始时间戳，用于速度估算 */
  timeStamp: number
}

export interface ToolStyleSettings {
  stroke: string
  strokeWidth: number
}

/**
 * 工具与宿主之间的契约。
 *
 * 刻意不让工具直接操作 store：工具只描述「我要新增/挪动/删除哪些图形」，
 * 由宿主决定如何落库（本地、或 Phase B 的乐观更新 + op 队列）。
 */
export interface ToolHost {
  readonly shapes: readonly Shape[]
  readonly selection: ReadonlySet<string>
  readonly style: ToolStyleSettings
  readonly canEdit: boolean

  addShape(shape: Shape): void
  /** 就地修改后的通知（rev 由修改方推进） */
  touchShape(shape: Shape): void
  /**
   * 上报落笔过程中的增量点（协议 append action）。
   * 用于让协作者实时看到线条延伸，而不是等松手才出现。
   */
  appendPoints(id: string, points: readonly (readonly number[])[]): void
  /**
   * 落笔即建形：本地入 store 并发送 create op。
   * 让远端在第一条 append 之前就拥有这个 id（否则 append 找不到目标被丢弃）。
   */
  createDraftShape(shape: Shape): void
  /** 删除图形并同步到服务端（橡皮、删除键） */
  removeShapes(ids: readonly string[]): void
  /** 仅本地移除，不发 op（调用方自己负责发 delete） */
  removeShapesLocal(ids: readonly string[]): void
  /** 替换整个图形数组（撤销/重做等） */
  replaceShapes(next: readonly Shape[]): void

  setSelection(ids: readonly string[]): void
  clearSelection(): void

  setDraft(shape: Shape | null): void
  setMarquee(rect: Rect | null): void

  /** 屏幕像素 → 世界长度（命中容差用） */
  tolerance(px: number): number

  /** 当前时间戳（毫秒）。注入而非直接 Date.now()，便于测试节流逻辑。 */
  now(): number

  /** 手势开始：宿主拍快照，供撤销栈使用 */
  beginGesture(): void
  /** 手势结束：宿主比对快照，有净变更则入撤销栈 */
  endGesture(): void

  openNoteEditor(shape: Shape & { type: 'text' }): void
  /** 一个图形画完后回调（用于自动回到选择态） */
  afterShapeCommitted(tool: ToolId): void
}

export interface Tool {
  readonly id: ToolId
  readonly cursor: ToolCursor
  onPointerDown(sample: PointerSample, host: ToolHost): void
  onPointerMove(sample: PointerSample, host: ToolHost): void
  onPointerUp(sample: PointerSample, host: ToolHost): void
  onPointerCancel?(host: ToolHost): void
  onDoubleClick?(sample: PointerSample, host: ToolHost): void
  /** 工具被切换走时清理草稿/临时状态 */
  onDeactivate?(host: ToolHost): void
}

/** 命中测试：从后往前（顶层优先），返回最上面的图形。 */
export function pickShape(
  shapes: readonly Shape[],
  point: Point,
  tolWorld: number,
  test: (shape: Shape, p: Point, tol: number) => boolean,
): Shape | null {
  for (let i = shapes.length - 1; i >= 0; i -= 1) {
    const shape = shapes[i]
    if (!shape || shape.deleted) continue
    if (test(shape, point, tolWorld)) return shape
  }
  return null
}
