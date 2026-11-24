/**
 * 渲染器：双 Canvas 分层 + Path2D 缓存 + 视口裁剪。
 *
 * 分层（见 web/README.md「渲染取舍」）：
 *   baseCanvas    —— 网格 + 已提交图形。只在「内容/视口/选中」变化时重绘。
 *   overlayCanvas —— 正在画的图形、选框、选中手柄、他人光标。变更频繁但内容少。
 * 落笔过程中 base 层完全不动（浏览器合成器保留位图），每帧成本只和「当前这一笔」
 * 有关，与画布上已有多少条笔迹无关。
 *
 * 为什么不做脏矩形：
 *   自由笔的脏区就是整条笔迹包围盒，多人同时画时脏区频繁重叠，
 *   维护合并/裁剪/重入的成本高于整层重绘；而整层重绘配合 Path2D 缓存 +
 *   视口裁剪在 1000 条笔迹下仍能满帧（见 README 实测数据）。
 */
import { clamp, expandRect, rectsIntersect, type Point, type Rect } from './geometry'
import {
  arrowHeadPath,
  shapeBounds,
  type ArrowShape,
  type RectShape,
  type Shape,
  type TextShape,
} from './shapes'
import { traceSmoothPath } from './smoothing'
import { wrapText } from './text'
import type { Viewport } from './viewport'

export interface CursorView {
  cid: string
  name: string
  color: string
  /** 世界坐标 */
  x: number
  y: number
}

export interface RenderInput {
  shapes: readonly Shape[]
  selection: ReadonlySet<string>
  draft: Shape | null
  marquee: Rect | null
  cursors: readonly CursorView[]
  readOnly: boolean
  /** 交互中（平移/缩放/绘制）时降级细节，松手后重绘高质量版本 */
  interacting: boolean
}

const GRID_DOT = 'rgba(22, 24, 29, 0.13)'
const GRID_MIN_PX = 15
const GRID_MAX_PX = 46
const SELECTION_PAD = 6
const FONT_STACK = 'ui-sans-serif, "PingFang SC", "Microsoft YaHei", system-ui, sans-serif'

interface TextLayout {
  lines: string[]
  font: string
  lineHeight: number
  version: string
}

export class Renderer {
  readonly base: HTMLCanvasElement
  readonly overlay: HTMLCanvasElement

  private readonly baseCtx: CanvasRenderingContext2D
  private readonly overlayCtx: CanvasRenderingContext2D
  private dpr = 1
  private width = 0
  private height = 0

  private frame = 0
  private sceneDirty = true
  private overlayDirty = true

  private readonly pathCache = new Map<string, { rev: number; path: Path2D }>()
  private readonly textCache = new Map<string, TextLayout>()
  private measureCtx: CanvasRenderingContext2D | null = null
  /** 网格 pattern 缓存：key = 平铺像素尺寸。每帧重建 canvas 会在平移时吃掉数毫秒。 */
  private gridTile: { key: string; pattern: CanvasPattern } | null = null

  constructor(
    private readonly container: HTMLElement,
    private readonly viewport: Viewport,
    private readonly getInput: () => RenderInput,
  ) {
    this.base = document.createElement('canvas')
    this.base.className = 'codraw-canvas codraw-canvas--base'
    this.overlay = document.createElement('canvas')
    this.overlay.className = 'codraw-canvas codraw-canvas--overlay'
    this.overlay.setAttribute('aria-hidden', 'true')
    container.append(this.base, this.overlay)

    const baseCtx = this.base.getContext('2d')
    const overlayCtx = this.overlay.getContext('2d')
    if (!baseCtx || !overlayCtx) throw new Error('Canvas 2D 上下文不可用')
    this.baseCtx = baseCtx
    this.overlayCtx = overlayCtx

    this.resize()
  }

  get size(): { width: number; height: number } {
    return { width: this.width, height: this.height }
  }

