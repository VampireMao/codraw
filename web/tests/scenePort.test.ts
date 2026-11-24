import { describe, expect, it } from 'vitest'
import { applyOpToStore, shapeIdsOf, type SceneStore } from '@/stores/scenePort'
import type { Shape } from '@/canvas/shapes'

/**
 * 这些用例覆盖两个真实线上缺陷（均由人工验收发现）：
 *
 *  1. 服务端广播 delete 时携带完整 shape（shape.deleted=true）而不是顶层 id。
 *     早期实现只读 op.id，导致「别人删掉的图形在我这边不会消失」，
 *     以及「自己擦掉的图形刷新后复活」。
 *  2. append 是逐批到达的，且必须能在已有 shape 上追加点。
 */

function makeStore(initial: Shape[] = []): SceneStore & { shapes: Shape[] } {
  const state = {
    shapes: [...initial],
    cid: 'c_test',
    addShape(s: Shape) {
      state.shapes.push(s)
    },
    addShapes(next: readonly Shape[]) {
      state.shapes.push(...next)
    },
    upsertShape(s: Shape) {
      const i = state.shapes.findIndex((x) => x.id === s.id)
      if (i === -1) state.shapes.push(s)
      else state.shapes[i] = s
    },
    updateShape(s: Shape) {
      state.upsertShape(s)
    },
    removeShapes(ids: readonly string[]) {
      const set = new Set(ids)
      state.shapes = state.shapes.filter((s) => !set.has(s.id))
    },
    markDeleted(id: string) {
      const s = state.shapes.find((x) => x.id === id)
      if (s) {
        s.deleted = true
        s.rev += 1
      }
    },
    setPending(_id: string, _pending: boolean) {},
    setShapes(next: Shape[]) {
      state.shapes = next
    },
    findShape(id: string) {
      return state.shapes.find((s) => s.id === id)
    },
    touch() {},
  }
  return state
}

function wireShape(id: string, deleted = false) {
  return {
    id,
    kind: 'rect',
    geom: { x: 0, y: 0, w: 10, h: 10 },
    style: { stroke: '#16181d', fill: 'none', strokeWidth: 2, opacity: 1, dash: 'solid' },
    data: {},
    text: null,
    deleted,
    rev: 1,
    author: { cid: 'c_other', name: 'other' },
    createdAt: 1,
    updatedAt: 1,
  }
}

describe('delete 的两种线上形态', () => {
  it('客户端形态：只带顶层 id', () => {
    const store = makeStore([wireShape('sh_1') as unknown as Shape])
    applyOpToStore(store, { opId: 'op_1', action: 'delete', id: 'sh_1' })
    expect(store.findShape('sh_1')?.deleted).toBe(true)
  })

  it('服务端广播形态：带完整 shape（这是实际抓到的缺陷）', () => {
    const store = makeStore([wireShape('sh_2') as unknown as Shape])
    applyOpToStore(store, {
      opId: 'op_2',
      action: 'delete',
      shape: wireShape('sh_2', true),
    } as never)
    expect(store.findShape('sh_2')?.deleted).toBe(true)
  })

  it('shapeIdsOf 对两种形态都能取到 id', () => {
    expect(shapeIdsOf({ opId: 'a', action: 'delete', id: 'sh_x' })).toEqual(['sh_x'])
    expect(shapeIdsOf({ opId: 'b', action: 'delete', shape: wireShape('sh_y', true) } as never)).toEqual([
      'sh_y',
    ])
  })

  it('缺少任何 id 时不崩、不误删', () => {
    const store = makeStore([wireShape('sh_3') as unknown as Shape])
    applyOpToStore(store, { opId: 'op_3', action: 'delete' } as never)
    expect(store.findShape('sh_3')?.deleted).toBe(false)
    expect(shapeIdsOf({ opId: 'c', action: 'delete' } as never)).toEqual([])
  })
})

describe('append 增量合并', () => {
  it('把增量点并入 freehand 并推进 rev', () => {
    const base = wireShape('sh_fh') as unknown as Shape
    const freehand = {
      ...base,
      type: 'freehand',
      points: [
        [0, 0],
        [10, 10],
      ],
    } as unknown as Shape
    const store = makeStore([freehand])
    const revBefore = freehand.rev

    applyOpToStore(store, {
      opId: 'op_ap',
      action: 'append',
      id: 'sh_fh',
      points: [
        [20, 20],
        [30, 30],
      ],
    })

    const after = store.findShape('sh_fh') as unknown as { points: number[][]; rev: number }
    expect(after.points).toHaveLength(4)
    expect(after.points[3]).toEqual([30, 30])
    // appendPoints 会就地 mutate 同一个对象，所以要在调用前记录旧 rev
    expect(after.rev).toBe(revBefore + 1)
  })

  it('目标不存在时静默忽略（append 先于 create 到达的容错）', () => {
    const store = makeStore([])
    expect(() =>
      applyOpToStore(store, { opId: 'op_x', action: 'append', id: 'sh_missing', points: [[1, 1]] }),
    ).not.toThrow()
    expect(store.findShape('sh_missing')).toBeUndefined()
  })

  it('非法坐标被丢弃', () => {
    const base = wireShape('sh_bad') as unknown as Shape
    const freehand = { ...base, type: 'freehand', points: [[0, 0]] } as unknown as Shape
    const store = makeStore([freehand])

    applyOpToStore(store, {
      opId: 'op_bad',
      action: 'append',
      id: 'sh_bad',
      points: [
        [Number.NaN, 1],
        [1, Number.POSITIVE_INFINITY],
        [5, 5],
      ] as number[][],
    })

    const after = store.findShape('sh_bad') as unknown as { points: number[][] }
    expect(after.points).toHaveLength(2)
    expect(after.points[1]).toEqual([5, 5])
  })
})
