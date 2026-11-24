import { describe, expect, it } from 'vitest'
import {
  arrowHeadPath,
  createArrow,
  createEllipse,
  createFreehand,
  createNote,
  createRect,
  createText,
  defaultStyle,
  fromWire,
  hitTest,
  moveShape,
  sceneBounds,
  shapeBounds,
  toWire,
  withAlpha,
  isLightColor,
  NOTE_FILL,
} from '@/canvas/shapes'
import {
  distToSegment,
  rectFromPoints,
  normalizeRect,
  round1,
  unionRect,
  expandRect,
} from '@/canvas/geometry'

const P = (x: number, y: number) => ({ x, y })

describe('图形构造与几何', () => {
  it('矩形从任意方向拖拽都得到规整 geom', () => {
    const r1 = createRect(P(100, 50), P(200, 150))
    expect(r1.geom).toEqual({ x: 100, y: 50, w: 100, h: 100 })

    const r2 = createRect(P(200, 150), P(100, 50))
    expect(r2.geom).toEqual({ x: 100, y: 50, w: 100, h: 100 })
  })

  it('自由笔依据点集重算包围盒', () => {
    const s = createFreehand([
      [10, 20],
      [110, 20],
      [60, 220],
    ])
    expect(s.geom).toEqual({ x: 10, y: 20, w: 100, h: 200 })
  })

  it('箭头保留端点方向（geom 允许负方向，用 x1..y2 覆盖）', () => {
    const a = createArrow(P(300, 300), P(100, 100))
    expect(a.x1).toBe(300)
    expect(a.y1).toBe(300)
    expect(a.x2).toBe(100)
    expect(a.y2).toBe(100)
    // geom 仍归一化为正矩形
    expect(a.geom).toEqual({ x: 100, y: 100, w: 200, h: 200 })
  })

  it('便签默认带纸黄色填充与合理尺寸', () => {
    const n = createNote(P(0, 0))
    expect(n.type).toBe('text')
    expect(n.variant).toBe('note')
    expect(n.style.fill).toBe(NOTE_FILL)
    expect(n.geom.w).toBeGreaterThan(200)
    expect(n.text).toBe('')
  })

  it('moveShape 同步平移 geom、点集与箭头端点', () => {
    const pen = createFreehand([
      [0, 0],
      [10, 10],
    ])
    moveShape(pen, 5, -5)
    expect(pen.geom).toMatchObject({ x: 5, y: -5 })
    expect(pen.points[0]).toEqual([5, -5])
    expect(pen.rev).toBe(2)

    const arrow = createArrow(P(0, 0), P(10, 10))
    moveShape(arrow, 3, 4)
    expect([arrow.x1, arrow.y1, arrow.x2, arrow.y2]).toEqual([3, 4, 13, 14])
  })

  it('shapeBounds 含笔宽余量，sceneBounds 聚合全部图形', () => {
    const a = createFreehand([[0, 0]], { style: { strokeWidth: 10 } })
    const b = createRect(P(100, 100), P(200, 200))
    const bounds = shapeBounds(a)
    expect(bounds.x).toBeCloseTo(-6, 6)
    expect(bounds.w).toBeCloseTo(12, 6)

    const scene = sceneBounds([a, b])
    expect(scene).not.toBeNull()
    if (!scene) throw new Error('unreachable')
    expect(scene.x).toBeLessThanOrEqual(-6)
    expect(scene.x + scene.w).toBeGreaterThanOrEqual(201)
  })

  it('sceneBounds 忽略已删除图形', () => {
    const a = createRect(P(0, 0), P(10, 10))
    a.deleted = true
    expect(sceneBounds([a])).toBeNull()
  })
})

describe('命中测试', () => {
  const rect = createRect(P(100, 100), P(200, 180))

  it('点在矩形描边上命中，远处不命中', () => {
    expect(hitTest(rect, P(100, 140), 2)).toBe(true)
    expect(hitTest(rect, P(150, 100), 2)).toBe(true)
    expect(hitTest(rect, P(400, 400), 2)).toBe(false)
  })

  it('空心矩形内部不命中，实心命中', () => {
    expect(hitTest(rect, P(150, 140), 2)).toBe(false)
    rect.style.fill = '#ff0000'
    expect(hitTest(rect, P(150, 140), 2)).toBe(true)
  })

  it('自由笔命中仅靠近折线（不填满包围盒）', () => {
    const pen = createFreehand([
      [0, 0],
      [100, 0],
    ])
    expect(hitTest(pen, P(50, 0), 2)).toBe(true)
    expect(hitTest(pen, P(50, 60), 2)).toBe(false)
  })

  it('箭头命中线段与箭头头部', () => {
    const arrow = createArrow(P(0, 0), P(200, 0))
    expect(hitTest(arrow, P(100, 0), 2)).toBe(true)
    expect(hitTest(arrow, P(100, 40), 2)).toBe(false)
    const head = arrowHeadPath(arrow)
    const tip = head[1]
    expect(tip).toBeDefined()
    expect(hitTest(arrow, tip!, 2)).toBe(true)
  })

  it('便签命中整个矩形区域', () => {
    const note = createNote(P(0, 0))
    expect(hitTest(note, P(20, 20), 0)).toBe(true)
    expect(hitTest(note, P(9999, 9999), 0)).toBe(false)
  })

  it('已删除图形永不命中', () => {
    const s = createEllipse(P(0, 0), P(100, 100))
    s.deleted = true
    expect(hitTest(s, P(50, 50), 8)).toBe(false)
  })

  it('容差随缩放变化：8px 容差在 400% 缩放下更严格', () => {
    const pen = createFreehand([
      [0, 0],
      [100, 0],
    ])
    // 世界坐标 10 的偏移
    expect(hitTest(pen, P(50, 10), 12)).toBe(true)
    expect(hitTest(pen, P(50, 10), 4)).toBe(false)
  })
})