  /** 尺寸变化（含 DPR 变化）。CSS 尺寸与缓冲尺寸分离，保证 Retina 清晰度。 */
  resize(): void {
    const rect = this.container.getBoundingClientRect()
    const width = Math.max(1, Math.round(rect.width))
    const height = Math.max(1, Math.round(rect.height))
    const dpr = clamp(window.devicePixelRatio || 1, 1, 3)

    if (width === this.width && height === this.height && dpr === this.dpr) return

    this.width = width
    this.height = height
    this.dpr = dpr
    this.viewport.resize(width, height)

    for (const canvas of [this.base, this.overlay]) {
      canvas.width = Math.round(width * dpr)
      canvas.height = Math.round(height * dpr)
      canvas.style.width = `${width}px`
      canvas.style.height = `${height}px`
    }
    this.invalidateScene()
  }

  invalidateScene(): void {
    this.sceneDirty = true
    this.overlayDirty = true
    this.requestRender()
  }

  invalidateOverlay(): void {
    this.overlayDirty = true
    this.requestRender()
  }

  /** 形状被删除时清掉对应缓存，避免长期会话内存只增不减。 */
  pruneCaches(shapes: readonly Shape[]): void {
    if (shapes.length === 0) {
      this.pathCache.clear()
      this.textCache.clear()
      return
    }
    if (this.pathCache.size <= shapes.length) return
    const alive = new Set<string>()
    for (const shape of shapes) alive.add(shape.id)
    for (const id of [...this.pathCache.keys()]) if (!alive.has(id)) this.pathCache.delete(id)
    for (const id of [...this.textCache.keys()]) if (!alive.has(id)) this.textCache.delete(id)
  }

  private requestRender(): void {
    if (this.frame) return
    this.frame = requestAnimationFrame(this.tick)
  }

  /**
   * 同步重绘一次并返回耗时（ms）。
   *
   * 为什么需要这个：rAF 采样在「窗口不可见 / 离屏」时会被浏览器
   * 限流到 1Hz，测出来的「帧率」毫无意义（真实设备上会显得极差）。
   * 同步重绘测的是「一次完整重绘要花多少毫秒」，与 vsync 无关，
   * 是判断「还剩下多少帧预算」的正确指标。
   */
  measureRepaint(iterations = 30): { total: number; perFrame: number; samples: number[] } {
    const samples: number[] = []
    // 预热 5 次（Path2D 首次构建、pattern 首次创建都要算进去才准）
    for (let i = 0; i < 5; i += 1) this.drawNow()
    for (let i = 0; i < iterations; i += 1) {
      const t0 = performance.now()
      this.drawNow()
      samples.push(performance.now() - t0)
    }
    samples.sort((a, b) => a - b)
    const total = samples.reduce((s, v) => s + v, 0)
    return { total, perFrame: total / Math.max(1, samples.length), samples }
  }

  /** 立即绘制两层（不排队）；仅供性能测量与导出使用。 */
  drawNow(): void {
    this.sceneDirty = false
    this.overlayDirty = false
    this.drawScene()
    this.drawOverlay()
  }

  private readonly tick = (): void => {
    this.frame = 0
    if (this.sceneDirty) {
      this.sceneDirty = false
      this.drawScene()
    }
    if (this.overlayDirty) {
      this.overlayDirty = false
      this.drawOverlay()
    }
  }

  /** 页面隐藏时停掉待渲染帧，省电。 */
  setRunning(running: boolean): void {
    if (running) this.invalidateScene()
    else if (this.frame) {
      cancelAnimationFrame(this.frame)
      this.frame = 0
    }
  }

  destroy(): void {
    if (this.frame) cancelAnimationFrame(this.frame)
    this.frame = 0
    this.pathCache.clear()
    this.textCache.clear()
    this.base.remove()
    this.overlay.remove()
  }

  /* ------------------------------------------------------------------ *
   * base 层
   * ------------------------------------------------------------------ */

  private drawScene(): void {
    const ctx = this.baseCtx
    const input = this.getInput()
    ctx.setTransform(1, 0, 0, 1, 0, 0)
    ctx.clearRect(0, 0, this.base.width, this.base.height)
    ctx.setTransform(this.dpr, 0, 0, this.dpr, 0, 0)

    this.drawGrid(ctx)

    ctx.save()
    ctx.translate(this.viewport.x, this.viewport.y)
    ctx.scale(this.viewport.scale, this.viewport.scale)

    const visible = expandRect(this.viewport.visibleWorldRect(), 48)
    for (const shape of input.shapes) {
      if (shape.deleted) continue
      if (!rectsIntersect(shapeBounds(shape), visible)) continue
      this.drawShape(ctx, shape)
    }
    ctx.restore()
  }

