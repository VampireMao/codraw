/** * BoardCanvas：画布引擎与 Vue 的唯一接口。 * * 职责边界（刻意做窄）： * -
引擎只管「世界坐标里的几何」；DOM 浮层（便签编辑）归 Vue； * -
组件负责建引擎、转发宿主能力、把交互变成「op」，再交给上层提交； * - **不直接发网络**：通过 `emit('commit',
op)` 让 RoomView 转给 realtime 层。 * 这样 Phase B 的网络时序（乐观渲染 / pending / ack）完全不污染交互代码。
*/
<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useBoardStore } from '@/stores/board'
import { History } from '@/canvas/history'
import { createTool } from '@/canvas/tools'
import { NoteTool } from '@/canvas/tools/note'
import { hitTest, toWire, type Shape } from '@/canvas/shapes'
import { pickShape } from '@/canvas/tools/types'
import type { PointerSample, Tool, ToolHost, ToolId } from '@/canvas/tools/types'
import { Renderer, type CursorView, type RenderInput } from '@/canvas/renderer'
import { Viewport, MAX_SCALE, MIN_SCALE } from '@/canvas/viewport'
import { clamp, type Point, type Rect } from '@/canvas/geometry'
import type { OpAddPayload } from '@/net/types'

const props = defineProps<{
  readOnly: boolean
  /** 已连上服务端（online）才上报光标、才把工具切到可绘制 */
  connected: boolean
}>()

const emit = defineEmits<{
  /** 视口缩放百分比变化 */
  zoom: [percent: number]
  /** 撤销栈可用性变化 */
  history: [state: { canUndo: boolean; canRedo: boolean }]
  /** 只读态下用户表达了编辑意图 */
  readonlyAttempt: []
  /** 本地产生一条写 op（由上层交给 realtime 提交） */
  commit: [op: OpAddPayload, shapeIds: string[]]
  /** 光标世界坐标（上层 20Hz 节流后上报） */
  cursor: [x: number, y: number]
}>()

const store = useBoardStore()
const hostEl = ref<HTMLDivElement | null>(null)

let viewport: Viewport | null = null
let renderer: Renderer | null = null
let tool: Tool = createTool('select')
const history = new History()

const draft = ref<Shape | null>(null)
const marquee = ref<Rect | null>(null)
const remoteCursors = ref<Map<string, CursorView & { at: number }>>(new Map())
const interacting = ref(false)
const cursorStyle = ref('default')
const toolCursor = ref('default')
let spaceHeld = false

/* ----------------------------- 手势：多点触控 ----------------------------- */

interface ActivePointer {
  id: number
  screen: Point
  type: string
}
const activePointers = new Map<number, ActivePointer>()
let pinch: { dist: number; center: Point; scale: number } | null = null
let panning = false
let lastPanScreen: Point | null = null

/* -------------------------------- op id --------------------------------- */

/** 生成 opId：协议要求 `^[A-Za-z0-9_-]{8,40}$`（与 shape id 规则一致）。 */
function createOpId(): string {
  const bytes = new Uint8Array(12)
  if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') {
    crypto.getRandomValues(bytes)
  } else {
    for (let i = 0; i < bytes.length; i += 1) bytes[i] = Math.floor(Math.random() * 256)
  }
  let tail = ''
  for (let i = 0; i < bytes.length; i += 1) tail += (bytes[i] ?? 0).toString(16).padStart(2, '0')
  return `op_${tail}`
}

/* -------------------------------- 工具宿主 -------------------------------- */

/** 手势开始时的场景快照（用于撤销 + 计算本手势产生了哪些 op） */
let gestureSnapshot: Shape[] | null = null
/** 本轮手势中已提交过的 update op（同一 shape 只发最后一次，减少 op 数） */
const gestureTouched = new Set<string>()