describe('序列化', () => {
  it('自由笔 round-trip 保留点数与坐标', () => {
    const shape = createFreehand([
      [1.24, 2.35],
      [10.06, 20.99],
    ])
    const wire = toWire(shape)
    // 协议要求 1 位小数
    expect(wire.data.points).toEqual([
      [1.2, 2.4],
      [10.1, 21],
    ])
    const back = fromWire(wire)
    expect(back).not.toBeNull()
    expect(back?.type).toBe('freehand')
    if (back?.type === 'freehand') {
      expect(back.points.length).toBe(2)
      expect(back.points[0]?.[0]).toBeCloseTo(1.2, 6)
    }
    expect(back?.id).toBe(shape.id)
    expect(back?.rev).toBe(shape.rev)
  })

  it('序列化剔除本地瞬态字段（pending 不上行）', () => {
    const shape = createRect(P(0, 0), P(10, 10))
    shape.pending = true
    const wire = toWire(shape) as unknown as Record<string, unknown>
    expect('pending' in wire).toBe(false)
  })

  it('便签的 text 在顶层，variant 在 data 内，kind 用协议字段名', () => {
    const note = createNote(P(10, 20))
    note.text = '中文便签'
    const wire = toWire(note)
    // 契约：字段名是 shape.kind，不是 shape.type（PROTOCOL §5.1）
    expect(wire.kind).toBe('text')
    expect(wire.text).toBe('中文便签')
    expect(wire.data.variant).toBe('note')
    const back = fromWire(wire)
    expect(back?.type === 'text' && back.text).toBe('中文便签')
    expect(back?.type === 'text' && back.variant).toBe('note')
  })

  it('箭头序列化后端点可复原', () => {
    const arrow = createArrow(P(30, 40), P(130, 240))
    const back = fromWire(toWire(arrow))
    expect(back?.type).toBe('arrow')
    if (back?.type === 'arrow') {
      expect([back.x1, back.y1, back.x2, back.y2]).toEqual([30, 40, 130, 240])
    }
  })

  it('非法输入不抛异常并返回 null', () => {
    expect(fromWire(null)).toBeNull()
    expect(fromWire({ type: 'blob' })).toBeNull()
    expect(fromWire({ type: 'rect' })).toBeNull() // 缺 id
    expect(fromWire({ id: 'sh_x', type: 'unknown-type' })).toBeNull()
  })

  it('未知 type 不渲染但保留不崩（协议 §9 前向兼容）', () => {
    const raw = { id: 'sh_abc123', type: 'star', geom: { x: 0, y: 0, w: 1, h: 1 } }
    expect(fromWire(raw)).toBeNull()
  })

  it('样式越界值被钳制而非拒绝', () => {
    const back = fromWire({
      id: 'sh_abc123',
      type: 'rect',
      geom: { x: 0, y: 0, w: 10, h: 10 },
      style: { stroke: '#000000', strokeWidth: 999, opacity: 0, dash: 'weird' },
      rev: 1,
    })
    expect(back?.style.strokeWidth).toBe(32)
    expect(back?.style.opacity).toBe(0.1)
    expect(back?.style.dash).toBe('solid')
  })
})

describe('纯函数工具', () => {
  it('round1 按协议保留 1 位小数', () => {
    expect(round1(1.249)).toBe(1.2)
    expect(round1(1.25)).toBe(1.3)
    expect(round1(-2.04)).toBe(-2)
  })

  it('rectFromPoints / normalizeRect 处理反向拖拽', () => {
    expect(normalizeRect(rectFromPoints(P(10, 10), P(0, 0)))).toEqual({ x: 0, y: 0, w: 10, h: 10 })
    expect(normalizeRect({ x: 10, y: 10, w: -10, h: -5 })).toEqual({ x: 0, y: 5, w: 10, h: 5 })
  })

  it('unionRect 与 expandRect', () => {
    expect(unionRect({ x: 0, y: 0, w: 10, h: 10 }, { x: 5, y: 5, w: 20, h: 20 })).toEqual({
      x: 0,
      y: 0,
      w: 25,
      h: 25,
    })
    expect(expandRect({ x: 5, y: 5, w: 10, h: 10 }, 2)).toEqual({ x: 3, y: 3, w: 14, h: 14 })
  })

  it('点到线段距离在端点处截断', () => {
    expect(distToSegment(P(0, 0), P(0, 10), P(10, 10))).toBeCloseTo(10, 9)
    expect(distToSegment(P(5, 5), P(0, 5), P(10, 5))).toBeCloseTo(0, 9)
    expect(distToSegment(P(5, 8), P(0, 5), P(10, 5))).toBeCloseTo(3, 9)
  })

  it('withAlpha 输出 rgba，非法输入原样返回', () => {
    expect(withAlpha('#ff0000', 0.5)).toBe('rgba(255, 0, 0, 0.5)')
    expect(withAlpha('red', 0.5)).toBe('red')
  })

  it('isLightColor 区分纸色与墨色', () => {
    expect(isLightColor('#FFFFFF')).toBe(true)
    expect(isLightColor('#16181D')).toBe(false)
  })

  it('defaultStyle 返回独立副本（避免共享引用）', () => {
    const a = defaultStyle()
    const b = defaultStyle()
    a.strokeWidth = 8
    expect(b.strokeWidth).toBe(2)
  })

  it('createText 保持纯文本变体', () => {
    const t = createText(P(0, 0), '你好')
    expect(t.variant).toBe('plain')
    expect(t.text).toBe('你好')
  })
})
