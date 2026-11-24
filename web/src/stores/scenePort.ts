/**
 * ScenePort 实现：把协议 op 翻译成 store 变更。
 *
 * 单独一层的理由：`realtime.ts` 不应该知道 Pinia 的存在（可单测），
 * 而 op → shape 的翻译规则（append 合并、update LWW、delete 软删）
 * 是纯业务逻辑，集中在这里比散落在各 handler 里更容易验证。
 *
 * 契约：docs/PROTOCOL.md §3.2（action 语义）、§5（shape）。
 */
import type { ScenePort } from '@/net/realtime'
import type { MemberRolePayload, OpAddPayload, OpBroadcastPayload, PresenceUser, Role } from '@/net/types'
import { fromWire, toWire, type Shape } from '@/canvas/shapes'

export interface SceneStore {
  shapes: readonly Shape[]
  cid: string
  addShape(shape: Shape): void
  addShapes(shapes: readonly Shape[]): void
  upsertShape(shape: Shape): void
  updateShape(shape: Shape): void
  removeShapes(ids: readonly string[]): void
  markDeleted(id: string): void
  setPending(id: string, pending: boolean): void
  setShapes(shapes: Shape[]): void
  findShape(id: string): Shape | undefined
  touch(): void
}

/**
 * `member.role` 需要的最小状态面。
 *
 * 单独抽出来（而不是直接依赖 Pinia store）是为了让角色状态迁移可以脱离
 * DOM/Pinia 单测——这是「授权后工具栏是否真的出现」最容易回归的地方。
 */
export interface MemberRoleStore {
  readonly cid: string
  readonly role: Role
  readonly ownerClientId: string | null
  readonly members: readonly PresenceUser[]
  setRole(role: Role): void
  setOwnerClientId(ownerClientId: string | null): void
  setMemberRole(clientId: string, role: Role): void
  /** 用户可见提示（可选注入：单测不关心 UI，正式实现接 toast） */
  notify?(message: string, tone: 'info' | 'ok' | 'warn'): void
}

function isRole(value: unknown): value is Role {
  return value === 'owner' || value === 'editor' || value === 'viewer'
}

/** 协议 shape（wire） → 本地模型，失败时返回 null（脏数据不崩）。 */
export function wireToShape(raw: unknown): Shape | null {
  return fromWire(raw)
}

/** 本地模型 → 协议 shape。store 侧不直接调，统一走这里便于加日志/裁剪。 */
export function shapeToWire(shape: Shape): unknown {
  return toWire(shape)
}

/**
 * 应用一条 op 到 store。**纯函数式地描述「应该做什么」**，实际 mutate 由 store 完成。
 *
 * 一致性规则（PROTOCOL §5.5）：
 *  - create：id 已存在 → 视为已应用（服务端也会返回 duplicate_shape，不重复加）
 *  - update：整对象覆盖（LWW），不做字段合并
 *  - delete：软删（保留对象，置 deleted）
 *  - append：并入 points 并重算包围盒
 */
export function applyOpToStore(store: SceneStore, op: OpAddPayload | OpBroadcastPayload): void {
  switch (op.action) {
    case 'create':
    case 'update': {
      if (!op.shape) return
      const shape = wireToShape(op.shape)
      if (!shape) return
      // 自己的 op 已乐观上屏：按 id 覆盖即可（served rev 会被服务端覆盖，这里以本地为准直到 ack）
      const existing = store.findShape(shape.id)
      if (existing && op.action === 'create') {
        // 重复 create（重放/幂等命中）：不新增，最多同步样式
        store.upsertShape({ ...shape, pending: existing.pending })
        return
      }
      store.upsertShape(shape)
      return
    }
    case 'delete': {
      // 服务端广播 delete 时可能带完整 shape（含 deleted:true），也可能只带 id。
      // 两种形态都要认，否则"别人删的图形在我这边不会消失"——这正是协作白板
      // 最不能出的问题。
      const id =
        op.id ??
        (op.shape && typeof op.shape === 'object' && typeof (op.shape as { id?: unknown }).id === 'string'
          ? (op.shape as { id: string }).id
          : undefined)
      if (!id) return
      store.markDeleted(id)
      return
    }
    case 'append': {
      if (!op.id || !op.points) return
      appendPoints(store, op.id, op.points)
      return
    }
    default:
      return
  }
}

