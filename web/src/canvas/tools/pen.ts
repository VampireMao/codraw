import { createFreehand, recomputeFreehandGeom, type FreehandShape } from '../shapes'
import { shouldKeepPoint, type InkPoint } from '../smoothing'
import type { PointerSample, Tool, ToolHost } from './types'

/** 抽稀阈值（世界单位）。1.2px 在 100% 缩放下几乎无损，同时去掉 40%+ 冗余点。 */
const MIN_DIST = 1.2
/** 协议 §6.1：单 shape 点数上限 4096，超过则分段（这里取 3800 留余量）。 */
const MAX_POINTS = 3800
/**
 * 落笔过程中的增量上报节奏。
 *
 * 别人看到的线条要"跟着笔尖长出来"，而不是等松手才出现；但每个采样点都发一条
 * 消息会在快速书写时产生 60+ msg/s/人。取 100ms 或 24 个点先到者为准：
 * 100ms 的延迟人眼几乎察觉不到，而消息量只有逐点上报的 1/6 左右。
 */
const APPEND_INTERVAL_MS = 100
const APPEND_BATCH_POINTS = 24

/**
 * 自由笔：落笔即渲染（overlay 草稿），并在落笔过程中按批上报 append，
 * 让协作者实时看到线条延伸；松手时提交最终形状。
 *
 * 采样点抽稀后写入 shape；平滑在渲染期用二次贝塞尔完成（不在数据层改变几何）。
 */
export class PenTool implements Tool {
  readonly id = 'pen' as const
  readonly cursor = 'crosshair' as const

  private active: FreehandShape | null = null
  /** 已上报给服务端的点数；append 只发这之后新增的部分。 */
  private sentCount = 0
  private lastFlushAt = 0

  onPointerDown(sample: PointerSample, host: ToolHost): void {
    if (!host.canEdit) return
    const point: InkPoint =
      sample.pointerType === 'pen'
        ? [sample.world.x, sample.world.y, sample.pressure]
        : [sample.world.x, sample.world.y]
    this.active = createFreehand([point], {
      style: { stroke: host.style.stroke, strokeWidth: host.style.strokeWidth },
    })
    this.sentCount = 0
    this.lastFlushAt = host.now()
    host.beginGesture()
    host.setDraft(this.active)
    // 落笔立刻 create：远端必须先有这个 id，后续 append 才有对象可追加。
    // 只带首个点时形状是"一个点"，视觉上是个圆点，随后 append 把它拉长。
    host.createDraftShape(this.active)
  }

  onPointerMove(sample: PointerSample, host: ToolHost): void {
    const shape = this.active
    if (!shape) return
    const point: InkPoint =
      sample.pointerType === 'pen'
        ? [sample.world.x, sample.world.y, sample.pressure]
        : [sample.world.x, sample.world.y]
    const last = shape.points[shape.points.length - 1]
    if (!shouldKeepPoint(last, point, MIN_DIST)) return
    shape.points.push(point)
    recomputeFreehandGeom(shape)
    shape.rev += 1
    host.setDraft(shape)

    // 达阈值就把新增点发给服务端。注意 sentCount 起点是 0 而非 1：
    // 首个点包含在第一次 append 里，避免远端要先收到 create 才知道这个 id。
    const unsent = shape.points.length - this.sentCount
    const now = host.now()
    if (unsent >= APPEND_BATCH_POINTS || now - this.lastFlushAt >= APPEND_INTERVAL_MS) {
      this.flush(shape, host, now)
    }
  }

  onPointerUp(_sample: PointerSample, host: ToolHost): void {
    const shape = this.active
    this.active = null
    if (!shape) return
    host.setDraft(null)

    // 形状在落笔时已经 create 过了，这里只补发尾巴的 append。
    // 不再调用 addShape：那会重复发一条 create，服务端会回 duplicate_shape。
    this.flush(shape, host, host.now())

    host.endGesture()
    if (shape.points.length > 1) host.afterShapeCommitted('pen')
  }

  onPointerCancel(host: ToolHost): void {
    if (this.active) {
      host.setDraft(null)
      host.endGesture()
    }
    this.active = null
    this.sentCount = 0
  }

  onDeactivate(host: ToolHost): void {
    this.onPointerCancel(host)
  }

  /** 把 [sentCount, points.length) 这段点作为一条 append 发出。 */
  private flush(shape: FreehandShape, host: ToolHost, now: number): void {
    if (shape.points.length <= this.sentCount) return
    const delta: number[][] = []
    for (let i = this.sentCount; i < shape.points.length; i += 1) {
      const p = shape.points[i]
      if (!p) continue
      const point: number[] = [p[0], p[1]]
      if (p.length > 2 && typeof p[2] === 'number') point.push(p[2])
      delta.push(point)
    }
    this.sentCount = shape.points.length
    this.lastFlushAt = now
    if (delta.length > 0) host.appendPoints(shape.id, delta)
  }
}

export { MAX_POINTS }