const host: ToolHost = {
  get shapes() {
    return store.shapes
  },
  get selection() {
    return store.selection
  },
  get style() {
    // 用 effectiveInkColor 而不是 style.stroke：进入房间后画笔颜色跟随头像色，
    // 即使 inkColor 尚未初始化也有确定的默认值。
    return { stroke: store.effectiveInkColor(), strokeWidth: store.style.strokeWidth }
  },
  get canEdit() {
    return !props.readOnly && store.canEdit
  },
  addShape(shape) {
    store.addShape(shape)
    commitCreate(shape)
  },
  touchShape(shape) {
    store.touch()
    renderer?.invalidateScene()
    // 拖动 60fps 路径：只记下 id，松手时统一发一条 update（见 endGesture）
    gestureTouched.add(shape.id)
  },
  appendPoints(id, points) {
    // 落笔过程中的增量点：发 append op 让协作者实时看到线条延伸。
    // 本地已经画出来了（draft/overlay），这里只负责同步，不重复写本地。
    if (points.length === 0) return
    emit('commit', { opId: createOpId(), action: 'append', id, points: points.map((p) => [...p]) }, [id])
  },
  createDraftShape(shape) {
    // 落笔即建形：先入本地 store（这样后续 append 的 id 一定存在），
    // 再发 create。远端收到 create 后才有对象可 append。
    store.addShape(shape)
    renderer?.invalidateScene()
    commitCreate(shape)
  },
  now() {
    return Date.now()
  },
  removeShapes(ids) {
    // 工具（橡皮/删除）删图形：必须发 delete op，否则服务端仍保留，
    // 刷新或他人重进房间时被"复活"。这是数据一致性问题，不是纯 UI 行为。
    if (ids.length === 0) return
    for (const id of ids) store.markDeleted(id)
    store.removeShapes(ids)
    renderer?.invalidateScene()
    commitDelete(ids)
  },
  removeShapesLocal(ids) {
    // 仅本地移除，不发 op。给"调用方自己负责发 delete"的场景用
    // （便签取消/清空），避免同一次删除发出两条 op。
    if (ids.length === 0) return
    for (const id of ids) store.markDeleted(id)
    store.removeShapesLocal(ids)
    renderer?.invalidateScene()
  },
  replaceShapes(next) {
    store.setShapes([...next])
    renderer?.pruneCaches(next)
    renderer?.invalidateScene()
  },
  setSelection(ids) {
    store.setSelection(ids)
  },
  clearSelection() {
    store.clearSelection()
  },
  setDraft(next) {
    draft.value = next
    renderer?.invalidateOverlay()
  },
  setMarquee(rect) {
    marquee.value = rect
    renderer?.invalidateOverlay()
  },
  tolerance(px) {
    return viewport ? viewport.screenToWorldLength(px) : px
  },
  beginGesture() {
    gestureSnapshot = deepCloneAll(store.shapes)
    gestureTouched.clear()
    interacting.value = true
  },
  endGesture() {
    commitGesture()
    interacting.value = false
  },
  openNoteEditor(shape) {
    openEditor(shape.id)
  },
  afterShapeCommitted(_toolId: ToolId) {
    // UX-SPEC §7.4：画完保持当前工具，便于连续画同类图形。
    // 工具切换由用户显式操作（点工具条或快捷键）决定，不自动改变。
  },
}

/* ------------------------------ op 提交助手 ------------------------------ */

function commitCreate(shape: Shape): void {
  emit('commit', { opId: createOpId(), action: 'create', shape: toWire(shape) as unknown as Shape }, [
    shape.id,
  ])
}

function commitUpdate(shape: Shape): void {
  emit('commit', { opId: createOpId(), action: 'update', shape: toWire(shape) as unknown as Shape }, [
    shape.id,
  ])
}

function commitDelete(ids: readonly string[]): void {
  for (const id of ids) {
    emit('commit', { opId: createOpId(), action: 'delete', id }, [id])
  }
}

/* -------------------------------- 撤销历史 -------------------------------- */

function deepCloneShape(shape: Shape): Shape {
  const copy = { ...shape, style: { ...shape.style }, geom: { ...shape.geom } } as Shape
  if (copy.type === 'freehand') {
    copy.points = shape.type === 'freehand' ? shape.points.map((p) => [...p] as typeof p) : []
  }
  return copy
}

function deepCloneAll(shapes: readonly Shape[]): Shape[] {
  return shapes.map(deepCloneShape)
}

function sameScene(a: readonly Shape[], b: readonly Shape[]): boolean {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i += 1) {
    const x = a[i]
    const y = b[i]
    if (!x || !y || x.id !== y.id || x.rev !== y.rev) return false
  }
  return true
}