  /**
   * 网格：用 pattern 平铺而非逐点 fillRect。
   * 1000+ 个 dot 逐个 arc 在 Retina 下要几毫秒，pattern 只是一次 fillRect，
   * 由合成器负责平铺，成本恒定。pattern 按「平铺像素尺寸」缓存，
   * 平移时只更新 transform，不重建位图。
   */
  private drawGrid(ctx: CanvasRenderingContext2D): void {
    const spacing = this.gridSpacing()
    const step = spacing * this.viewport.scale * this.dpr
    if (!Number.isFinite(step) || step < 4) return
    const size = Math.ceil(step)
    if (size <= 0 || size > 2048) return

    const key = `${size}:${this.dpr >= 2 ? 'hi' : 'lo'}`
    if (!this.gridTile || this.gridTile.key !== key) {
      const tile = document.createElement('canvas')
      tile.width = size
      tile.height = size
      const tileCtx = tile.getContext('2d')
      if (!tileCtx) return
      tileCtx.fillStyle = GRID_DOT
      const radius = this.dpr >= 2 ? 1.1 : 1
      tileCtx.beginPath()
      tileCtx.arc(0.5, 0.5, radius, 0, Math.PI * 2)
      tileCtx.fill()
      const pattern = ctx.createPattern(tile, 'repeat')
      if (!pattern) return
      this.gridTile = { key, pattern }
    }

    // 让点阵跟随世界原点平移（否则平移时点会「游走」）
    const originX = this.viewport.x * this.dpr
    const originY = this.viewport.y * this.dpr
    const offsetX = ((originX % step) + step) % step
    const offsetY = ((originY % step) + step) % step
    this.gridTile.pattern.setTransform(new DOMMatrix([1, 0, 0, 1, offsetX - step, offsetY - step]))

    ctx.save()
    ctx.fillStyle = this.gridTile.pattern
    ctx.fillRect(0, 0, this.width, this.height)
    ctx.restore()
  }

  /** 网格间距随缩放跳档：屏幕上始终落在 15–46px，视觉密度恒定。 */
  private gridSpacing(): number {
    let spacing = 32
    const scale = this.viewport.scale
    for (let i = 0; i < 8 && spacing * scale < GRID_MIN_PX; i += 1) spacing *= 2
    for (let i = 0; i < 8 && spacing * scale > GRID_MAX_PX; i += 1) spacing /= 2
    return spacing
  }

  /* ------------------------------------------------------------------ *
   * 形状绘制
   * ------------------------------------------------------------------ */

  private pathFor(shape: Shape): Path2D {
    const cached = this.pathCache.get(shape.id)
    if (cached && cached.rev === shape.rev) return cached.path
    const path = new Path2D()
    switch (shape.type) {
      case 'freehand':
        traceSmoothPath(path, shape.points, shape.closed)
        break
      case 'rect':
        this.traceRect(path, shape)
        break
      case 'ellipse': {
        const r = shape.geom
        path.ellipse(
          r.x + r.w / 2,
          r.y + r.h / 2,
          Math.max(r.w / 2, 0.01),
          Math.max(r.h / 2, 0.01),
          0,
          0,
          Math.PI * 2,
        )
        break
      }
      case 'arrow':
        path.moveTo(shape.x1, shape.y1)
        path.lineTo(shape.x2, shape.y2)
        break
      default:
        break
    }
    this.pathCache.set(shape.id, { rev: shape.rev, path })
    return path
  }

  private traceRect(path: Path2D, shape: RectShape): void {
    const { x, y, w, h } = shape.geom
    const radius = Math.min(shape.radius, w / 2, h / 2)
    if (radius <= 0.5) {
      path.rect(x, y, w, h)
      return
    }
    path.moveTo(x + radius, y)
    path.lineTo(x + w - radius, y)
    path.quadraticCurveTo(x + w, y, x + w, y + radius)
    path.lineTo(x + w, y + h - radius)
    path.quadraticCurveTo(x + w, y + h, x + w - radius, y + h)
    path.lineTo(x + radius, y + h)
    path.quadraticCurveTo(x, y + h, x, y + h - radius)
    path.lineTo(x, y + radius)
    path.quadraticCurveTo(x, y, x + radius, y)
    path.closePath()
  }

