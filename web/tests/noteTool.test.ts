import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { NoteTool } from '@/canvas/tools/note'
import type { PointerSample, ToolHost } from '@/canvas/tools/types'

function sample(): PointerSample {
  return {
    world: { x: 100, y: 100 },
    screen: { x: 100, y: 100 },
    pressure: 0.5,
    pointerType: 'mouse',
    shiftKey: false,
    altKey: false,
    timeStamp: 0,
  }
}

function host() {
  return {
    shapes: [],
    selection: new Set<string>(),
    canEdit: true,
    tolerance: () => 1,
    beginGesture: vi.fn(),
    addShape: vi.fn(),
    setSelection: vi.fn(),
    endGesture: vi.fn(),
    openNoteEditor: vi.fn(),
    afterShapeCommitted: vi.fn(),
  } as unknown as ToolHost & {
    addShape: ReturnType<typeof vi.fn>
    openNoteEditor: ReturnType<typeof vi.fn>
  }
}

beforeEach(() => {
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('便签工具：编辑器延后打开', () => {
  it('pointerdown 只创建便签，编辑器在点击序列结束后才打开', () => {
    const h = host()
    const tool = new NoteTool()

    tool.onPointerDown(sample(), h)

    expect(h.addShape).toHaveBeenCalledTimes(1)
    expect(h.openNoteEditor).not.toHaveBeenCalled()

    vi.runAllTimers()

    expect(h.openNoteEditor).toHaveBeenCalledTimes(1)
    expect(h.openNoteEditor.mock.calls[0]?.[0]?.type).toBe('text')
  })
})