/** 一条历史记录：记住受影响的对象，撤销后只同步这些（不是整场景）。 */
function changedIds(before: readonly Shape[], after: readonly Shape[]): string[] {
  const ids = new Set<string>()
  const beforeMap = new Map(before.map((s) => [s.id, s]))
  for (const shape of after) {
    const prev = beforeMap.get(shape.id)
    if (!prev || prev !== shape) ids.add(shape.id)
  }
  const afterIds = new Set(after.map((s) => s.id))
  for (const shape of before) {
    if (!afterIds.has(shape.id)) ids.add(shape.id)
  }
  return [...ids]
}

/**
 * 把「本地这些 shape 的当前状态」同步给服务端。
 *
 * 三种情况，不能用同一种 op 表达：
 *  1. 本地有、且撤销/重做前也活着 → update（内容变了）
 *  2. 本地没有 → delete（被撤掉/删掉）
 *  3. 本地有，但**撤销/重做前它是删除状态** → create（服务端已标 deleted，
 *     此时发 update 会得到 `shape_deleted` 并被客户端回滚，重做就"失效"）
 *
 * 第 3 点是重做功能失效的根因：撤销一次 create 会发 delete，服务端把该 shape
 * 标记为已删除；重做时本地恢复了图形，但服务端不会因为 update 而复活它。
 */
function syncShapes(ids: readonly string[], before?: readonly Shape[]): void {
  const beforeById = new Map((before ?? []).map((s) => [s.id, s]))
  for (const id of ids) {
    const shape = store.findShape(id)
    if (!shape || shape.deleted) {
      commitDelete([id])
      continue
    }
    // 该 shape 在本次变更前是「不存在或已删除」→ 服务端视角它需要重建
    const prev = beforeById.get(id)
    if (before !== undefined && (!prev || prev.deleted)) {
      commitCreate(shape)
    } else {
      commitUpdate(shape)
    }
  }
}

/** 记录一次可撤销变更，并在需要时同步到服务端。 */
function recordSceneChange(before: Shape[], after: Shape[], label: string): void {
  if (sameScene(before, after)) return
  const touched = changedIds(before, after)
  // 深拷贝一次，避免闭包持有活引用（后续 setShapes 会替换数组，但对象可能被复用）
  const snapshotBefore = before.map(deepCloneShape)
  const snapshotAfter = after.map(deepCloneShape)
  history.push({
    label,
    undo: () => {
      store.setShapes(snapshotBefore.map(deepCloneShape))
      renderer?.invalidateScene()
      // 传入「撤销前的状态」，让 syncShapes 知道哪些 shape 需要重建
      syncShapes(touched, snapshotAfter)
    },
    redo: () => {
      store.setShapes(snapshotAfter.map(deepCloneShape))
      renderer?.invalidateScene()
      syncShapes(touched, snapshotBefore)
    },
  })
}

/**
 * 手势收尾：
 *  1. 有拖动的图形各发一条 update op（一条 op 覆盖整段拖动，而不是 60 条）；
 *  2. 有净变化则入撤销栈。
 */
function commitGesture(): void {
  const before = gestureSnapshot
  gestureSnapshot = null
  if (!before) return

  // 拖动/样式变更：每个被 touch 的图形发一条 update（本地已是最终状态）
  for (const id of gestureTouched) {
    const shape = store.findShape(id)
    if (shape && !shape.deleted) commitUpdate(shape)
  }
  gestureTouched.clear()

  recordSceneChange(before, deepCloneAll(store.shapes), '编辑')
}

/** 撤销：只回滚本地 + 重发受影响 shape 的 op（协议无逆操作，靠 LWW 收敛）。 */
function undo(): void {
  if (history.undo()) {
    renderer?.invalidateScene()
  }
}

function redo(): void {
  if (history.redo()) {
    renderer?.invalidateScene()
  }
}

/* --------------------------------- 事件 --------------------------------- */

function toSample(event: PointerEvent | MouseEvent): PointerSample {
  const rect = hostEl.value?.getBoundingClientRect() ?? { left: 0, top: 0 }
  const screen = { x: event.clientX - rect.left, y: event.clientY - rect.top }
  const world = viewport ? viewport.screenToWorld(screen) : screen
  const pointerType = 'pointerType' in event ? event.pointerType : 'mouse'
  const pressure = 'pressure' in event ? event.pressure : 0.5
  return {
    world,
    screen,
    pressure: pressure > 0 ? pressure : 0.5,
    pointerType: pointerType === 'pen' ? 'pen' : pointerType === 'touch' ? 'touch' : 'mouse',
    shiftKey: event.shiftKey,
    altKey: event.altKey,
    timeStamp: event.timeStamp,
  }
}