  private drawShape(ctx: CanvasRenderingContext2D, shape: Shape): void {
    const { style } = shape
    ctx.save()
    ctx.globalAlpha = style.opacity * (shape.pending ? 0.6 : 1)
    ctx.strokeStyle = style.stroke
    ctx.lineWidth = style.strokeWidth
    ctx.lineJoin = 'round'
    ctx.lineCap = 'round'
    if (style.dash === 'dashed') ctx.setLineDash([style.strokeWidth * 3, style.strokeWidth * 2.4])
    else if (style.dash === 'dotted') ctx.setLineDash([0.1, style.strokeWidth * 2.2])

    if (shape.type === 'text') {
      this.drawText(ctx, shape)
      ctx.restore()
      return
    }

    const path = this.pathFor(shape)
    if (style.fill !== 'none' && (shape.type === 'rect' || shape.type === 'ellipse')) {
      ctx.fillStyle = style.fill
      ctx.fill(path)
    }
    ctx.stroke(path)

    if (shape.type === 'arrow') this.drawArrowHeads(ctx, shape)
    ctx.restore()
  }

  private drawArrowHeads(ctx: CanvasRenderingContext2D, shape: ArrowShape): void {
    const [a, tip, b] = arrowHeadPath(shape)
    if (!a || !tip || !b) return
    const head = new Path2D()
    head.moveTo(a.x, a.y)
    head.lineTo(tip.x, tip.y)
    head.lineTo(b.x, b.y)
    head.closePath()
    ctx.fillStyle = shape.style.stroke
    ctx.fill(head)

    if (shape.head === 'both') {
      const len = Math.hypot(shape.x2 - shape.x1, shape.y2 - shape.y1)
      const size = Math.min(18, Math.max(9, len * 0.28))
      const spread = Math.PI / 7
      const angle = Math.atan2(shape.y1 - shape.y2, shape.x1 - shape.x2)
      const start = new Path2D()
      start.moveTo(shape.x1 - size * Math.cos(angle - spread), shape.y1 - size * Math.sin(angle - spread))
      start.lineTo(shape.x1, shape.y1)
      start.lineTo(shape.x1 - size * Math.cos(angle + spread), shape.y1 - size * Math.sin(angle + spread))
      start.closePath()
      ctx.fill(start)
    }
  }

  private drawText(ctx: CanvasRenderingContext2D, shape: TextShape): void {
    const layout = this.layoutText(shape)
    const { x, y, w, h } = shape.geom

    if (shape.variant === 'note') {
      const path = new Path2D()
      const radius = 10
      path.moveTo(x + radius, y)
      path.lineTo(x + w - radius, y)
      path.quadraticCurveTo(x + w, y, x + w, y + radius)
      path.lineTo(x + w, y + h - radius)
      path.quadraticCurveTo(x + w, y + h, x + w - radius, y + h)
      path.lineTo(x + radius, y + h)
      path.quadraticCurveTo(x, y + h, x, y + h - radius)
      path.lineTo(x, y + radius)
      path.quadraticCurveTo(x, y, x + radius, y)
      path.closePath()

      ctx.save()
      ctx.setLineDash([])
      ctx.fillStyle = shape.style.fill === 'none' ? '#FCF3D9' : shape.style.fill
      ctx.fill(path)
      ctx.fillStyle = 'rgba(22, 24, 29, 0.06)'
      ctx.fill(new Path2D(path))
      ctx.strokeStyle = 'rgba(138, 122, 74, 0.22)'
      ctx.lineWidth = 1
      ctx.stroke(path)
      // 便签顶部 3px 色条：暗示「这是可以拖动的一块纸」
      ctx.clip(path)
      ctx.fillStyle = 'rgba(212, 160, 23, 0.32)'
      ctx.fillRect(x, y, w, 3)
      ctx.restore()

      const padX = 14
      const padY = 16
      ctx.save()
      ctx.font = layout.font
      ctx.textBaseline = 'alphabetic'
      ctx.clip(path)
      if (!shape.text) {
        ctx.fillStyle = 'rgba(91, 96, 112, 0.55)'
        ctx.fillText('双击编辑…', x + padX, y + padY + layout.lineHeight * 0.72)
      } else {
        // 只画放得下的行，并裁到便签内部：宁可少画一行，也不让文字溢出纸面
        const maxLines = Math.max(1, Math.floor((h - padY * 2) / layout.lineHeight) + 1)
        const count = Math.min(layout.lines.length, maxLines)
        ctx.fillStyle = '#16181D'
        for (let i = 0; i < count; i += 1) {
          ctx.fillText(layout.lines[i] ?? '', x + padX, y + padY + layout.lineHeight * (i + 0.72))
        }
      }
      ctx.restore()
      return
    }

    ctx.save()
    ctx.font = layout.font
    ctx.textBaseline = 'alphabetic'
    ctx.fillStyle = shape.text ? shape.style.stroke : 'rgba(91, 96, 112, 0.5)'
    const maxLines = Math.max(1, Math.floor(h / layout.lineHeight) + 1)
    const count = Math.min(layout.lines.length, maxLines)
    for (let i = 0; i < count; i += 1) {
      ctx.fillText(layout.lines[i] ?? '', x, y + layout.lineHeight * (i + 0.8))
    }
    ctx.restore()
  }

