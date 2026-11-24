/**
 * 画布性能基准（Node 环境，模拟渲染热路径）。
 *
 * 测量的是「每帧 CPU 侧准备工作的成本」：
 *   1. 视口变换 + 视口裁剪（1000 条笔迹）
 *   2. 命中测试（一次点击要扫全部图形）
 *   3. Path2D 缓存的键查找（模拟缓存命中）
 *   4. 序列化（1000 shape → wire）
 * 真实绘制由浏览器 GPU 负责，这里测的是我们代码占的那部分时间。
 *
 * 运行：npm run bench
 */
import { performance } from 'node:perf_hooks'
import { Viewport } from '../src/canvas/viewport'
import { createFreehand, hitTest, toWire, sceneBounds } from '../src/canvas/shapes'
import { shapeBounds } from '../src/canvas/shapes'
import { expandRect, rectsIntersect } from '../src/canvas/geometry'

const STROKE_COUNT = 1000
const POINTS_PER_STROKE = 60
const FRAMES = 600

function buildStrokes(count: number) {
  const shapes = []
  for (let i = 0; i < count; i += 1) {
    const points = []
    const ox = (i % 40) * 90
    const oy = Math.floor(i / 40) * 70
    for (let p = 0; p < POINTS_PER_STROKE; p += 1) {
      points.push([ox + p * 1.4 + Math.sin(p * 0.5) * 12, oy + Math.cos(p * 0.4) * 18])
    }
    shapes.push(createFreehand(points))
  }
  return shapes
}

function percentile(sorted: number[], p: number): number {
  const index = Math.min(sorted.length - 1, Math.max(0, Math.ceil((p / 100) * sorted.length) - 1))
  return sorted[index] ?? 0
}

function bench(label: string, fn: () => void, iterations: number): number[] {
  // 预热，避免 JIT 首轮把均值拉高
  for (let i = 0; i < 30; i += 1) fn()
  const samples: number[] = []
  for (let i = 0; i < iterations; i += 1) {
    const t0 = performance.now()
    fn()
    samples.push(performance.now() - t0)
  }
  samples.sort((a, b) => a - b)
  const mean = samples.reduce((s, v) => s + v, 0) / samples.length
  console.log(
    `${label.padEnd(38)} mean ${mean.toFixed(3)}ms  p50 ${percentile(samples, 50).toFixed(3)}ms  ` +
      `p95 ${percentile(samples, 95).toFixed(3)}ms  p99 ${percentile(samples, 99).toFixed(3)}ms`,
  )
  return samples
}

console.log(`\n=== CoDraw 画布基准 · ${STROKE_COUNT} 条笔迹 × ${POINTS_PER_STROKE} 点 ===\n`)

const shapes = buildStrokes(STROKE_COUNT)
const totalPoints = shapes.reduce((sum, s) => sum + s.points.length, 0)
console.log(`图形 ${shapes.length} 个，点 ${totalPoints.toLocaleString()} 个\n`)

const vp = new Viewport()
vp.resize(1440, 900)
vp.reset()
vp.set({ scale: 1, x: 720, y: 450 })

// 1) 视口变换：每帧对全部图形做 bounds → 转换 → 裁剪（真实热路径）
bench(
  'frame: transform + cull 1000 shapes',
  () => {
    const visible = expandRect(vp.visibleWorldRect(), 48)
    let drawn = 0
    for (const shape of shapes) {
      const box = shapeBounds(shape)
      if (!rectsIntersect(box, visible)) continue
      const screen = vp.worldRectToScreen(box)
      if (screen.w > 0) drawn += 1
    }
    if (drawn < 0) throw new Error('impossible')
  },
  FRAMES,
)

// 2) 缩放中：每帧改一次视口 + 全量裁剪
bench(
  'frame: zoom step + cull 1000 shapes',
  () => {
    vp.zoomAt({ x: 700, y: 400 }, 1.004)
    const visible = expandRect(vp.visibleWorldRect(), 48)
    for (const shape of shapes) {
      if (!rectsIntersect(shapeBounds(shape), visible)) continue
    }
  },
  FRAMES,
)

// 3) 命中测试：一次点击（从顶层找）
const clickPoint = { x: 400, y: 300 }
bench(
  'click: hit test worst case (1000)',
  () => {
    for (let i = shapes.length - 1; i >= 0; i -= 1) {
      const shape = shapes[i]
      if (shape && hitTest(shape, clickPoint, 6)) return
    }
  },
  FRAMES,
)

// 4) Path2D 缓存命中查找（Map.get + rev 比较）
const cache = new Map<string, { rev: number }>()
for (const shape of shapes) cache.set(shape.id, { rev: shape.rev })
bench(
  'cache lookup 1000 shapes (path hit)',
  () => {
    for (const shape of shapes) {
      const hit = cache.get(shape.id)
      if (hit && hit.rev !== shape.rev) cache.set(shape.id, { rev: shape.rev })
    }
  },
  FRAMES,
)

// 5) 序列化 1000 shape（重连补齐 / 落盘）
bench(
  'serialize 1000 shapes → wire',
  () => {
    const out = shapes.map(toWire)
    if (out.length !== shapes.length) throw new Error('bad')
  },
  60,
)

// 6) 场景包围盒（适配视图）
bench('sceneBounds 1000 shapes', () => sceneBounds(shapes), 200)

// 7) 撤销快照（浅拷贝数组 + 引用）
bench(
  'undo snapshot 1000 shapes (shallow)',
  () => {
    const snapshot = [...shapes]
    if (snapshot.length !== shapes.length) throw new Error('bad')
  },
  FRAMES,
)

// 8) 撤销快照（深拷贝：freehand 点数组全复制）
bench(
  'undo snapshot 1000 shapes (deep)',
  () => {
    const copy = shapes.map((s) => ({
      ...s,
      geom: { ...s.geom },
      style: { ...s.style },
      points: s.points.map((p) => [p[0], p[1]]),
    }))
    if (copy.length !== shapes.length) throw new Error('bad')
  },
  60,
)

console.log('\n判定：单帧预算 16.67ms（60fps）。上面 frame 行均需 < 2ms 才算留足余量。\n')