function onPointerDown(event: PointerEvent): void {
  if (!hostEl.value || !viewport) return
  if (event.pointerType === 'mouse' && event.button !== 0 && event.button !== 1) return

  hostEl.value.setPointerCapture(event.pointerId)
  activePointers.set(event.pointerId, {
    id: event.pointerId,
    screen: { x: event.clientX, y: event.clientY },
    type: event.pointerType,
  })

  if (activePointers.size >= 2) {
    beginPinch()
    return
  }

  if (spaceHeld || event.button === 1) {
    event.preventDefault()
    startPan({ x: event.clientX, y: event.clientY })
    return
  }

  if (props.readOnly) {
    startPan({ x: event.clientX, y: event.clientY })
    return
  }

  tool.onPointerDown(toSample(event), host)
}

function onPointerMove(event: PointerEvent): void {
  if (!viewport) return
  const tracked = activePointers.get(event.pointerId)
  if (tracked) tracked.screen = { x: event.clientX, y: event.clientY }

  // 光标上报：仅在空闲（未绘制/未平移）时，避免拖动时刷屏
  if (activePointers.size === 0 && props.connected && !props.readOnly) {
    const sample = toSample(event)
    emit('cursor', sample.world.x, sample.world.y)
  }

  if (activePointers.size >= 2) {
    updatePinch()
    return
  }

  if (panning && lastPanScreen) {
    const dx = event.clientX - lastPanScreen.x
    const dy = event.clientY - lastPanScreen.y
    lastPanScreen = { x: event.clientX, y: event.clientY }
    viewport.panBy(dx, dy)
    return
  }

  if (props.readOnly) return
  tool.onPointerMove(toSample(event), host)
}

function onPointerUp(event: PointerEvent): void {
  activePointers.delete(event.pointerId)
  if (hostEl.value?.hasPointerCapture(event.pointerId)) {
    hostEl.value.releasePointerCapture(event.pointerId)
  }
  if (activePointers.size < 2) pinch = null

  if (panning) {
    stopPan()
    return
  }
  if (!props.readOnly) tool.onPointerUp(toSample(event), host)
}

function onPointerCancel(event: PointerEvent): void {
  activePointers.delete(event.pointerId)
  pinch = null
  stopPan()
  if (!props.readOnly) tool.onPointerCancel?.(host)
}

function onPointerLeave(event: PointerEvent): void {
  if (activePointers.has(event.pointerId)) return
  if (!props.readOnly) tool.onPointerCancel?.(host)
}

function startPan(screen: Point): void {
  panning = true
  lastPanScreen = screen
  interacting.value = true
  cursorStyle.value = 'grabbing'
}

function stopPan(): void {
  if (!panning) return
  panning = false
  lastPanScreen = null
  interacting.value = false
  cursorStyle.value = spaceHeld ? 'grab' : toolCursor.value
}

/* ------------------------------- 双指缩放 ------------------------------- */

function beginPinch(): void {
  if (!viewport) return
  const pts = [...activePointers.values()].slice(0, 2)
  const a = pts[0]
  const b = pts[1]
  if (!a || !b) return
  pinch = {
    dist: Math.hypot(a.screen.x - b.screen.x, a.screen.y - b.screen.y),
    center: { x: (a.screen.x + b.screen.x) / 2, y: (a.screen.y + b.screen.y) / 2 },
    scale: viewport.scale,
  }
  tool.onPointerCancel?.(host)
  interacting.value = true
}

