/**
 * 图形数据模型 + 命中测试 + 序列化。
 *
 * 契约：`docs/PROTOCOL.md` §5（服务端权威 shape）。
 * 本地模型与线格式的差异只在两处：
 *   1. 线格式的 `kind` 在本地叫 `type`（本地没有信封，不会歧义；
 *      `toWire/fromWire` 负责改名，见 §5.1 的字段决策）；
 *   2. `pending` 是本地瞬态字段，序列化时剔除。
 *
 * 便签：`kind:"text"` + `data.variant:"note"` + `style.fill` 作底色，
 * **不扩展 kind 枚举**（PROTOCOL §5.2）——服务端校验无需特殊分支。
 */
import {
  distToPolyline,
  distanceToEllipse,
  expandRect,
  distToSegment,
  normalizeRect,
  pointInRect,
  rectFromPoints,
  round1,
  unionRect,
  type Point,
  type Rect,
} from './geometry'
import type { InkPoint } from './smoothing'

export type ShapeType = 'freehand' | 'rect' | 'ellipse' | 'arrow' | 'text'
export type TextVariant = 'note' | 'plain'
export type DashStyle = 'solid' | 'dashed' | 'dotted'
export type ArrowHead = 'end' | 'both'

export interface ShapeStyle {
  stroke: string
  fill: string
  strokeWidth: number
  opacity: number
  dash: DashStyle
}

export interface ShapeAuthor {
  cid: string
  name: string
}

interface ShapeBase {
  id: string
  geom: Rect
  style: ShapeStyle
  author: ShapeAuthor | null
  rev: number
  deleted: boolean
  createdAt: number
  updatedAt: number
  /** 本地乐观渲染、尚未被服务端确认（Phase B 使用；Phase A 恒为 false）。 */
  pending: boolean
}

export interface FreehandShape extends ShapeBase {
  type: 'freehand'
  points: InkPoint[]
  closed: boolean
}

export interface RectShape extends ShapeBase {
  type: 'rect'
  radius: number
}

export interface EllipseShape extends ShapeBase {
  type: 'ellipse'
}

export interface ArrowShape extends ShapeBase {
  type: 'arrow'
  x1: number
  y1: number
  x2: number
  y2: number
  head: ArrowHead
}

export interface TextShape extends ShapeBase {
  type: 'text'
  text: string
  variant: TextVariant
}

export type Shape = FreehandShape | RectShape | EllipseShape | ArrowShape | TextShape

export const DEFAULT_STROKE = '#16181d'
export const NOTE_FILL = '#fcf3d9'
export const NOTE_WIDTH = 260

/**
 * 画笔调色板。与服务端 `paletteColor`（backend/internal/ws/ws.go）保持同一组色值，
 * 这样"默认颜色 = 我的头像颜色"在视觉上是同一个色。
 *
 * 全部为小写 `#rrggbb`：服务端按 PROTOCOL §5.3 校验格式，大写会被拒。
 */
export const INK_PALETTE = [
  '#3d5afe',
  '#e8590c',
  '#12b886',
  '#7048e8',
  '#e64980',
  '#0ca678',
  '#f59f00',
  '#1c7ed6',
] as const

/** 调色板之外还允许用墨色（与服务端默认笔色一致）。 */
export const INK_COLORS = [DEFAULT_STROKE, ...INK_PALETTE] as const

export const defaultStyle = (overrides: Partial<ShapeStyle> = {}): ShapeStyle => ({
  stroke: DEFAULT_STROKE,
  fill: 'none',
  strokeWidth: 2,
  opacity: 1,
  dash: 'solid',
  ...overrides,
})

const ID_ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'
let idCounter = 0