  /** 文本按字符换行（中文没有空格，按词换行不适用），结果缓存。 */
  private layoutText(shape: TextShape): TextLayout {
    const version = `${shape.text.length}:${shape.text.slice(0, 8)}:${shape.geom.w}:${shape.style.strokeWidth}:${shape.variant}`
    const cached = this.textCache.get(shape.id)
    if (cached && cached.version === version) return cached

    /**
     * 字号：
     *  - 普通 text 遵守协议 §6.4「fontSize = strokeWidth * 6」；
     *  - 便签是叠加在 text 上的展示变体（data.variant），用固定 16px 正文，
     *    否则默认笔宽 4 会算出 24px，两行就撑破 180px 高的便签。
     */
    const fontSize = shape.variant === 'note' ? 16 : Math.max(12, shape.style.strokeWidth * 6)
    const font = `${fontSize}px ${FONT_STACK}`
    const lineHeight = Math.round(fontSize * 1.6)
    const padX = shape.variant === 'note' ? 28 : 0
    const maxWidth = Math.max(24, shape.geom.w - padX)

    if (!this.measureCtx) this.measureCtx = document.createElement('canvas').getContext('2d')
    const measure = this.measureCtx
    const lines: string[] = []
    if (measure) {
      measure.font = font
      const wrapped = wrapText(shape.text, {
        maxWidth,
        measure: (text) => measure.measureText(text).width,
      })
      lines.push(...wrapped)
    } else {
      lines.push(shape.text)
    }

    const layout: TextLayout = { lines, font, lineHeight, version }
    this.textCache.set(shape.id, layout)
    return layout
  }

  /* ------------------------------------------------------------------ *
   * overlay 层
   * ------------------------------------------------------------------ */

  private drawOverlay(): void {
    const ctx = this.overlayCtx
    const input = this.getInput()
    ctx.setTransform(1, 0, 0, 1, 0, 0)
    ctx.clearRect(0, 0, this.overlay.width, this.overlay.height)
    ctx.setTransform(this.dpr, 0, 0, this.dpr, 0, 0)

    if (input.draft) {
      ctx.save()
      ctx.translate(this.viewport.x, this.viewport.y)
      ctx.scale(this.viewport.scale, this.viewport.scale)
      this.drawShape(ctx, input.draft)
      ctx.restore()
    }

    if (input.selection.size > 0 && !input.readOnly) {
      ctx.save()
      ctx.translate(this.viewport.x, this.viewport.y)
      ctx.scale(this.viewport.scale, this.viewport.scale)
      const scale = this.viewport.scale
      ctx.setLineDash([])
      for (const shape of input.shapes) {
        if (!input.selection.has(shape.id) || shape.deleted) continue
        const box = expandRect(shapeBounds(shape), SELECTION_PAD / scale)
        ctx.strokeStyle = '#3D5AFE'
        ctx.lineWidth = 1.5 / scale
        ctx.strokeRect(box.x, box.y, box.w, box.h)
        if (input.selection.size === 1) this.drawHandles(ctx, box, scale)
      }
      ctx.restore()
    }

    if (input.marquee) {
      const screen = this.viewport.worldRectToScreen(input.marquee)
      ctx.save()
      ctx.fillStyle = 'rgba(61, 90, 254, 0.06)'
      ctx.strokeStyle = 'rgba(61, 90, 254, 0.72)'
      ctx.lineWidth = 1
      ctx.fillRect(screen.x, screen.y, screen.w, screen.h)
      ctx.strokeRect(screen.x + 0.5, screen.y + 0.5, screen.w, screen.h)
      ctx.restore()
    }

    for (const cursor of input.cursors) this.drawCursor(ctx, cursor)
  }