function updatePinch(): void {
  if (!pinch || !viewport || !hostEl.value) return
  const pts = [...activePointers.values()].slice(0, 2)
  const a = pts[0]
  const b = pts[1]
  if (!a || !b) return
  const dist = Math.hypot(a.screen.x - b.screen.x, a.screen.y - b.screen.y)
  const center = { x: (a.screen.x + b.screen.x) / 2, y: (a.screen.y + b.screen.y) / 2 }
  const rect = hostEl.value.getBoundingClientRect()
  const localCenter = { x: center.x - rect.left, y: center.y - rect.top }
  const prevCenter = { x: pinch.center.x - rect.left, y: pinch.center.y - rect.top }

  const factor = pinch.dist > 0 ? clamp(dist / pinch.dist, 0.5, 2) : 1
  viewport.zoomAt(localCenter, pinch.scale * factor)
  viewport.panBy(localCenter.x - prevCenter.x, localCenter.y - prevCenter.y)
  pinch = { ...pinch, dist, center }
}

/* -------------------------------- 滚轮缩放 ------------------------------- */

let wheelRaf = 0
let wheelDelta = 0
let wheelAnchor: Point = { x: 0, y: 0 }

function onWheel(event: WheelEvent): void {
  if (!viewport || !hostEl.value) return
  event.preventDefault()
  const rect = hostEl.value.getBoundingClientRect()
  wheelAnchor = { x: event.clientX - rect.left, y: event.clientY - rect.top }

  if (event.ctrlKey || event.metaKey) {
    const unit = event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? 400 : 1
    wheelDelta += event.deltaY * unit
    if (!wheelRaf) wheelRaf = requestAnimationFrame(applyWheel)
  } else {
    const dx = event.shiftKey ? -event.deltaY : -event.deltaX
    const dy = event.shiftKey ? 0 : -event.deltaY
    viewport.panBy(dx, dy)
  }
}

function applyWheel(): void {
  wheelRaf = 0
  if (!viewport) return
  const factor = Math.exp(-wheelDelta * 0.0016)
  viewport.zoomAt(wheelAnchor, viewport.scale * factor)
  wheelDelta = 0
}

/* --------------------------------- 双击 --------------------------------- */

function onDoubleClick(event: MouseEvent): void {
  if (!viewport) return
  if (props.readOnly) {
    emit('readonlyAttempt')
    return
  }
  const sample = toSample(event)
  const hit = pickShape(store.shapes, sample.world, host.tolerance(8), hitTest)
  if (hit && hit.type === 'text') {
    store.setSelection([hit.id])
    openEditor(hit.id)
    return
  }
  new NoteTool(true).onPointerDown(sample, host)
}

/* -------------------------------- 键盘 --------------------------------- */

function isTypingTarget(target: EventTarget | null): boolean {
  return (
    target instanceof HTMLInputElement ||
    target instanceof HTMLTextAreaElement ||
    target instanceof HTMLSelectElement ||
    (target instanceof HTMLElement && target.isContentEditable)
  )
}

function onKeyDown(event: KeyboardEvent): void {
  if (isTypingTarget(event.target)) return

  if (event.code === 'Space' && !spaceHeld && !event.repeat) {
    spaceHeld = true
    cursorStyle.value = 'grab'
    event.preventDefault()
    return
  }

  const mod = event.ctrlKey || event.metaKey

  if (mod && event.key.toLowerCase() === 'z') {
    event.preventDefault()
    if (event.shiftKey) redo()
    else undo()
    return
  }

  if (mod && event.key.toLowerCase() === 'a') {
    event.preventDefault()
    store.selectAll()
    return
  }

  if (event.key === 'Delete' || event.key === 'Backspace') {
    if (props.readOnly || !store.canEdit) return
    event.preventDefault()
    deleteSelected()
    return
  }

  if (event.key === 'Escape') {
    if (store.editingNoteId) {
      cancelNote()
      return
    }
    store.clearSelection()
    return
  }

  if (mod) return

  if (event.key === '=' || event.key === '+') {
    event.preventDefault()
    zoomTo(viewport ? viewport.percent + 25 : 125)
    return
  }
  if (event.key === '-') {
    event.preventDefault()
    zoomTo(viewport ? viewport.percent - 25 : 75)
    return
  }
  if (event.key === '0') {
    event.preventDefault()
    fitView()
    return
  }
  if (!props.readOnly) store.applyKey(event.key)
}

function onKeyUp(event: KeyboardEvent): void {
  if (event.code === 'Space') {
    spaceHeld = false
    if (!panning) cursorStyle.value = toolCursor.value
  }
}