/** 生成协议合法 id：`sh_` + 12 位 base62。 */
export function createShapeId(): string {
  idCounter += 1
  let tail = ''
  const bytes = new Uint8Array(10)
  if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') {
    crypto.getRandomValues(bytes)
  } else {
    for (let i = 0; i < bytes.length; i += 1) bytes[i] = Math.floor(Math.random() * 256)
  }
  for (let i = 0; i < bytes.length; i += 1) {
    tail += ID_ALPHABET[(bytes[i] ?? 0) % ID_ALPHABET.length]
  }
  // 计数器后缀保证同毫秒批量创建也不重复
  return `sh_${tail}${idCounter.toString(36)}`
}

const now = (): number => Date.now()

interface BaseInit {
  id?: string
  style?: Partial<ShapeStyle>
  author?: ShapeAuthor | null
  pending?: boolean
}

function base(init: BaseInit, geom: Rect): ShapeBase {
  const ts = now()
  return {
    id: init.id ?? createShapeId(),
    geom,
    style: defaultStyle(init.style),
    author: init.author ?? null,
    rev: 1,
    deleted: false,
    createdAt: ts,
    updatedAt: ts,
    pending: init.pending ?? false,
  }
}

export function createFreehand(points: InkPoint[], init: BaseInit = {}): FreehandShape {
  const shape: FreehandShape = {
    ...base(init, { x: 0, y: 0, w: 0, h: 0 }),
    type: 'freehand',
    points,
    closed: false,
  }
  recomputeFreehandGeom(shape)
  return shape
}

export function createRect(start: Point, end: Point, init: BaseInit = {}): RectShape {
  return {
    ...base(init, normalizeRect(rectFromPoints(start, end))),
    type: 'rect',
    radius: 0,
  }
}

export function createEllipse(start: Point, end: Point, init: BaseInit = {}): EllipseShape {
  return {
    ...base(init, normalizeRect(rectFromPoints(start, end))),
    type: 'ellipse',
  }
}

export function createArrow(start: Point, end: Point, init: BaseInit = {}): ArrowShape {
  const shape: ArrowShape = {
    ...base(init, normalizeRect(rectFromPoints(start, end))),
    type: 'arrow',
    x1: round1(start.x),
    y1: round1(start.y),
    x2: round1(end.x),
    y2: round1(end.y),
    head: 'end',
  }
  return shape
}

export function createNote(topLeft: Point, init: BaseInit = {}): TextShape {
  const style = { fill: NOTE_FILL, strokeWidth: 4, ...init.style }
  return {
    ...base({ ...init, style }, { x: round1(topLeft.x), y: round1(topLeft.y), w: NOTE_WIDTH, h: 180 }),
    type: 'text',
    text: '',
    variant: 'note',
  }
}

export function createText(topLeft: Point, text: string, init: BaseInit = {}): TextShape {
  return {
    ...base(init, { x: round1(topLeft.x), y: round1(topLeft.y), w: 240, h: 32 }),
    type: 'text',
    text,
    variant: 'plain',
  }
}

/** 重算自由笔包围盒（点集变化后必须调用）。 */
export function recomputeFreehandGeom(shape: FreehandShape): void {
  const points = shape.points
  if (points.length === 0) {
    shape.geom = { x: 0, y: 0, w: 0, h: 0 }
    return
  }
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
  shape.geom = { x: round1(minX), y: round1(minY), w: round1(maxX - minX), h: round1(maxY - minY) }
}

export function moveShape(shape: Shape, dx: number, dy: number): void {
  shape.geom = {
    ...shape.geom,
    x: round1(shape.geom.x + dx),
    y: round1(shape.geom.y + dy),
  }
  if (shape.type === 'freehand') {
    for (const p of shape.points) {
      p[0] = round1(p[0] + dx)
      p[1] = round1(p[1] + dy)
    }
  }
  if (shape.type === 'arrow') {
    shape.x1 = round1(shape.x1 + dx)
    shape.y1 = round1(shape.y1 + dy)
    shape.x2 = round1(shape.x2 + dx)
    shape.y2 = round1(shape.y2 + dy)
  }
  shape.updatedAt = now()
  shape.rev += 1
}

