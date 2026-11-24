/**
 * 视口：世界坐标 ↔ 屏幕坐标（CSS 像素），平移、缩放、锚点缩放、边界约束。
 *
 * 变换定义（单一真相）：
 *   screen = world * scale + translate
 *   world  = (screen - translate) / scale
 *
 * 采用「缩放 + 平移」而非矩阵，是因为画布只有这两类变换，
 * 反解一行就能算出来，避免引入矩阵求逆和浮点误差累积。
 */
import { clamp, type Point, type Rect, type Size } from './geometry'

export const MIN_SCALE = 0.1
export const MAX_SCALE = 8

/** 世界坐标安全边界，与协议 §0「坐标限制在 ±1e6 内」对齐，这里留余量。 */
export const WORLD_LIMIT = 500_000

export interface ViewportState {
  scale: number
  x: number
  y: number
}

type Listener = () => void

export class Viewport {
  scale = 1
  x = 0
  y = 0
  width = 0
  height = 0

  private listeners = new Set<Listener>()

  subscribe(listener: Listener): () => void {
    this.listeners.add(listener)
    return () => {
      this.listeners.delete(listener)
    }
  }

  private emit(): void {
    for (const listener of this.listeners) listener()
  }

  /** 容器尺寸变化（CSS 像素）。不改变世界中心。 */
  resize(width: number, height: number): void {
    if (width === this.width && height === this.height) return
    const prevCenter = this.screenToWorld({ x: this.width / 2, y: this.height / 2 })
    this.width = Math.max(width, 1)
    this.height = Math.max(height, 1)
    if (this.prevValid) {
      this.centerOn(prevCenter)
    }
    this.emit()
  }

  private prevValid = false

  set(state: Partial<ViewportState>): void {
    if (state.scale !== undefined) this.scale = clamp(state.scale, MIN_SCALE, MAX_SCALE)
    if (state.x !== undefined) this.x = state.x
    if (state.y !== undefined) this.y = state.y
    this.prevValid = true
    this.emit()
  }

  /** 屏幕（CSS px）→ 世界 */
  screenToWorld(point: Point): Point {
    return {
      x: (point.x - this.x) / this.scale,
      y: (point.y - this.y) / this.scale,
    }
  }

  /** 世界 → 屏幕（CSS px） */
  worldToScreen(point: Point): Point {
    return {
      x: point.x * this.scale + this.x,
      y: point.y * this.scale + this.y,
    }
  }

  /** 世界矩形 → 屏幕矩形（CSS px） */
  worldRectToScreen(rect: Rect): Rect {
    return {
      x: rect.x * this.scale + this.x,
      y: rect.y * this.scale + this.y,
      w: rect.w * this.scale,
      h: rect.h * this.scale,
    }
  }

  /** 当前可见的世界矩形（用于视口裁剪 / 适配）。 */
  visibleWorldRect(): Rect {
    return {
      x: -this.x / this.scale,
      y: -this.y / this.scale,
      w: this.width / this.scale,
      h: this.height / this.scale,
    }
  }

  /** 屏幕像素换算成世界长度：用于把「8px 命中容差」变成世界单位。 */
  screenToWorldLength(px: number): number {
    return px / this.scale
  }

  panBy(dxScreen: number, dyScreen: number): void {
    this.x += dxScreen
    this.y += dyScreen
    this.constrain()
    this.prevValid = true
    this.emit()
  }

  /** 以屏幕点 anchor 为锚缩放：锚点下的世界点保持不动。 */
  zoomAt(anchorScreen: Point, nextScale: number): void {
    const scale = clamp(nextScale, MIN_SCALE, MAX_SCALE)
    if (scale === this.scale) return
    const world = this.screenToWorld(anchorScreen)
    this.scale = scale
    this.x = anchorScreen.x - world.x * scale
    this.y = anchorScreen.y - world.y * scale
    this.constrain()
    this.prevValid = true
    this.emit()
  }

  zoomBy(anchorScreen: Point, factor: number): void {
    this.zoomAt(anchorScreen, this.scale * factor)
  }

  /** 以视口中心为锚缩放（按钮/快捷键用）。 */
  zoomByCenter(factor: number): void {
    this.zoomBy({ x: this.width / 2, y: this.height / 2 }, factor)
  }

  /** 把某个世界点放到视口中心（不改变缩放）。 */
  centerOn(world: Point): void {
    this.x = this.width / 2 - world.x * this.scale
    this.y = this.height / 2 - world.y * this.scale
    this.constrain()
    this.prevValid = true
  }

  reset(): void {
    this.scale = 1
    this.x = this.width / 2
    this.y = this.height / 2
    this.prevValid = true
    this.emit()
  }

  /**
   * 适配视图：把所有内容放进视口并居中，四周留 padding（UX-SPEC §7.9：64px）。
   * 空内容时回到原点，缩放 100%。
   */
  fit(bounds: Rect | null, padding = 64): void {
    if (!bounds || bounds.w <= 0 || bounds.h <= 0) {
      this.scale = 1
      this.x = this.width / 2
      this.y = this.height / 2
      this.prevValid = true
      this.emit()
      return
    }
    const usableW = Math.max(this.width - padding * 2, 48)
    const usableH = Math.max(this.height - padding * 2, 48)
    // 留 5% 余量，避免图形紧贴边缘时观感局促
    const raw = Math.min(usableW / (bounds.w * 1.05), usableH / (bounds.h * 1.05))
    this.scale = clamp(raw, MIN_SCALE, MAX_SCALE)
    const cx = bounds.x + bounds.w / 2
    const cy = bounds.y + bounds.h / 2
    this.x = this.width / 2 - cx * this.scale
    this.y = this.height / 2 - cy * this.scale
    this.constrain()
    this.prevValid = true
    this.emit()
  }

  /** 视口中心所在的世界坐标。 */
  center(): Point {
    return this.screenToWorld({ x: this.width / 2, y: this.height / 2 })
  }

  /** 不变量：视口中心不能跑到世界边界之外，避免「画布彻底迷失」。 */
  private constrain(): void {
    const center = this.center()
    const cx = clamp(center.x, -WORLD_LIMIT, WORLD_LIMIT)
    const cy = clamp(center.y, -WORLD_LIMIT, WORLD_LIMIT)
    if (cx !== center.x) this.x = this.width / 2 - cx * this.scale
    if (cy !== center.y) this.y = this.height / 2 - cy * this.scale
  }

  snapshot(): ViewportState {
    return { scale: this.scale, x: this.x, y: this.y }
  }

  restore(state: ViewportState): void {
    this.set(state)
  }

  /** 缩放百分比（取整，用于 UI 展示）。 */
  get percent(): number {
    return Math.round(this.scale * 100)
  }
}

/** 判断屏幕尺寸是否需要切换移动端布局（UX-SPEC：断点 768）。 */
export const MOBILE_BREAKPOINT = 768

export function isMobileWidth(width: number): boolean {
  return width < MOBILE_BREAKPOINT
}

export type { Size }