function deleteSelected(): void {
  const ids = [...store.selection]
  if (ids.length === 0) return
  const before = deepCloneAll(store.shapes)
  store.removeShapes(ids)
  const after = deepCloneAll(store.shapes)
  commitDelete(ids)
  // 注意顺序：先发 op 再入栈，这样撤销时的 syncShapes 用的是已回滚后的状态
  recordSceneChange(before, after, '删除')
}

/* ------------------------------ 便签编辑浮层 ------------------------------ */

const noteTextareaEl = ref<HTMLTextAreaElement | null>(null)
const noteValue = ref('')
const viewportVersion = ref(0)
let keyboardInset = ref(0)

const editingShape = computed(() => {
  const id = store.editingNoteId
  if (!id) return null
  const shape = store.shapes.find((s) => s.id === id)
  return shape && shape.type === 'text' ? shape : null
})

const noteBox = computed(() => {
  void viewportVersion.value
  const shape = editingShape.value
  if (!shape || !viewport) return null
  const screen = viewport.worldRectToScreen(shape.geom)
  return {
    left: `${screen.x}px`,
    top: `${screen.y}px`,
    width: `${Math.max(screen.w, 180)}px`,
    height: `${Math.max(screen.h, 110)}px`,
    padding: `${clamp(14 * viewport.scale, 8, 22)}px`,
    fontSize: `${clamp(16 * viewport.scale, 11, 26)}px`,
    bottom: `${keyboardInset.value}px`,
  }
})

function openEditor(id: string): void {
  const shape = store.shapes.find((s) => s.id === id)
  if (!shape || shape.type !== 'text') return
  store.editingNoteId = id
  noteValue.value = shape.text
  void nextTick(() => {
    noteTextareaEl.value?.focus()
    const len = noteValue.value.length
    noteTextareaEl.value?.setSelectionRange(len, len)
    // 移动端键盘弹起后，确保浮层在可视区内
    scrollNoteIntoView()
  })
}

function scrollNoteIntoView(): void {
  nextTick(() => {
    noteTextareaEl.value?.scrollIntoView({ block: 'center', behavior: 'smooth' })
  })
}

function commitNote(): void {
  const shape = editingShape.value
  if (!shape) {
    store.editingNoteId = null
    return
  }
  const text = noteValue.value
  const before = deepCloneAll(store.shapes)
  store.editingNoteId = null

  if (text.trim() === '') {
    store.removeShapesLocal([shape.id])
    commitDelete([shape.id])
    recordSceneChange(before, deepCloneAll(store.shapes), '便签')
    return
  }
  if (text !== shape.text) {
    const next = { ...shape, text, rev: shape.rev + 1, updatedAt: Date.now() }
    store.updateShape(next)
    commitUpdate(next)
    recordSceneChange(before, deepCloneAll(store.shapes), '便签文字')
  }
  renderer?.invalidateScene()
}

function cancelNote(): void {
  const shape = editingShape.value
  const before = deepCloneAll(store.shapes)
  store.editingNoteId = null
  if (shape && shape.text.trim() === '' && noteValue.value.trim() === '') {
    store.removeShapesLocal([shape.id])
    commitDelete([shape.id])
    recordSceneChange(before, deepCloneAll(store.shapes), '便签')
  }
  renderer?.invalidateScene()
}

function onNoteKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') {
    event.preventDefault()
    cancelNote()
  }
  if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) {
    event.preventDefault()
    commitNote()
  }
}

/* -------------------------------- 视口 API ------------------------------- */

function fitView(): void {
  if (!viewport) return
  viewport.fit(store.bounds ?? null, 64)
  renderer?.invalidateScene()
}

function zoomTo(percent: number): void {
  if (!viewport) return
  const target = clamp(percent / 100, MIN_SCALE, MAX_SCALE)
  viewport.zoomByCenter(target / viewport.scale)
  renderer?.invalidateScene()
}

/* -------------------------------- 渲染输入 ------------------------------- */

const CURSOR_TTL_MS = 5000

function activeCursors(): CursorView[] {
  const now = Date.now()
  const out: CursorView[] = []
  for (const [cid, cursor] of remoteCursors.value) {
    if (now - cursor.at > CURSOR_TTL_MS) {
      remoteCursors.value.delete(cid)
      continue
    }
    out.push(cursor)
  }
  return out
}

