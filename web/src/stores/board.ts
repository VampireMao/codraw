/**
 * 房间 store：图形、选择、工具、连接状态、成员。
 *
 * 与网络层的边界：
 *  - store 不认识 WS，只认识 `RealtimeClient`；
 *  - 所有写操作走 `commit(op payload)`，由 realtime 层决定「立刻发」还是「进 pending」；
 *  - 远端事件由 `createScenePort()` 应用（见 stores/scenePort.ts）。
 *
 * 形状渲染用 `shallowRef` + `triggerRef`：上千图形做深响应式会给每个点数组装
 * Proxy，落笔时每次 push 都触发依赖收集，实测帧时间翻倍。
 */
import { defineStore } from 'pinia'
import { computed, ref, shallowRef, triggerRef } from 'vue'
import { sceneBounds, DEFAULT_STROKE, toWire, type Shape, type ShapeStyle } from '@/canvas/shapes'
import type { ToolId } from '@/canvas/tools/types'
import { TOOL_KEYS } from '@/canvas/history'
import type { ConnectionStatus, JoinedUser, PresenceUser, Role, RoomLimits } from '@/net/types'
import { DEFAULT_LIMITS, canRoleEdit } from '@/net/types'
import { ensureClientId, writeClientId } from './storage'

export const useBoardStore = defineStore('board', () => {
  /** clientId：同一浏览器标签页生命周期内不变（协议 §2.1） */
  const cid = ref(ensureClientId())
  const code = ref('')
  const title = ref('未命名白板')
  const role = ref<Role>('viewer')
  /** 当前房主 clientId；null = 无主房间（PERMISSIONS §6.3） */
  const ownerClientId = ref<string | null>(null)
  const status = ref<ConnectionStatus>('connecting')
  const pendingCount = ref(0)
  const online = ref(1)
  const members = ref<PresenceUser[]>([])
  const limits = ref<RoomLimits>({ ...DEFAULT_LIMITS })
  /** 已应用的房间序号（调试与 UI 展示用） */
  const appliedSeq = ref(0)
  /** 当前用户在房间里的身份（joined.you），颜色由服务端分配 */
  const me = ref<PresenceUser | null>(null)

  const shapes = shallowRef<Shape[]>([])
  /**
   * 场景版本号：就地修改 shape（append 追点、拖动）时递增。
   *
   * 为什么需要它：`shapes` 是 shallowRef，`triggerRef` 只能通知"值可能变了"，
   * 但 watch 的 getter 返回的仍是同一个数组引用，Vue 会判定无变化而跳过回调，
   * 画布就不重绘。加一个显式版本号，让监听方有可靠的触发源。
   *
   * 不用 deep watch：1000 条笔迹时深度遍历每帧都要跑一遍，代价不可接受。
   */
  const sceneVersion = ref(0)
  const selection = ref<Set<string>>(new Set())
  const tool = ref<ToolId>('select')
  /**
   * 画笔颜色。进入房间时默认取自己的头像色（joined.you.color），
   * 之后用户可在工具栏里改。null 表示"还没拿到身份"，此时用默认墨色。
   */
  const inkColor = ref<string | null>(null)
  const style = ref<ShapeStyle>({
    stroke: DEFAULT_STROKE,
    fill: 'none',
    strokeWidth: 2,
    opacity: 1,
    dash: 'solid',
  })
  const emptyHintDismissed = ref(false)
  const shareOpen = ref(false)
  const editingNoteId = ref<string | null>(null)
  /** 引导态：joined 之前显示骨架/加载，不显示空态文案 */
  const joined = ref(false)

  const hasShapes = computed(() => shapes.value.length > 0)
  const selectionCount = computed(() => selection.value.size)
  const isOwner = computed(() => role.value === 'owner')
  /**
   * 可编辑 = 服务端给的权威角色有写权限（PERMISSIONS §1）。
   * `owner` 隐含 `editor`；viewer 无论如何都不可写。
   */
  const canEdit = computed(() => canRoleEdit(role.value))
  /** 只读视图（组件侧的派生值）：角色一变就跟着变，不做任何缓存。 */
  const readOnly = computed(() => !canEdit.value)
  const selectedShapes = computed(() => shapes.value.filter((s) => selection.value.has(s.id)))
  const bounds = computed(() => sceneBounds(shapes.value))
  const myColor = computed(() => me.value?.color ?? '#3D5AFE')
  const isOnline = computed(() => status.value === 'online')

  const toolLabel = computed(
    () =>
      ({
        select: '选择',
        pen: '画笔',
        rect: '矩形',
        arrow: '箭头',
        note: '便签',
        eraser: '橡皮',
      })[tool.value],
  )

  const statusLabel = computed(() => {
    switch (status.value) {
      case 'online':
        return '已连接'
      case 'connecting':
        return '连接中…'
      case 'reconnecting':
        return pendingCount.value > 0 ? `离线 · 待同步 ${pendingCount.value} 条` : '重连中…'
      case 'offline':
        return pendingCount.value > 0 ? `离线 · 待同步 ${pendingCount.value} 条` : '离线'
      default:
        return '未连接'
    }
  })

  /* -------------------------------- 形状操作 ------------------------------- */

  function setShapes(next: Shape[]): void {
    shapes.value = next
    triggerRef(shapes)
  }

  function addShape(shape: Shape): void {
    shapes.value = [...shapes.value, shape]
    triggerRef(shapes)
    emptyHintDismissed.value = true
  }

  function addShapes(next: readonly Shape[]): void {
    if (next.length === 0) return
    shapes.value = [...shapes.value, ...next]
    triggerRef(shapes)
    emptyHintDismissed.value = true
  }

  function removeShapes(ids: readonly string[]): void {
    const set = new Set(ids)
    shapes.value = shapes.value.filter((s) => !set.has(s.id))
    const nextSel = new Set(selection.value)
    for (const id of set) nextSel.delete(id)
    selection.value = nextSel
    triggerRef(shapes)
  }

  /**
   * 仅本地移除（不发 op）。给"调用方自己负责发 delete"的场景用，
   * 避免同一次删除发出两条 op。
   */
  function removeShapesLocal(ids: readonly string[]): void {
    removeShapes(ids)
  }

  /** 按 id 就地替换（远端 update / LWW 覆盖）。 */
  function upsertShape(shape: Shape): void {
    const index = shapes.value.findIndex((s) => s.id === shape.id)
    if (index === -1) {
      addShape(shape)
      return
    }
    shapes.value = shapes.value.map((s, i) => (i === index ? shape : s))
    triggerRef(shapes)
  }

  /** 软删除：置 deleted 并保留（便于 Snapshot 语义与撤销）。 */
  function markDeleted(id: string): void {
    const shape = shapes.value.find((s) => s.id === id)
    if (!shape) return
    shape.deleted = true
    shape.rev += 1
    triggerRef(shapes)
    // 与 touch() 同理：就地修改不换数组引用，必须显式推进版本号才能触发重绘。
    sceneVersion.value += 1
  }

  function updateShape(shape: Shape): void {
    const index = shapes.value.findIndex((s) => s.id === shape.id)
    if (index === -1) return
    shapes.value = shapes.value.map((s, i) => (i === index ? shape : s))
    triggerRef(shapes)
  }

  /** 就地改对象后调用（拖动 60fps 路径，避免全量复制）。 */
  function touch(): void {
    triggerRef(shapes)
    // triggerRef 不足以让浅监听回调重绘（数组引用没变），显式推进版本号。
    sceneVersion.value += 1
  }

  function findShape(id: string): Shape | undefined {
    return shapes.value.find((s) => s.id === id)
  }

  /** pending 视觉：60% 不透明度（UX-SPEC §7.10）。 */
  function setPending(id: string, pending: boolean): void {
    const shape = shapes.value.find((s) => s.id === id)
    if (!shape || shape.pending === pending) return
    shape.pending = pending
    triggerRef(shapes)
  }

  /* -------------------------------- 选择 --------------------------------- */

  function setSelection(ids: readonly string[]): void {
    selection.value = new Set(ids)
  }

  function clearSelection(): void {
    if (selection.value.size === 0) return
    selection.value = new Set()
  }

  function selectAll(): void {
    selection.value = new Set(shapes.value.filter((s) => !s.deleted).map((s) => s.id))
  }

  function setTool(next: ToolId): void {
    if (next !== 'select' && !canEdit.value) return
    tool.value = next
    if (next !== 'select') clearSelection()
  }

  /**
   * 归一化颜色为服务端要求的 `#rrggbb` 小写格式（PROTOCOL §5.3）。
   * 服务端对格式错误的 stroke 直接拒 op，所以这一步不能省。
   */
  function normalizeInkColor(raw: string): string {
    const v = raw.trim().toLowerCase()
    if (/^#[0-9a-f]{6}$/.test(v)) return v
    // #rgb → #rrggbb
    if (/^#[0-9a-f]{3}$/.test(v)) {
      return `#${v[1]}${v[1]}${v[2]}${v[2]}${v[3]}${v[3]}`
    }
    return DEFAULT_STROKE.toLowerCase()
  }

  /** 设置画笔颜色；同时写进 style，让后续创建的图形立刻生效。 */
  function setInkColor(next: string): void {
    const color = normalizeInkColor(next)
    inkColor.value = color
    style.value = { ...style.value, stroke: color }
  }

  /** 当前实际生效的画笔颜色（未拿到身份时退回默认墨色）。 */
  function effectiveInkColor(): string {
    return inkColor.value ?? normalizeInkColor(style.value.stroke)
  }

  function applyKey(key: string): boolean {
    const next = TOOL_KEYS[key.toLowerCase()]
    if (!next) return false
    setTool(next)
    return true
  }

  /* ------------------------------ 房间与连接 ------------------------------ */

  /**
   * 采用服务端下发的权威 clientId。
   *
   * 客户端本地生成的 cid 只是“候选值”：服务端可能拒绝它并分配一个新的
   * （例如没有有效会话 token 时），此后所有 presence、授权、op 归属都以
   * 服务端返回的为准。
   */
  function setCid(next: string): void {
    const value = next.trim()
    if (!value || value === cid.value) return
    cid.value = value
    writeClientId(value)
    // 自己的颜色/角色由 joined/setMembers 重新对齐，避免留着旧身份的缓存。
    if (me.value && me.value.clientId !== value) me.value = null
  }

  function setRoomMeta(next: { code: string; title: string }): void {
    code.value = next.code.toUpperCase()
    title.value = next.title || '未命名白板'
  }

  function applyJoined(payload: {
    you: JoinedUser
    room: { code: string; title: string; epoch: number; seq: number }
    online: number
    limits: RoomLimits
    ownerClientId?: string | null
  }): void {
    joined.value = true
    me.value = payload.you
    role.value = payload.you.role
    // ownerClientId 缺失时按 you.role / isOwner 兜底（老服务端没有该字段也不至于显示错）
    ownerClientId.value =
      payload.ownerClientId ??
      (payload.you.isOwner || payload.you.role === 'owner' ? payload.you.clientId : null)
    code.value = payload.room.code.toUpperCase()
    title.value = payload.room.title || '未命名白板'
    online.value = payload.online
    limits.value = { ...DEFAULT_LIMITS, ...payload.limits }
    appliedSeq.value = Math.max(appliedSeq.value, payload.room.seq)
    // 首次进入房间：画笔颜色默认跟随自己的头像色。
    // 只在用户还没手动选过色时设置，避免重连时覆盖用户的选择。
    if (inkColor.value === null && payload.you.color) {
      inkColor.value = normalizeInkColor(payload.you.color)
    }
    if (!canRoleEdit(payload.you.role)) {
      tool.value = 'select'
      clearSelection()
    }
  }

  /**
   * 更新自己的权威角色（`member.role` 命中自己时调用）。
   * 降级立即回到选择工具并清选区：不能让人继续拖一个已经没有权限改的图形。
   */
  function setRole(next: Role): void {
    if (role.value === next) return
    role.value = next
    if (!canRoleEdit(next)) {
      tool.value = 'select'
      clearSelection()
    }
  }

  /** 更新当前房主（null = 无主房间）。 */
  function setOwnerClientId(next: string | null): void {
    ownerClientId.value = next
  }

  /**
   * 把成员列表里某人的角色改掉（`member.role` 的非自己分支）。
   * 列表是普通数组 + 每次替换引用，避免深响应式的额外开销。
   */
  function setMemberRole(clientId: string, next: Role): void {
    let changed = false
    const updated = members.value.map((user) => {
      if (user.clientId !== clientId || user.role === next) return user
      changed = true
      return { ...user, role: next }
    })
    if (!changed) return
    members.value = updated
    if (me.value?.clientId === clientId) me.value = { ...me.value, role: next }
  }

  function setStatus(next: ConnectionStatus, pending = pendingCount.value): void {
    status.value = next
    pendingCount.value = pending
  }

  function setMembers(next: readonly PresenceUser[]): void {
    members.value = [...next]
    online.value = Math.max(1, next.length)
    // 从全量列表里刷新自己的颜色（服务端隔一段时间可能重排调色板）
    const mine = next.find((user) => user.clientId === cid.value)
    if (mine) me.value = mine
  }

  function setAppliedSeq(seq: number): void {
    appliedSeq.value = seq
  }

  function clearBoard(): void {
    setShapes([])
    clearSelection()
    editingNoteId.value = null
    emptyHintDismissed.value = false
  }

  /** 序列化当前画布（导出/调试用）。 */
  function serialize(): ReturnType<typeof toWire>[] {
    return shapes.value.map(toWire)
  }

  return {
    cid,
    setCid,
    code,
    title,
    role,
    ownerClientId,
    isOwner,
    status,
    statusLabel,
    pendingCount,
    online,
    members,
    limits,
    appliedSeq,
    me,
    myColor,
    isOnline,
    joined,
    shapes,
    sceneVersion,
    selection,
    tool,
    style,
    inkColor,
    setInkColor,
    effectiveInkColor,
    readOnly,
    emptyHintDismissed,
    shareOpen,
    editingNoteId,
    hasShapes,
    selectionCount,
    canEdit,
    selectedShapes,
    bounds,
    toolLabel,
    setShapes,
    addShape,
    addShapes,
    removeShapes,
    removeShapesLocal,
    upsertShape,
    markDeleted,
    updateShape,
    touch,
    findShape,
    setPending,
    setSelection,
    clearSelection,
    selectAll,
    setTool,
    applyKey,
    setRoomMeta,
    applyJoined,
    setRole,
    setOwnerClientId,
    setMemberRole,
    setStatus,
    setMembers,
    setAppliedSeq,
    clearBoard,
    serialize,
  }
})