/** append：把增量点并入本地 freehand/arrow 并重算几何。 */
function appendPoints(store: SceneStore, id: string, points: ReadonlyArray<readonly number[]>): void {
  const shape = store.findShape(id)
  if (!shape) return
  if (shape.type !== 'freehand') return

  for (const point of points) {
    const x = point[0]
    const y = point[1]
    if (typeof x !== 'number' || typeof y !== 'number' || !Number.isFinite(x) || !Number.isFinite(y)) continue
    const pressure = point[2]
    if (typeof pressure === 'number') {
      shape.points.push([x, y, pressure])
    } else {
      shape.points.push([x, y])
    }
  }
  recomputeBounds(shape)
  shape.rev += 1
  shape.updatedAt = Date.now()
  store.touch()
}

function recomputeBounds(shape: Shape & { type: 'freehand' }): void {
  let minX = Number.POSITIVE_INFINITY
  let minY = Number.POSITIVE_INFINITY
  let maxX = Number.NEGATIVE_INFINITY
  let maxY = Number.NEGATIVE_INFINITY
  for (const p of shape.points) {
    if (p[0] < minX) minX = p[0]
    if (p[1] < minY) minY = p[1]
    if (p[0] > maxX) maxX = p[0]
    if (p[1] > maxY) maxY = p[1]
  }
  if (!Number.isFinite(minX)) return
  shape.geom = {
    x: Math.round(minX * 10) / 10,
    y: Math.round(minY * 10) / 10,
    w: Math.round((maxX - minX) * 10) / 10,
    h: Math.round((maxY - minY) * 10) / 10,
  }
}

/**
 * 构造 ScenePort。
 *
 * `getShapesById` 用于回滚：op 携带 shapeIds（创建时即已知），
 * 回滚直接按 id 移除，比存整个 shape 副本省内存。
 */
export function createScenePort(store: SceneStore & MemberRoleStore): ScenePort {
  return {
    applyRemoteOp(op) {
      applyOpToStore(store, op)
    },
    applyLocalOp(op) {
      // 乐观渲染：本地立即上屏，标记 pending（60% 不透明度）
      applyOpToStore(store, op)
      const ids = shapeIdsOf(op)
      for (const id of ids) store.setPending(id, true)
    },

    replaceAll(shapes, _seq, _epoch) {
      // 快照整替换：保留本地未确认（pending）的图形，其余全部替换。
      // 否则用户在快照到达前画的几笔会被清掉（协议允许保留未发送草稿）。
      //
      // 注意：入参是**协议形状**（`kind` 字段），必须经 fromWire 转成本地模型
      // （`type` 字段），否则渲染层读不到类型。
      const incoming = shapes
        .map((raw) => fromWire(raw))
        .filter((shape): shape is Shape => !!shape && !shape.deleted)
      const pendingLocal = store.shapes.filter((shape) => shape.pending && !shape.deleted)
      const incomingIds = new Set(incoming.map((shape) => shape.id))
      const survivors = pendingLocal.filter((shape) => !incomingIds.has(shape.id))
      store.setShapes([...incoming, ...survivors])
    },

    rollbackOp(_opId, shapeIds) {
      // 只删「这条 op 创建、且仍处于 pending」的图形，
      // 避免把服务端已确认的同 id 图形误删
      const removable = shapeIds.filter((id) => {
        const shape = store.findShape(id)
        return !!shape && shape.pending
      })
      if (removable.length > 0) store.removeShapes(removable)
    },

    confirmOp(_opId, shapeIds) {
      for (const id of shapeIds) store.setPending(id, false)
    },

    listShapeIds() {
      return store.shapes.filter((shape) => !shape.deleted).map((shape) => shape.id)
    },

    applyMemberRole(payload) {
      applyMemberRoleToStore(store, payload)
    },
  }
}