  private drawHandles(ctx: CanvasRenderingContext2D, box: Rect, scale: number): void {
    const size = 7 / scale
    const half = size / 2
    const points: Point[] = [
      { x: box.x, y: box.y },
      { x: box.x + box.w, y: box.y },
      { x: box.x + box.w, y: box.y + box.h },
      { x: box.x, y: box.y + box.h },
    ]
    ctx.fillStyle = '#FFFFFF'
    ctx.strokeStyle = '#3D5AFE'
    ctx.lineWidth = 1.5 / scale
    for (const p of points) {
      ctx.beginPath()
      ctx.rect(p.x - half, p.y - half, size, size)
      ctx.fill()
      ctx.stroke()
    }
  }

  private drawCursor(ctx: CanvasRenderingContext2D, cursor: CursorView): void {
    const p = this.viewport.worldToScreen({ x: cursor.x, y: cursor.y })
    if (p.x < -90 || p.y < -50 || p.x > this.width + 90 || p.y > this.height + 50) return

    ctx.save()
    ctx.translate(p.x, p.y)
    const path = new Path2D()
    path.moveTo(0, 0)
    path.lineTo(0, 15)
    path.lineTo(4.2, 11.4)
    path.lineTo(7.2, 17.6)
    path.lineTo(9.8, 16.2)
    path.lineTo(6.9, 10.2)
    path.lineTo(12, 10)
    path.closePath()
    ctx.fillStyle = cursor.color
    ctx.strokeStyle = '#FFFFFF'
    ctx.lineWidth = 1.5
    ctx.lineJoin = 'round'
    ctx.fill(path)
    ctx.stroke(path)

    ctx.font = `500 11px ${FONT_STACK}`
    const labelW = ctx.measureText(cursor.name).width + 12
    const labelH = 18
    const lx = 12
    const ly = 14
    ctx.fillStyle = cursor.color
    this.roundRect(ctx, lx, ly, labelW, labelH, 6)
    ctx.fill()
    ctx.fillStyle = '#FFFFFF'
    ctx.textBaseline = 'middle'
    ctx.fillText(cursor.name, lx + 6, ly + labelH / 2 + 0.5)
    ctx.restore()
  }

  private roundRect(
    ctx: CanvasRenderingContext2D,
    x: number,
    y: number,
    w: number,
    h: number,
    r: number,
  ): void {
    const radius = Math.min(r, w / 2, h / 2)
    ctx.beginPath()
    ctx.moveTo(x + radius, y)
    ctx.lineTo(x + w - radius, y)
    ctx.quadraticCurveTo(x + w, y, x + w, y + radius)
    ctx.lineTo(x + w, y + h - radius)
    ctx.quadraticCurveTo(x + w, y + h, x + w - radius, y + h)
    ctx.lineTo(x + radius, y + h)
    ctx.quadraticCurveTo(x, y + h, x, y + h - radius)
    ctx.lineTo(x, y + radius)
    ctx.quadraticCurveTo(x, y, x + radius, y)
    ctx.closePath()
  }

  /** 导出当前视口为 PNG（P1 预留）。 */
  exportViewportPng(background = '#F7F6F2'): string {
    const out = document.createElement('canvas')
    out.width = this.base.width
    out.height = this.base.height
    const ctx = out.getContext('2d')
    if (!ctx) return this.base.toDataURL('image/png')
    ctx.fillStyle = background
    ctx.fillRect(0, 0, out.width, out.height)
    ctx.drawImage(this.base, 0, 0)
    ctx.drawImage(this.overlay, 0, 0)
    return out.toDataURL('image/png')
  }
}
