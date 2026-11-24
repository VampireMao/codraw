/**
 * 笔迹处理：点抽稀 + 二次贝塞尔平滑。
 *
 * 为什么不是「逐点 lineTo」：
 *  - 原始 pointermove 采样受 60Hz/120Hz 限制且抖动，直接连线会有折角；
 *  - 二次贝塞尔以「相邻点中点」为曲线端点，天然 C1 连续，且不增加控制点，
 *    可视效果接近 Catmull-Rom 但计算量更低。
 *
 * 为什么不是 RDP（道格拉斯-普克）：
 *  - RDP 是离线算法，需要看完整条笔画才能决定保留哪些点，不适合落笔即绘；
 *  - 这里用「最小间距抽稀」：相邻采样点距离小于阈值就丢弃，等价于低通滤波，
 *    在线增量可用，且能减少 40%~60% 的点数。
 */
import type { Point } from './geometry'

/** 协议里点格式为 [x, y, pressure?]。 */
export type InkPoint = [number, number] | [number, number, number]

export const pressureOf = (point: InkPoint): number =>
  point.length > 2 && typeof point[2] === 'number' ? point[2] : 0.5

export interface PathSink {
  moveTo(x: number, y: number): void
  lineTo(x: number, y: number): void
  quadraticCurveTo(cx: number, cy: number, x: number, y: number): void
  closePath(): void
}

/**
 * 把点集写入 path sink。单点会画一个极短线段，保证「点一下」也能落墨。
 * points 至少 1 个；调用方负责避免空数组。
 */
export function traceSmoothPath(sink: PathSink, points: readonly InkPoint[], closed = false): void {
  const n = points.length
  if (n === 0) return
  const p0 = points[0]
  if (!p0) return
  if (n === 1) {
    sink.moveTo(p0[0], p0[1])
    sink.lineTo(p0[0] + 0.01, p0[1] + 0.01)
    return
  }
  if (n === 2) {
    const p1 = points[1]
    if (!p1) return
    sink.moveTo(p0[0], p0[1])
    sink.lineTo(p1[0], p1[1])
    return
  }

  sink.moveTo(p0[0], p0[1])
  for (let i = 1; i < n - 1; i += 1) {
    const current = points[i]
    const next = points[i + 1]
    if (!current || !next) continue
    const mx = (current[0] + next[0]) / 2
    const my = (current[1] + next[1]) / 2
    sink.quadraticCurveTo(current[0], current[1], mx, my)
  }
  const last = points[n - 1]
  const penultimate = points[n - 2]
  if (last && penultimate) {
    sink.quadraticCurveTo(penultimate[0], penultimate[1], last[0], last[1])
  }
  if (closed) sink.closePath()
}

/**
 * 在线抽稀：和上一个保留点距离不足 minDist 就丢弃。
 * 返回 true 表示该点被保留（调用方可直接 push 到原数组）。
 */
export function shouldKeepPoint(last: InkPoint | undefined, next: InkPoint, minDist: number): boolean {
  if (!last) return true
  const dx = next[0] - last[0]
  const dy = next[1] - last[1]
  return dx * dx + dy * dy >= minDist * minDist
}

/** 离线抽稀（用于导入/测试的整条笔画）。 */
export function thinPoints(points: readonly InkPoint[], minDist: number): InkPoint[] {
  const out: InkPoint[] = []
  for (const point of points) {
    if (shouldKeepPoint(out[out.length - 1], point, minDist)) out.push(point)
  }
  return out
}

/** 点集的包围盒（世界坐标），不叠加笔宽。 */
export function pointsBounds(points: readonly InkPoint[]): {
  minX: number
  minY: number
  maxX: number
  maxY: number
} {
  let minX = Number.POSITIVE_INFINITY
  let minY = Number.POSITIVE_INFINITY
  let maxX = Number.NEGATIVE_INFINITY
  let maxY = Number.NEGATIVE_INFINITY
  for (const p of points) {
    if (p[0] < minX) minX = p[0]
    if (p[1] < minY) minY = p[1]
    if (p[0] > maxX) maxX = p[0]
    if (p[1] > maxY) maxY = p[1]
  }
  if (!Number.isFinite(minX)) return { minX: 0, minY: 0, maxX: 0, maxY: 0 }
  return { minX, minY, maxX, maxY }
}

export function toPoint(p: InkPoint): Point {
  return { x: p[0], y: p[1] }
}
