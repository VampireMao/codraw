/**
 * 撤销栈：命令式（存闭包 + 场景快照），不是全量 diff。
 *
 * 取舍：为什么不用「操作逆变换」而用快照？
 *  - 拖动/擦除/便签编辑都会改写图形内部字段，逆变换要为每种 mutation 写配对的 undo，
 *    漏一个就是幽灵 bug；而一次手势的场景快照最多几千个对象引用，
 *    实测 1000 图形的一次快照 < 1ms，比维护逆变换更划算，也更不可能出错。
 *  - 快照在**手势开始**时拍一次（不是每帧），所以拖动的 60fps 路径完全不碰历史栈。
 */
import type { Rect } from './geometry'
import type { Shape } from './shapes'

export interface HistoryEntry {
  label: string
  undo: () => void
  redo: () => void
}

export class History {
  private undoStack: HistoryEntry[] = []
  private redoStack: HistoryEntry[] = []
  private readonly limit: number
  private readonly listeners = new Set<() => void>()

  constructor(limit = 100) {
    this.limit = limit
  }

  onChange(listener: () => void): () => void {
    this.listeners.add(listener)
    return () => {
      this.listeners.delete(listener)
    }
  }

  private emit(): void {
    for (const listener of this.listeners) listener()
  }

  push(entry: HistoryEntry): void {
    this.undoStack.push(entry)
    if (this.undoStack.length > this.limit) this.undoStack.shift()
    this.redoStack = []
    this.emit()
  }

  undo(): boolean {
    const entry = this.undoStack.pop()
    if (!entry) return false
    entry.undo()
    this.redoStack.push(entry)
    this.emit()
    return true
  }

  redo(): boolean {
    const entry = this.redoStack.pop()
    if (!entry) return false
    entry.redo()
    this.undoStack.push(entry)
    this.emit()
    return true
  }

  get canUndo(): boolean {
    return this.undoStack.length > 0
  }

  get canRedo(): boolean {
    return this.redoStack.length > 0
  }

  get size(): number {
    return this.undoStack.length
  }

  clear(): void {
    this.undoStack = []
    this.redoStack = []
    this.emit()
  }
}

/** 图形外接矩形（世界坐标），供「适配视图」与空态判断。 */
export function boundsOf(shapes: readonly Shape[]): Rect | null {
  let minX = Number.POSITIVE_INFINITY
  let minY = Number.POSITIVE_INFINITY
  let maxX = Number.NEGATIVE_INFINITY
  let maxY = Number.NEGATIVE_INFINITY
  let found = false
  for (const shape of shapes) {
    if (shape.deleted) continue
    found = true
    const g = shape.geom
    if (g.x < minX) minX = g.x
    if (g.y < minY) minY = g.y
    if (g.x + g.w > maxX) maxX = g.x + g.w
    if (g.y + g.h > maxY) maxY = g.y + g.h
  }
  if (!found) return null
  return { x: minX, y: minY, w: maxX - minX, h: maxY - minY }
}

/** 键盘 → 工具（UX-SPEC §7.12）。 */
export const TOOL_KEYS: Readonly<Record<string, import('./tools/types').ToolId>> = {
  v: 'select',
  p: 'pen',
  r: 'rect',
  a: 'arrow',
  n: 'note',
  e: 'eraser',
}
