import { describe, expect, it, vi } from 'vitest'
import { History } from '@/canvas/history'
import { createRect } from '@/canvas/shapes'

describe('History 撤销栈', () => {
  it('空栈时 undo/redo 返回 false 且不抛异常', () => {
    const h = new History()
    expect(h.canUndo).toBe(false)
    expect(h.canRedo).toBe(false)
    expect(h.undo()).toBe(false)
    expect(h.redo()).toBe(false)
  })

  it('undo/redo 对称执行闭包', () => {
    const h = new History()
    const undo = vi.fn()
    const redo = vi.fn()
    h.push({ label: 't', undo, redo })

    expect(h.canUndo).toBe(true)
    expect(h.undo()).toBe(true)
    expect(undo).toHaveBeenCalledOnce()
    expect(h.canUndo).toBe(false)
    expect(h.canRedo).toBe(true)

    expect(h.redo()).toBe(true)
    expect(redo).toHaveBeenCalledOnce()
    expect(h.canUndo).toBe(true)
    expect(h.canRedo).toBe(false)
  })

  it('新操作清空 redo 栈', () => {
    const h = new History()
    h.push({ label: 'a', undo: vi.fn(), redo: vi.fn() })
    h.undo()
    expect(h.canRedo).toBe(true)
    h.push({ label: 'b', undo: vi.fn(), redo: vi.fn() })
    expect(h.canRedo).toBe(false)
  })

  it('超过上限时丢弃最旧记录', () => {
    const h = new History(3)
    for (let i = 0; i < 5; i += 1) h.push({ label: `${i}`, undo: vi.fn(), redo: vi.fn() })
    expect(h.size).toBe(3)
  })

  it('onChange 在 push/undo/redo/clear 时触发，可退订', () => {
    const h = new History()
    const listener = vi.fn()
    const off = h.onChange(listener)

    h.push({ label: 'a', undo: vi.fn(), redo: vi.fn() })
    expect(listener).toHaveBeenCalledTimes(1)
    h.undo()
    expect(listener).toHaveBeenCalledTimes(2)
    h.redo()
    expect(listener).toHaveBeenCalledTimes(3)
    h.clear()
    expect(listener).toHaveBeenCalledTimes(4)

    off()
    h.push({ label: 'b', undo: vi.fn(), redo: vi.fn() })
    expect(listener).toHaveBeenCalledTimes(4)
  })

  it('撤销真实场景：新建 → 删除 → 撤销恢复', () => {
    const h = new History()
    let scene = [] as ReturnType<typeof createRect>[]
    const rect = createRect({ x: 0, y: 0 }, { x: 10, y: 10 })

    const before = [...scene]
    scene = [...scene, rect]
    const after = [...scene]
    h.push({
      label: '新建',
      undo: () => {
        scene = before
      },
      redo: () => {
        scene = after
      },
    })

    expect(scene).toHaveLength(1)
    h.undo()
    expect(scene).toHaveLength(0)
    h.redo()
    expect(scene).toHaveLength(1)
    expect(scene[0]?.id).toBe(rect.id)
  })
})