/** 描边包围盒（世界坐标，含笔宽与箭头扩散），用于裁剪与适配视图。 */
export function shapeBounds(shape: Shape): Rect {
  const pad = shape.style.strokeWidth / 2 + 1
  if (shape.type === 'arrow') {
    const box = normalizeRect(rectFromPoints({ x: shape.x1, y: shape.y1 }, { x: shape.x2, y: shape.y2 }))
    return expandRect(box, pad + 4)
  }
  return expandRect(shape.geom, pad)
}

export function sceneBounds(shapes: readonly Shape[]): Rect | null {
  let acc: Rect | null = null
  for (const shape of shapes) {
    if (shape.deleted) continue
    acc = unionRect(acc, shapeBounds(shape))
  }
  return acc
}

/** 判定某个世界点是否命中图形；tolWorld 为屏幕容差换算后的世界长度。 */
export function hitTest(shape: Shape, p: Point, tolWorld: number): boolean {
  if (shape.deleted) return false
  const strokeTol = shape.style.strokeWidth / 2 + tolWorld

  switch (shape.type) {
    case 'freehand': {
      if (shape.points.length === 0) return false
      if (!pointInRect(p, shapeBounds(shape), tolWorld)) return false
      return (
        distToPolyline(
          p,
          shape.points.map((pt) => ({ x: pt[0], y: pt[1] })),
        ) <= strokeTol
      )
    }
    case 'rect': {
      if (!pointInRect(p, shapeBounds(shape), tolWorld)) return false
      if (shape.style.fill !== 'none' && pointInRect(p, shape.geom)) return true
      const r = shape.geom
      const corners = [
        { x: r.x, y: r.y },
        { x: r.x + r.w, y: r.y },
        { x: r.x + r.w, y: r.y + r.h },
        { x: r.x, y: r.y + r.h },
      ]
      for (let i = 0; i < corners.length; i += 1) {
        const a = corners[i]
        const b = corners[(i + 1) % corners.length]
        if (a && b && distToSegment(p, a, b) <= strokeTol) return true
      }
      return false
    }
    case 'ellipse': {
      if (!pointInRect(p, shapeBounds(shape), tolWorld)) return false
      if (shape.style.fill !== 'none' && pointInRect(p, shape.geom)) return true
      return (
        distanceToEllipse(p, shape.geom, shape.style.strokeWidth) <= tolWorld + shape.style.strokeWidth / 2
      )
    }
    case 'arrow': {
      const a = { x: shape.x1, y: shape.y1 }
      const b = { x: shape.x2, y: shape.y2 }
      if (distToSegment(p, a, b) <= strokeTol + 2) return true
      const head = arrowHeadPath(shape)
      for (let i = 1; i < head.length; i += 1) {
        const s = head[i - 1]
        const e = head[i]
        if (s && e && distToSegment(p, s, e) <= strokeTol + 2) return true
      }
      return false
    }
    case 'text': {
      return pointInRect(p, shape.geom, tolWorld)
    }
    default:
      return false
  }
}

/** 箭头头部两条边（世界坐标）。渲染与命中测试共用，保证「看到的就是能点的」。 */
export function arrowHeadPath(shape: ArrowShape): Point[] {
  const { x1, y1, x2, y2 } = shape
  const angle = Math.atan2(y2 - y1, x2 - x1)
  const size = Math.min(18, Math.max(9, Math.hypot(x2 - x1, y2 - y1) * 0.28))
  const spread = Math.PI / 7
  return [
    { x: x2 - size * Math.cos(angle - spread), y: y2 - size * Math.sin(angle - spread) },
    { x: x2, y: y2 },
    { x: x2 - size * Math.cos(angle + spread), y: y2 - size * Math.sin(angle + spread) },
  ]
}

