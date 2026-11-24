import { describe, expect, it } from 'vitest'
import { shouldKeepPoint, thinPoints, traceSmoothPath, pointsBounds, pressureOf } from '@/canvas/smoothing'
import type { InkPoint } from '@/canvas/smoothing'

describe('点抽稀', () => {
  it('第一个点永远保留', () => {
    expect(shouldKeepPoint(undefined, [5, 5], 10)).toBe(true)
  })

  it('距离小于阈值丢弃，大于等于保留', () => {
    expect(shouldKeepPoint([0, 0], [1, 1], 10)).toBe(false)
    expect(shouldKeepPoint([0, 0], [10, 0], 10)).toBe(true)
    expect(shouldKeepPoint([0, 0], [9.99, 0], 10)).toBe(false)
  })

  it('thinPoints 把密集采样压到阈值以上', () => {
    const dense: InkPoint[] = []
    for (let i = 0; i < 1000; i += 1) dense.push([i * 0.1, 0])
    const thin = thinPoints(dense, 5)
    // 100 单位长度 / 5 阈值 ≈ 20 个点
    expect(thin.length).toBeGreaterThan(15)
    expect(thin.length).toBeLessThan(25)
    expect(thin[0]).toEqual([0, 0])
  })

  it('压感缺失时回落到 0.5', () => {
    expect(pressureOf([1, 2])).toBe(0.5)
    expect(pressureOf([1, 2, 0.82])).toBe(0.82)
    expect(pressureOf([1, 2, 0])).toBe(0)
  })

  it('pointsBounds 处理空数组与单点', () => {
    expect(pointsBounds([])).toEqual({ minX: 0, minY: 0, maxX: 0, maxY: 0 })
    expect(pointsBounds([[7, -3]])).toEqual({ minX: 7, minY: -3, maxX: 7, maxY: -3 })
  })
})

describe('贝塞尔平滑', () => {
  interface Call {
    op: string
    args: number[]
  }

  function recorder(): { sink: Call[]; api: Parameters<typeof traceSmoothPath>[0] } {
    const sink: Call[] = []
    return {
      sink,
      api: {
        moveTo: (x, y) => sink.push({ op: 'move', args: [x, y] }),
        lineTo: (x, y) => sink.push({ op: 'line', args: [x, y] }),
        quadraticCurveTo: (cx, cy, x, y) => sink.push({ op: 'quad', args: [cx, cy, x, y] }),
        closePath: () => sink.push({ op: 'close', args: [] }),
      },
    }
  }

  it('单点退化为极短线段（点一下也落墨）', () => {
    const { sink, api } = recorder()
    traceSmoothPath(api, [[10, 10]])
    expect(sink[0]?.op).toBe('move')
    expect(sink[1]?.op).toBe('line')
  })

  it('两点画直线', () => {
    const { sink, api } = recorder()
    traceSmoothPath(api, [
      [0, 0],
      [10, 10],
    ])
    expect(sink.map((c) => c.op)).toEqual(['move', 'line'])
  })

  it('三点以上用二次贝塞尔，且控制点为中间采样点', () => {
    const { sink, api } = recorder()
    traceSmoothPath(api, [
      [0, 0],
      [10, 10],
      [20, 0],
    ])
    expect(sink[0]?.op).toBe('move')
    expect(sink.some((c) => c.op === 'quad')).toBe(true)
    const quads = sink.filter((c) => c.op === 'quad')
    // 第一个 quad 的控制点必须是 P1
    expect(quads[0]?.args[0]).toBe(10)
    expect(quads[0]?.args[1]).toBe(10)
  })

  it('closed 时闭合路径', () => {
    const { sink, api } = recorder()
    traceSmoothPath(
      api,
      [
        [0, 0],
        [10, 0],
        [10, 10],
      ],
      true,
    )
    expect(sink[sink.length - 1]?.op).toBe('close')
  })

  it('空点集安全返回', () => {
    const { sink, api } = recorder()
    traceSmoothPath(api, [])
    expect(sink).toHaveLength(0)
  })
})