function renderInput(): RenderInput {
  return {
    shapes: store.shapes,
    selection: store.selection,
    draft: draft.value,
    marquee: marquee.value,
    cursors: activeCursors(),
    readOnly: props.readOnly,
    interacting: interacting.value,
  }
}

/* -------------------------------- 远端光标 ------------------------------- */

function showRemoteCursor(cid: string, name: string, color: string, x: number, y: number): void {
  if (cid === store.cid) return
  const existing = remoteCursors.value.get(cid)
  remoteCursors.value.set(cid, { cid, name, color, x, y, at: Date.now() })
  if (!existing) {
    // 新成员：立刻重绘一次
    renderer?.invalidateOverlay()
  }
  renderer?.invalidateOverlay()
}

function removeRemoteCursor(cid: string): void {
  if (remoteCursors.value.delete(cid)) renderer?.invalidateOverlay()
}

/** 定时清理 5s 未更新的光标（协议建议渐隐，这里直接移除避免残留）。 */
let cursorSweeper = 0

/* --------------------------------- 生命周期 -------------------------------- */

let unsubViewport: (() => void) | null = null

onMounted(() => {
  if (!hostEl.value) return
  viewport = new Viewport()
  renderer = new Renderer(hostEl.value, viewport, renderInput)
  viewport.reset()
  renderer.invalidateScene()

  let lastZoom = -1
  unsubViewport = viewport.subscribe(() => {
    renderer?.invalidateScene()
    viewportVersion.value += 1
    const percent = viewport ? viewport.percent : 100
    if (percent !== lastZoom) {
      lastZoom = percent
      emit('zoom', percent)
    }
  })
  emit('zoom', viewport.percent)

  history.onChange(() => emit('history', { canUndo: history.canUndo, canRedo: history.canRedo }))

  window.addEventListener('keydown', onKeyDown)
  window.addEventListener('keyup', onKeyUp)
  window.addEventListener('resize', onResize)
  document.addEventListener('visibilitychange', onVisibility)

  // 视觉视口：移动端键盘弹起时缩小可视区，便签浮层跟着上移
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', onVisualViewport)
    window.visualViewport.addEventListener('scroll', onVisualViewport)
  }

  cursorSweeper = window.setInterval(() => {
    if (remoteCursors.value.size === 0) return
    const before = remoteCursors.value.size
    activeCursors()
    if (remoteCursors.value.size !== before) renderer?.invalidateOverlay()
  }, 1000)
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeyDown)
  window.removeEventListener('keyup', onKeyUp)
  window.removeEventListener('resize', onResize)
  document.removeEventListener('visibilitychange', onVisibility)
  if (window.visualViewport) {
    window.visualViewport.removeEventListener('resize', onVisualViewport)
    window.visualViewport.removeEventListener('scroll', onVisualViewport)
  }
  if (wheelRaf) cancelAnimationFrame(wheelRaf)
  window.clearInterval(cursorSweeper)
  unsubViewport?.()
  tool.onDeactivate?.(host)
  renderer?.destroy()
  renderer = null
  viewport = null
})

function onResize(): void {
  renderer?.resize()
}

function onVisibility(): void {
  renderer?.setRunning(document.visibilityState === 'visible')
}

function onVisualViewport(): void {
  const vv = window.visualViewport
  if (!vv) return
  // 键盘高度 ≈ 布局视口高度 − 视觉视口高度（减去滚动偏移）
  const inset = Math.max(0, window.innerHeight - vv.height - vv.offsetTop)
  keyboardInset.value = inset > 80 ? inset : 0
}

watch(
  () => store.tool,
  (next) => {
    tool.onDeactivate?.(host)
    tool = createTool(next)
    toolCursor.value = tool.cursor
    cursorStyle.value = spaceHeld ? 'grab' : tool.cursor
    draft.value = null
    marquee.value = null
    renderer?.invalidateOverlay()
  },
)

watch(
  () => store.shapes,
  (next) => {
    renderer?.pruneCaches(next)
    renderer?.invalidateScene()
  },
)

// 就地修改（append 追点、拖动）不会换数组引用，靠版本号触发重绘。
watch(
  () => store.sceneVersion,
  () => {
    renderer?.pruneCaches(store.shapes)
    renderer?.invalidateScene()
  },
)

watch(
  () => store.selection,
  () => renderer?.invalidateOverlay(),
)