/* ------------------------------------------------------------------------- *
 * 序列化：本地模型 → 协议 shape（docs/PROTOCOL.md §5.1）
 * ------------------------------------------------------------------------- */

export interface WireStyle {
  stroke: string
  fill: string
  strokeWidth: number
  opacity: number
  dash: DashStyle
}

/**
 * 协议 §0 要求颜色为 **`#rrggbb` 小写**。
 * UI 令牌与设计稿用大写（`#16181D`）更易读，所以只在上行序列化时归一化——
 * 这样「设计稿大小写」与「协议小写」两个约束同时满足，且只在边界处做转换。
 */
export function normalizeColor(color: string): string {
  if (color === 'none') return 'none'
  const match = /^#([0-9a-fA-F]{6})$/.exec(color)
  return match?.[1] ? `#${match[1].toLowerCase()}` : color
}

export interface WireShape {
  id: string
  /** 契约：字段名是 `kind`，不是 `type`（避免与信封 type 混淆，PROTOCOL §5.1） */
  kind: ShapeType
  geom: { x: number; y: number; w: number; h: number }
  style: WireStyle
  data: Record<string, unknown>
  text: string | null
  author: ShapeAuthor | null
  rev: number
  deleted: boolean
  createdAt: number
  updatedAt: number
}

export function toWire(shape: Shape): WireShape {
  const data: Record<string, unknown> = {}
  if (shape.type === 'freehand') {
    data.points = shape.points.map((p) => [round1(p[0]), round1(p[1]), ...(p.length > 2 ? [p[2]] : [])])
    data.closed = shape.closed
  } else if (shape.type === 'rect') {
    data.radius = shape.radius
  } else if (shape.type === 'arrow') {
    data.x1 = shape.x1
    data.y1 = shape.y1
    data.x2 = shape.x2
    data.y2 = shape.y2
    data.head = shape.head
  } else if (shape.type === 'text') {
    data.variant = shape.variant
  }

  return {
    id: shape.id,
    kind: shape.type,
    geom: {
      x: shape.geom.x,
      y: shape.geom.y,
      w: shape.geom.w,
      h: shape.geom.h,
    },
    style: {
      stroke: normalizeColor(shape.style.stroke),
      fill: normalizeColor(shape.style.fill),
      strokeWidth: shape.style.strokeWidth,
      opacity: shape.style.opacity,
      dash: shape.style.dash,
    },
    data,
    text: shape.type === 'text' ? shape.text : null,
    author: shape.author,
    rev: shape.rev,
    deleted: shape.deleted,
    createdAt: shape.createdAt,
    updatedAt: shape.updatedAt,
  }
}

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value)

const asNumber = (value: unknown, fallback: number): number =>
  typeof value === 'number' && Number.isFinite(value) ? value : fallback

const asString = (value: unknown, fallback: string): string => (typeof value === 'string' ? value : fallback)

function readStyle(raw: unknown): ShapeStyle {
  if (!isRecord(raw)) return defaultStyle()
  return {
    stroke: asString(raw.stroke, DEFAULT_STROKE),
    fill: asString(raw.fill, 'none'),
    strokeWidth: Math.min(32, Math.max(1, asNumber(raw.strokeWidth, 2))),
    opacity: Math.min(1, Math.max(0.1, asNumber(raw.opacity, 1))),
    dash: raw.dash === 'dashed' || raw.dash === 'dotted' ? raw.dash : 'solid',
  }
}

function readPoints(raw: unknown): InkPoint[] {
  if (!Array.isArray(raw)) return []
  const out: InkPoint[] = []
  for (const entry of raw) {
    if (!Array.isArray(entry)) continue
    const x = asNumber(entry[0], Number.NaN)
    const y = asNumber(entry[1], Number.NaN)
    if (!Number.isFinite(x) || !Number.isFinite(y)) continue
    const pressure = asNumber(entry[2], 0.5)
    out.push(entry.length > 2 ? [x, y, pressure] : [x, y])
  }
  return out
}