/**
 * `member.role` 的状态迁移（PERMISSIONS §3.1 / §6.2）。
 *
 * 规则：
 *  - 成员列表里对应的 role 就地更新（列表本身用新数组替换，保证响应式）；
 *  - 命中自己 → 更新 `role`，升级/降级都要让工具栏立刻跟着变（不做缓存）；
 *  - `role === 'owner'` → 记录新 `ownerClientId`，并把原房主降为 editor
 *    （服务端也会广播原房主的那条，这里做兜底，避免 UI 短暂出现两个房主）。
 *
 * toast 由调用方（session 层）根据环境决定，这里保持纯状态迁移，便于单测。
 */
export function applyMemberRoleToStore(store: MemberRoleStore, payload: MemberRolePayload): void {
  if (!payload || typeof payload.clientId !== 'string' || !payload.clientId) return
  if (!isRole(payload.role)) return

  const { clientId, role } = payload
  const isSelf = clientId === store.cid
  const previousRole = isSelf ? store.role : undefined
  const previousOwner = store.ownerClientId

  // 1. 成员列表
  store.setMemberRole(clientId, role)

  // 2. 自己
  if (isSelf) {
    store.setRole(role)
  }

  // 3. 房主归属
  if (role === 'owner') {
    store.setOwnerClientId(clientId)
    if (previousOwner && previousOwner !== clientId) {
      // 原房主降为 editor（PERMISSIONS §2.3）。这条通常也会由服务端单独广播，
      // 这里先落一步保证 UI 不会出现两个「房主」徽标。
      store.setMemberRole(previousOwner, 'editor')
      if (previousOwner === store.cid) store.setRole('editor')
    }
  } else if (store.ownerClientId === clientId) {
    // 房主被降级/撤销 → 房间进入无主状态，等后台广播新的 owner
    store.setOwnerClientId(null)
  }

  // 4. 自己的角色变化提示（PERMISSIONS §6.2）
  if (isSelf && previousRole !== undefined) {
    const toast = selfRoleToast(previousRole, role)
    if (toast) store.notify?.(toast.message, toast.tone)
  } else if (!isSelf && previousOwner === store.cid && role === 'owner') {
    // 自己把房主交出去了：不再有管理入口，给一句确认
    store.notify?.('房主权限已移交给其他成员', 'info')
  }
}

/** 自己角色变化 → toast 文案（PERMISSIONS §6.2）。无变化返回 null。 */
export function selfRoleToast(
  previous: Role,
  next: Role,
): { message: string; tone: 'info' | 'ok' | 'warn' } | null {
  if (previous === next) return null
  if (next === 'owner') return { message: '你已成为房主', tone: 'ok' }
  if (next === 'editor') {
    // 从 owner 降为 editor 是「移交」，不是被授权
    if (previous === 'owner') return { message: '房主权限已移交给其他成员', tone: 'info' }
    return { message: '你已获得编辑权限', tone: 'ok' }
  }
  return { message: '你已被设为只读', tone: 'warn' }
}

/** 从 op 里推断涉及的 shape id（用于 pending 标记与回滚）。 */
export function shapeIdsOf(op: OpAddPayload | OpBroadcastPayload): string[] {
  if (op.action === 'create' || op.action === 'update') {
    const id =
      op.shape && typeof op.shape === 'object' && typeof (op.shape as { id?: unknown }).id === 'string'
        ? (op.shape as { id: string }).id
        : null
    return id ? [id] : []
  }
  if (op.action === 'delete') {
    // 服务端的 delete 广播带完整 shape，客户端发出的只带 id，两种都要覆盖。
    const id =
      op.id ??
      (op.shape && typeof op.shape === 'object' && typeof (op.shape as { id?: unknown }).id === 'string'
        ? (op.shape as { id: string }).id
        : null)
    return id ? [id] : []
  }
  if (op.action === 'append' && op.id) return [op.id]
  return []
}