watch(
  () => props.readOnly,
  () => {
    cursorStyle.value = props.readOnly ? 'grab' : toolCursor.value
    renderer?.invalidateScene()
  },
)

watch(
  () => store.members,
  (members) => {
    // presence.leave 后立刻移除该用户的光标（协议 §4.6：leave 立即生效）
    const alive = new Set(members.map((m) => m.clientId))
    let changed = false
    for (const cid of [...remoteCursors.value.keys()]) {
      if (!alive.has(cid)) {
        remoteCursors.value.delete(cid)
        changed = true
      }
    }
    if (changed) renderer?.invalidateOverlay()
  },
)

defineExpose({
  fitView,
  zoomTo,
  undo,
  redo,
  getViewport: () => viewport,
  getRenderer: () => renderer,
  showRemoteCursor,
  removeRemoteCursor,
})
</script>

<template>
  <div
    ref="hostEl"
    class="board-canvas"
    :style="{ cursor: cursorStyle }"
    @pointerdown="onPointerDown"
    @pointermove="onPointerMove"
    @pointerup="onPointerUp"
    @pointercancel="onPointerCancel"
    @pointerleave="onPointerLeave"
    @wheel="onWheel"
    @contextmenu.prevent
    @dblclick="onDoubleClick"
  >
    <div v-if="!store.hasShapes && store.joined" class="board-canvas__hint" aria-hidden="true">
      <p class="board-canvas__hint-title">
        {{ props.readOnly ? '这个白板还是空的' : '双击任意处添加便签' }}
      </p>
      <p class="board-canvas__hint-sub">
        {{ props.readOnly ? '房主开始绘制后，内容会实时出现在这里' : '或选择画笔，写下第一个想法' }}
      </p>
    </div>

    <!-- 便签编辑：HTML textarea 浮层，天然支持中文输入法组合输入 -->
    <div v-if="editingShape && noteBox" class="note-editor" :style="noteBox">
      <textarea
        ref="noteTextareaEl"
        v-model="noteValue"
        class="note-editor__input"
        :style="{ padding: noteBox.padding, fontSize: noteBox.fontSize }"
        placeholder="说点什么…"
        spellcheck="false"
        @blur="commitNote"
        @keydown="onNoteKeydown"
      />
      <span class="note-editor__hint">Esc 取消 · ⌘↵ 完成</span>
    </div>
  </div>
</template>

<style scoped>
.board-canvas {
  position: absolute;
  inset: 0;
  touch-action: none;
  overscroll-behavior: none;
  user-select: none;
  -webkit-user-select: none;
  -webkit-touch-callout: none;
  background: var(--paper);
}

.board-canvas :deep(.codraw-canvas) {
  position: absolute;
  inset: 0;
  display: block;
  width: 100%;
  height: 100%;
}

.board-canvas__hint {
  position: absolute;
  inset: 0;
  display: grid;
  place-content: center;
  gap: 4px;
  padding: 24px;
  text-align: center;
  pointer-events: none;
  user-select: none;
}

.board-canvas__hint-title {
  margin: 0;
  font-size: var(--text-md);
  line-height: var(--leading);
  color: var(--ink-soft);
  opacity: 0.55;
}

.board-canvas__hint-sub {
  margin: 0;
  font-size: var(--text-sm);
  line-height: var(--leading);
  color: var(--ink-soft);
  opacity: 0.38;
}

.note-editor {
  position: absolute;
  z-index: 30;
  display: flex;
  flex-direction: column;
  box-sizing: border-box;
  border-radius: 10px;
  background: var(--note);
  box-shadow:
    0 0 0 2px var(--brand),
    var(--shadow);
  overflow: hidden;
}

.note-editor__input {
  flex: 1;
  width: 100%;
  min-height: 0;
  border: 0;
  background: transparent;
  color: var(--ink);
  font-family: var(--font-sans);
  line-height: 1.6;
  resize: none;
  outline: none;
}

.note-editor__input::placeholder {
  color: var(--ink-soft);
  opacity: 0.45;
}

.note-editor__hint {
  position: absolute;
  right: 10px;
  bottom: 6px;
  font-size: 10px;
  line-height: 1.4;
  color: var(--ink-soft);
  opacity: 0.55;
  pointer-events: none;
}
</style>
