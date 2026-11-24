/**
 * 画布性能自测。
 *
 * 为什么不测 rAF 帧率：浏览器窗口不可见 / 离屏时，rAF 会被限流到 1Hz，
 * 测出的「帧率」是浏览器节流的结果，不是我们的性能。CI 与自动化环境尤其如此。
 *
 * 正确指标：**一次完整重绘（网格 + 全部可见图形 + overlay）需要多少毫秒**。
 * 60fps 的预算是 16.67ms/帧，所以「重绘耗时」直接决定还剩多少余量。
 * 同时给出「每帧绘制的图形数」，用于估算某台设备能撑多少笔迹。
 */
import { createFreehand, type Shape } from './shapes'

export interface PerfReport {
  shapes: number
  points: number
  /** 每次完整重绘的平均耗时（ms） */
  perFrame: number
  /** 重绘耗时分布 */
  p50: number
  p95: number
  p99: number
  max: number
  /** 按 16.67ms 预算折算，理论上限帧率 */
  budgetFps: number
  /** 占单帧预算的百分比 */
  budgetUsed: number
  samples: number
}

/** 造 N 条像真实手写的笔迹（正弦扰动 + 变长），避免用直线或重复点作弊。 */
export function generateStrokes(count: number, pointsPerStroke = 60): Shape[] {
  const shapes: Shape[] = []
  const palette = ['#16181D', '#3D5AFE', '#E8590C', '#12B886', '#7048E8']
  for (let i = 0; i < count; i += 1) {
    const points: [number, number][] = []
    const ox = (i % 36) * 96 + ((i * 37) % 40)
    const oy = Math.floor(i / 36) * 78 + ((i * 53) % 30)
    const amplitude = 6 + ((i * 7) % 16)
    const freq = 0.3 + ((i * 11) % 60) / 100
    const len = Math.round(pointsPerStroke * (0.6 + ((i * 13) % 80) / 100))
    for (let p = 0; p < len; p += 1) {
      points.push([
        Math.round((ox + p * 1.5) * 10) / 10,
        Math.round((oy + Math.sin(p * freq) * amplitude + Math.cos(p * 0.21) * 4) * 10) / 10,
      ])
    }
    shapes.push(
      createFreehand(points, {
        style: {
          stroke: palette[i % palette.length] ?? '#16181D',
          strokeWidth: 1 + (i % 4),
        },
      }),
    )
  }
  return shapes
}

function percentile(sorted: number[], p: number): number {
  if (sorted.length === 0) return 0
  const idx = Math.min(sorted.length - 1, Math.max(0, Math.ceil((p / 100) * sorted.length) - 1))
  return sorted[idx] ?? 0
}

export interface RepaintMeasurer {
  measureRepaint: (iterations?: number) => { total: number; perFrame: number; samples: number[] }
}

/** 跑一次测量并把结果整理成报告。 */
export function measureCanvas(
  shapes: readonly Shape[],
  measurer: RepaintMeasurer,
  iterations = 40,
): PerfReport {
  const raw = measurer.measureRepaint(iterations)
  const sorted = [...raw.samples].sort((a, b) => a - b)
  const perFrame = raw.perFrame
  const points = shapes.reduce((sum, s) => sum + (s.type === 'freehand' ? s.points.length : 0), 0)

  return {
    shapes: shapes.length,
    points,
    perFrame,
    p50: percentile(sorted, 50),
    p95: percentile(sorted, 95),
    p99: percentile(sorted, 99),
    max: sorted[sorted.length - 1] ?? 0,
    budgetFps: perFrame > 0 ? 1000 / perFrame : 0,
    budgetUsed: Math.min(100, (perFrame / (1000 / 60)) * 100),
    samples: sorted.length,
  }
}

export function formatPerfReport(report: PerfReport): string {
  return (
    `${report.shapes} 条笔迹 / ${report.points} 点 · 重绘 ${report.perFrame.toFixed(2)}ms/帧` +
    ` (p95 ${report.p95.toFixed(2)}ms) · 占 60fps 预算 ${report.budgetUsed.toFixed(1)}%` +
    ` · 理论上限 ${report.budgetFps.toFixed(0)}fps`
  )
}
