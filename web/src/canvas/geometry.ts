/** 基础几何类型与纯函数。画布、命中测试、视口共用，不依赖 DOM。 */

export interface Point {
  x: number
  y: number
}

export interface Size {
  width: number
  height: number
}

export interface Rect {
  x: number
  y: number
  w: number
  h: number
}

export const clamp = (value: number, min: number, max: number): number =>
  value < min ? min : value > max ? max : value

export const lerp = (a: number, b: number, t: number): number => a + (b - a) * t

export const distance = (a: Point, b: Point): number => Math.hypot(b.x - a.x, b.y - a.y)

/** 保留 1 位小数（与协议 §0 坐标精度一致），避免浮点尾巴影响序列化。 */
export const round1 = (value: number): number => Math.round(value * 10) / 10

export function rectFromPoints(a: Point, b: Point): Rect {
  return {
    x: Math.min(a.x, b.x),
    y: Math.min(a.y, b.y),
    w: Math.abs(b.x - a.x),
    h: Math.abs(b.y - a.y),
  }
}

export function normalizeRect(rect: Rect): Rect {
  const x = rect.w < 0 ? rect.x + rect.w : rect.x
  const y = rect.h < 0 ? rect.y + rect.h : rect.y
  return { x, y, w: Math.abs(rect.w), h: Math.abs(rect.h) }
}

export function expandRect(rect: Rect, by: number): Rect {
  return { x: rect.x - by, y: rect.y - by, w: rect.w + by * 2, h: rect.h + by * 2 }
}

export function unionRect(a: Rect | null, b: Rect | null): Rect | null {
  if (!a) return b
  if (!b) return a
  const x = Math.min(a.x, b.x)
  const y = Math.min(a.y, b.y)
  const right = Math.max(a.x + a.w, b.x + b.w)
  const bottom = Math.max(a.y + a.h, b.y + b.h)
  return { x, y, w: right - x, h: bottom - y }
}

export function rectsIntersect(a: Rect, b: Rect): boolean {
  return a.x < b.x + b.w && a.x + a.w > b.x && a.y < b.y + b.h && a.y + a.h > b.y
}

export function rectContains(outer: Rect, inner: Rect): boolean {
  return (
    inner.x >= outer.x &&
    inner.y >= outer.y &&
    inner.x + inner.w <= outer.x + outer.w &&
    inner.y + inner.h <= outer.y + outer.h
  )
}

export function pointInRect(p: Point, rect: Rect, pad = 0): boolean {
  return (
    p.x >= rect.x - pad && p.x <= rect.x + rect.w + pad && p.y >= rect.y - pad && p.y <= rect.y + rect.h + pad
  )
}

export function rectCenter(rect: Rect): Point {
  return { x: rect.x + rect.w / 2, y: rect.y + rect.h / 2 }
}

export function isValidRect(rect: Rect | null): rect is Rect {
  return !!rect && Number.isFinite(rect.w) && Number.isFinite(rect.h)
}

/** 点到线段的最短距离。命中测试的核心。 */
export function distToSegment(p: Point, a: Point, b: Point): number {
  const dx = b.x - a.x
  const dy = b.y - a.y
  const lenSq = dx * dx + dy * dy
  if (lenSq === 0) return Math.hypot(p.x - a.x, p.y - a.y)
  let t = ((p.x - a.x) * dx + (p.y - a.y) * dy) / lenSq
  t = clamp(t, 0, 1)
  const px = a.x + t * dx
  const py = a.y + t * dy
  return Math.hypot(p.x - px, p.y - py)
}

/** 点到折线的最短距离；少于 2 个点时退化为点距离。 */
export function distToPolyline(p: Point, points: readonly Point[]): number {
  if (points.length === 0) return Number.POSITIVE_INFINITY
  const first = points[0]
  if (!first) return Number.POSITIVE_INFINITY
  if (points.length === 1) return distance(p, first)
  let best = Number.POSITIVE_INFINITY
  for (let i = 1; i < points.length; i += 1) {
    const a = points[i - 1]
    const b = points[i]
    if (!a || !b) continue
    const d = distToSegment(p, a, b)
    if (d < best) best = d
  }
  return best
}

/** 椭圆边界上的最近点近似距离：用于「点是否靠近椭圆描边」。 */
export function distanceToEllipse(p: Point, rect: Rect, strokeWidth: number): number {
  const rx = Math.max(rect.w / 2, 0.001)
  const ry = Math.max(rect.h / 2, 0.001)
  const cx = rect.x + rx
  const cy = rect.y + ry
  const dx = (p.x - cx) / (rx + strokeWidth / 2)
  const dy = (p.y - cy) / (ry + strokeWidth / 2)
  const norm = Math.hypot(dx, dy)
  // 归一化距离 → 世界距离的粗略换算
  const approxRadius = (rx + ry) / 2
  return Math.abs(norm - 1) * approxRadius
}