/**
 * 协议 shape → 本地模型。
 *
 * 契约：`kind` 是类型字段（PROTOCOL §5.1）；为兼容早期实现，
 * 缺失 `kind` 时回退读 `type`（服务端不会同时发两个）。
 *
 * 未知 kind 返回 null（协议 §9：不渲染但保留数据；保留原始数据由上层负责）。
 */
export function fromWire(raw: unknown): Shape | null {
  if (!isRecord(raw)) return null
  const kind = typeof raw.kind === 'string' ? raw.kind : raw.type
  if (kind !== 'freehand' && kind !== 'rect' && kind !== 'ellipse' && kind !== 'arrow' && kind !== 'text') {
    return null
  }
  const id = asString(raw.id, '')
  if (!id) return null

  const geom = isRecord(raw.geom) ? raw.geom : {}
  const rect: Rect = {
    x: asNumber(geom.x, 0),
    y: asNumber(geom.y, 0),
    w: Math.max(0, asNumber(geom.w, 0)),
    h: Math.max(0, asNumber(geom.h, 0)),
  }
  const authorRaw = isRecord(raw.author) ? raw.author : null
  const common: BaseInit = {
    id,
    style: readStyle(raw.style),
    author: authorRaw ? { cid: asString(authorRaw.cid, ''), name: asString(authorRaw.name, '') } : null,
    pending: false,
  }
  const merged = base(common, rect)
  merged.rev = Math.max(1, asNumber(raw.rev, 1))
  merged.deleted = raw.deleted === true
  merged.createdAt = asNumber(raw.createdAt, merged.createdAt)
  merged.updatedAt = asNumber(raw.updatedAt, merged.updatedAt)

  const data = isRecord(raw.data) ? raw.data : {}

  switch (kind) {
    case 'freehand': {
      const shape: FreehandShape = {
        ...merged,
        type: 'freehand',
        points: readPoints(data.points),
        closed: data.closed === true,
      }
      if (shape.points.length > 0 && rect.w === 0 && rect.h === 0) recomputeFreehandGeom(shape)
      return shape
    }
    case 'rect':
      return { ...merged, type: 'rect', radius: Math.min(64, Math.max(0, asNumber(data.radius, 0))) }
    case 'ellipse':
      return { ...merged, type: 'ellipse' }
    case 'arrow':
      return {
        ...merged,
        type: 'arrow',
        x1: asNumber(data.x1, rect.x),
        y1: asNumber(data.y1, rect.y),
        x2: asNumber(data.x2, rect.x + rect.w),
        y2: asNumber(data.y2, rect.y + rect.h),
        head: data.head === 'both' ? 'both' : 'end',
      }
    case 'text':
      return {
        ...merged,
        type: 'text',
        text: asString(raw.text, ''),
        variant: data.variant === 'note' ? 'note' : 'plain',
      }
    default:
      return null
  }
}

/** 深浅色工具：把 #rrggbb 转成带透明度的 rgba()。 */
export function withAlpha(hex: string, alpha: number): string {
  const match = /^#([0-9a-f]{6})$/i.exec(hex)
  if (!match || !match[1]) return hex
  const value = Number.parseInt(match[1], 16)
  const r = (value >> 16) & 255
  const g = (value >> 8) & 255
  const b = value & 255
  return `rgba(${r}, ${g}, ${b}, ${alpha})`
}

/** 简单的亮度判断，用于决定便签上的文字颜色。 */
export function isLightColor(hex: string): boolean {
  const match = /^#([0-9a-f]{6})$/i.exec(hex)
  if (!match || !match[1]) return true
  const value = Number.parseInt(match[1], 16)
  const r = (value >> 16) & 255
  const g = (value >> 8) & 255
  const b = value & 255
  return (r * 299 + g * 587 + b * 114) / 1000 > 150
}
