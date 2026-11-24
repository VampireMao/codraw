<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, shallowRef } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import BoardCanvas from '@/components/board/BoardCanvas.vue'
import Toolbar from '@/components/board/Toolbar.vue'
import ZoomCluster from '@/components/board/ZoomCluster.vue'
import ConnectionPill from '@/components/board/ConnectionPill.vue'
import OnlineStack from '@/components/board/OnlineStack.vue'
import SharePanel from '@/components/board/SharePanel.vue'
import SelectionBar from '@/components/board/SelectionBar.vue'
import NamePrompt from '@/components/board/NamePrompt.vue'
import { CoButton, CoIcon, CoTooltip, pushToast } from '@/components/ui'
import { useBoardStore } from '@/stores/board'
import { formatRoomCode, isValidRoomCode, normalizeRoomCode } from '@/net/roomCode'
import { createRoomSession, cursorBus, type RoomSession } from '@/net/session'
import { fetchRoom, ApiError } from '@/net/rest'
import { readName, readRoomKey, writeName, writeRoomKey } from '@/stores/storage'
import { copyToClipboard } from '@/utils/clipboard'
import { errorText, type OpAddPayload } from '@/net/types'

const props = defineProps<{ code: string }>()

const route = useRoute()
const router = useRouter()
const store = useBoardStore()

const canvasRef = ref<InstanceType<typeof BoardCanvas> | null>(null)
const zoom = ref(100)
const historyState = ref({ canUndo: false, canRedo: false })
const copied = ref(false)
const shareOpen = ref(false)
const notFound = ref(false)
const fatalError = ref<string | null>(null)
const session = shallowRef<RoomSession | null>(null)
const booting = ref(true)

/** 昵称弹窗：首次进入（无已保存昵称）时自动打开；顶栏头像也可再次打开。 */
const nameOpen = ref(false)
const nameFirstTime = ref(false)
/** 当前生效的昵称，用于弹窗预填与顶栏显示 */
const currentName = ref(readName())

/**
 * 只读 token：从 `?rt=` 读取（协议 §1.3）。
 * 也支持 `?ro=1` / `?ro=<token>` 的老式链接：当作只读意图，token 直接透传。
 */
const readonlyToken = computed<string | null>(() => {
  const rt = route.query.rt
  if (typeof rt === 'string' && rt) return rt
  const ro = route.query.ro
  if (typeof ro === 'string' && ro && ro !== '1') return ro
  return null
})

/** 从 `?k=` 读取编辑密钥（分享链接形式 `/r/CODE?k=editorKey`）。 */
const editorKeyFromUrl = computed<string | null>(() => {
  const k = route.query.k
  return typeof k === 'string' && k ? k : null
})

const normalizedCode = computed(() => normalizeRoomCode(props.code))
const codeOk = computed(() => isValidRoomCode(normalizedCode.value))
const displayCode = computed(() => formatRoomCode(normalizedCode.value))
const isDebug = computed(() => route.query.debug === '1')

/**
 * 只读判定：有只读 token，或服务端下发的权威角色没有写权限。
 *
 * **必须只依赖 `store.role` 响应式值**：收到 `member.role` 后角色会立即变化，
 * 工具栏/横幅要靠这里自动跟着变（PERMISSIONS §6.2）。
 * 只读 token 是安全底线（PERMISSIONS §4.2 第 1 条），存在时永远只读。
 */
const readOnly = computed(() => {
  if (readonlyToken.value) return true
  return !store.canEdit
})

/** 连接中（还没拿到 joined）时不要显示「空房间」文案。 */
const showCanvas = computed(() => !notFound.value && codeOk.value && !fatalError.value)

const shareViewUrl = computed(() => {
  if (typeof window === 'undefined') return ''
  const url = new URL(`/r/${normalizedCode.value}`, window.location.origin)
  // 服务端签发的只读链接在分享面板里动态获取；这里给一个不含 token 的兜底地址
  return url.toString()
})

const shareEditUrl = computed(() => {
  if (typeof window === 'undefined') return ''
  const url = new URL(`/r/${normalizedCode.value}`, window.location.origin)
  const key = editorKeyFromUrl.value ?? readRoomKey(normalizedCode.value)
  if (key) url.searchParams.set('k', key)
  return url.toString()
})

const canShareEdit = computed(() => store.canEdit)

/* ------------------------------- 房间生命周期 ------------------------------ */

async function boot(): Promise<void> {
  booting.value = true
  notFound.value = false
  fatalError.value = null

  if (!codeOk.value) {
    notFound.value = true
    booting.value = false
    return
  }

  // 编辑密钥：URL 优先，其次本地缓存（同一浏览器刷新后仍是编辑者）
  const urlKey = editorKeyFromUrl.value
  if (urlKey) writeRoomKey(normalizedCode.value, urlKey)
  const editorKey = readonlyToken.value
    ? undefined
    : (urlKey ?? readRoomKey(normalizedCode.value) ?? undefined)

  store.setRoomMeta({ code: normalizedCode.value, title: '未命名白板' })
  store.clearBoard()
  store.setStatus('connecting', 0)
  store.joined = false

  // 先查房间元信息：房间不存在时不要傻等 WS 握手失败（更快的 404）
  try {
    const info = await fetchRoom(normalizedCode.value, editorKey)
    store.setRoomMeta({ code: info.code, title: info.title })
    if (info.isFull) {
      fatalError.value = '房间人数已满，请稍后再试'
      booting.value = false
      return
    }
  } catch (error) {
    if (error instanceof ApiError) {
      if (error.code === 'room_not_found') {
        notFound.value = true
        booting.value = false
        return
      }
      // 其他错误（网络抖动/后端未就绪）不阻断：让 WS 去试，它有自己的重连与提示
      if (import.meta.env.DEV) {
        pushToast(`房间信息获取失败：${errorText(error.code)}`, 'warn')
      }
    }
    // 网络错误：继续尝试 WS（离线后第一笔也能画）
  }

  booting.value = false

  const next = await createRoomSession({
    store,
    roomCode: normalizedCode.value,
    ...(editorKey ? { editorKey } : {}),
    ...(readonlyToken.value ? { readonlyToken: readonlyToken.value } : {}),
    displayName: currentName.value || `访客-${store.cid.slice(0, 4)}`,
    onRoomNotFound: () => {
      notFound.value = true
      session.value?.dispose()
      session.value = null
    },
  })
  session.value = next

  // 分享链接进来的人通常没有昵称：默认「访客-xxxx」别人认不出，
  // 所以引导一次。用户填过就不再打扰。
  if (!currentName.value) {
    nameFirstTime.value = true
    nameOpen.value = true
  }
}

/**
 * 应用新昵称。
 *
 * 协议没有 rename 消息（PROTOCOL §3 的客户端消息是封闭集合），服务端只在
 * 握手时读取 name，所以改名必须重建会话。画布内容不会丢：
 * store 里的 shapes 保留，新连接通过 resync.snapshot 覆盖成权威状态。
 */
async function applyName(name: string): Promise<void> {
  const trimmed = name.trim().slice(0, 20)
  if (!trimmed) return

  writeName(trimmed)
  currentName.value = trimmed
  nameOpen.value = false
  nameFirstTime.value = false

  // 只重建网络会话，不动画布：避免闪一下白屏
  session.value?.dispose()
  session.value = null
  store.joined = false
  store.setStatus('connecting', 0)
  await boot()
  pushToast(`已改名为「${trimmed}」`, 'ok')
}

function skipName(): void {
  nameOpen.value = false
  nameFirstTime.value = false
  pushToast('随时可以点右上角头像改名', 'info')
}

function openNamePrompt(): void {
  // 只读访客也能改名：名字只影响 presence（光标标签、在线列表），不影响写权限。
  nameFirstTime.value = false
  nameOpen.value = true
}

onMounted(() => {
  void boot()
  unsubscribeCursor = cursorBus.on((cursor) => {
    canvasRef.value?.showRemoteCursor(
      cursor.from.clientId,
      cursor.from.name,
      cursor.from.color,
      cursor.x,
      cursor.y,
    )
  })
})

let unsubscribeCursor: (() => void) | null = null

function teardown(): void {
  unsubscribeCursor?.()
  unsubscribeCursor = null
  session.value?.dispose()
  session.value = null
}

onBeforeUnmount(teardown)

/* -------------------------------- 顶栏交互 -------------------------------- */

/**
 * 复制房间号（不是链接）。
 *
 * 这里是 `ABC-DEF` 这种可读格式：用户往往要口头报给同事、或手动输入到
 * 「加入房间」输入框，带连字符更易读，而输入框本身会忽略连字符。
 * 完整链接请用顶栏右侧的「分享」按钮。
 */
async function copyRoomCode(): Promise<void> {
  const code = displayCode.value
  if (!code) {
    pushToast('房间号还没准备好，请稍后再试', 'warn')
    return
  }
  const result = await copyToClipboard(code)
  if (result === 'ok') {
    copied.value = true
    pushToast(`房间号 ${code} 已复制`, 'ok')
    window.setTimeout(() => {
      copied.value = false
    }, 1600)
  } else {
    pushToast('复制失败，请手动选中房间号', 'warn')
  }
}

function goHome(): void {
  void router.push({ name: 'home' })
}

function gotoCreate(): void {
  void router.push({ name: 'home' })
}

function openOps(): void {
  window.open('/ops', '_blank', 'noopener')
}

/* -------------------------------- 画布操作 -------------------------------- */

function onHistory(state: { canUndo: boolean; canRedo: boolean }): void {
  historyState.value = state
}

function onZoom(percent: number): void {
  zoom.value = percent
}

function doUndo(): void {
  canvasRef.value?.undo()
}

function doRedo(): void {
  canvasRef.value?.redo()
}

function doFit(): void {
  canvasRef.value?.fitView()
}

function doZoom(percent: number): void {
  canvasRef.value?.zoomTo(percent)
}

function onDeleteSelection(): void {
  if (readOnly.value) {
    pushToast('只读模式 · 房主可授予你编辑权限', 'warn')
    return
  }
  window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Delete', bubbles: true }))
  store.clearSelection()
}

/* ------------------------------- 只读拦截 ------------------------------- */

function onReadonlyAttempt(): void {
  pushToast('只读模式 · 房主可授予你编辑权限', 'warn', 3200)
}

/** 光标上报：由 BoardCanvas 在 pointermove 时调用，realtime 层做 20Hz 节流 */
function onCursorMove(x: number, y: number): void {
  session.value?.sendCursor(x, y)
}

/** 本地写 op → realtime（负责乐观渲染、pending、重放与 ack 对账） */
function onCommit(op: OpAddPayload, shapeIds: string[]): void {
  const realtime = session.value?.realtime
  if (!realtime) {
    // 会话还没建好：不静默丢弃，明确提示（正常路径下不会发生）
    pushToast('连接尚未就绪，请稍后重试', 'warn')
    return
  }
  if (readOnly.value) {
    pushToast('只读模式 · 房主可授予你编辑权限', 'warn')
    return
  }
  realtime.submitOp(op, shapeIds)
}

/* ------------------------------ 成员权限 ------------------------------ */

function onGrantRole(clientId: string, role: 'editor' | 'viewer'): void {
  const realtime = session.value?.realtime
  if (!realtime) {
    pushToast('连接尚未就绪，请稍后重试', 'warn')
    return
  }
  if (!store.isOwner) {
    pushToast('只有房主可以管理成员权限', 'warn')
    return
  }
  realtime.grantRole(clientId, role)
}

function onTransferOwner(clientId: string): void {
  const realtime = session.value?.realtime
  if (!realtime) {
    pushToast('连接尚未就绪，请稍后重试', 'warn')
    return
  }
  if (!store.isOwner) {
    pushToast('只有房主可以转移房主', 'warn')
    return
  }
  const target = store.members.find((member) => member.clientId === clientId)
  realtime.transferOwner(clientId)
  pushToast(`正在把房主转给「${target?.name || '该成员'}」…`, 'info')
}

defineExpose({ store })
</script>

<template>
  <div class="room" :class="{ 'room--readonly': readOnly }">
    <!-- ------------------------------ 顶栏 ------------------------------ -->
    <header class="room__header">
      <div class="room__left">
        <button class="room__brand" type="button" aria-label="返回首页" @click="goHome">
          <span class="room__logo" aria-hidden="true">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" aria-hidden="true">
              <path
                d="M4.5 17.5c2.4-1 3.4-3.2 4.6-5.6 1-2 2-3.6 3.4-3.6 1.5 0 2 1.4 2.8 3.2.9 2 2 4.2 4.2 5.2"
                stroke="currentColor"
                stroke-width="1.8"
                stroke-linecap="round"
              />
              <circle cx="5" cy="18" r="1.6" fill="currentColor" />
              <circle cx="19.4" cy="17.4" r="1.6" fill="currentColor" />
            </svg>
          </span>
          <span class="room__wordmark">同绘</span>
        </button>

        <span class="room__sep" aria-hidden="true" />

        <CoTooltip text="点击复制房间号" placement="bottom">
          <button class="room-code" type="button" @click="copyRoomCode">
            <span>{{ displayCode }}</span>
            <CoIcon :name="copied ? 'check' : 'copy'" :size="13" class="room-code__icon" />
          </button>
        </CoTooltip>

        <span v-if="readOnly && store.joined" class="status-pill status-pill--neutral room__ro">
          <CoIcon name="eye" :size="13" />
          <span>只读模式</span>
        </span>
      </div>

      <div class="room__right">
        <!-- 当前身份：头像 + 名字 + 角色徽标；点头像可改名（只读访客也能改） -->
        <CoTooltip v-if="store.joined" text="修改昵称" placement="bottom">
          <button
            class="room__me"
            type="button"
            :aria-label="`当前昵称 ${currentName || '未设置'}${store.isOwner ? '，房主' : readOnly ? '，只读' : ''}，点击修改`"
            @click="openNamePrompt"
          >
            <span class="room__me-avatar" :style="{ background: store.myColor }" aria-hidden="true">
              {{ (currentName || '访').slice(0, 1) }}
            </span>
            <span class="room__me-name">{{ currentName || '设置昵称' }}</span>
            <span v-if="store.isOwner" class="badge badge--owner">
              <CoIcon name="crown" :size="11" />
              房主
            </span>
            <span v-else-if="readOnly" class="badge badge--viewer">只读</span>
          </button>
        </CoTooltip>

        <span class="room__online-desktop">
          <OnlineStack
            :members="store.members"
            :online="store.online"
            :is-owner="store.isOwner"
            :self-id="store.cid"
            :owner-client-id="store.ownerClientId"
            @grant="onGrantRole"
            @transfer="onTransferOwner"
          />
        </span>

        <ConnectionPill :status="store.status" :pending="store.pendingCount" :role="store.role" />

        <span class="room__sep room__sep--right" aria-hidden="true" />

        <CoButton variant="primary" size="sm" :disabled="!store.joined" @click="shareOpen = true">
          <template #icon>
            <CoIcon name="share" :size="15" />
          </template>
          <span class="room__share-label">分享</span>
        </CoButton>

        <button v-if="isDebug" class="room__icon-btn" type="button" aria-label="运维面板" @click="openOps">
          <CoIcon name="more" :size="18" />
        </button>
      </div>
    </header>

    <!-- ------------------------------ 画布 ------------------------------ -->
    <main class="room__stage">
      <!-- 房间不存在 -->
      <div v-if="notFound" class="room__gate">
        <span class="room__gate-code mono">404</span>
        <h1 class="room__gate-title">房间不存在或已过期</h1>
        <p class="room__gate-text">链接可能输错了一位，或者这个房间已经过期被清理。</p>
        <CoButton variant="primary" size="lg" @click="gotoCreate">
          <template #icon>
            <CoIcon name="plus" :size="18" />
          </template>
          新建一个白板
        </CoButton>
      </div>

      <!-- 房间满 / 致命错误 -->
      <div v-else-if="fatalError" class="room__gate">
        <h1 class="room__gate-title">{{ fatalError }}</h1>
        <p class="room__gate-text">请稍后刷新页面再试。</p>
        <CoButton variant="secondary" size="lg" @click="() => router.go(0)">刷新页面</CoButton>
      </div>

      <template v-else>
        <BoardCanvas
          v-if="showCanvas"
          ref="canvasRef"
          :read-only="readOnly"
          :connected="store.isOnline"
          @zoom="onZoom"
          @history="onHistory"
          @readonly-attempt="onReadonlyAttempt"
          @commit="onCommit"
          @cursor="onCursorMove"
        />

        <!-- 绘图工具栏：只读时整条不渲染（PERMISSIONS §6.1「工具栏只保留平移/缩放/适配」） -->
        <Toolbar
          v-if="showCanvas && store.canEdit"
          :history="historyState"
          @undo="doUndo"
          @redo="doRedo"
          @fit="doFit"
        />

        <div v-if="showCanvas" class="room__zoom" :class="{ 'room__zoom--readonly': readOnly }">
          <ZoomCluster :zoom="zoom" @zoom="doZoom" @fit="doFit" />
        </div>

        <!-- 连接中骨架：只在 joined 之前显示一秒以上，避免闪烁 -->
        <div v-if="booting && !store.joined" class="room__booting" role="status">
          <CoIcon name="spinner" :size="18" class="spin" />
          <span>正在连接房间…</span>
        </div>

        <div v-if="readOnly && store.joined" class="room__ro-banner" role="status">
          <CoIcon name="eye" :size="14" />
          <span>只读模式 · 房主可授予你编辑权限</span>
        </div>
      </template>
    </main>

    <SelectionBar
      v-if="store.selectionCount > 0 && !readOnly"
      class="room__sel-bar"
      :count="store.selectionCount"
      @delete="onDeleteSelection"
      @cancel="store.clearSelection()"
    />

    <SharePanel
      :open="shareOpen"
      :code="normalizedCode"
      :edit-url="shareEditUrl"
      :view-url="shareViewUrl"
      :can-share-edit="canShareEdit"
      :editor-key="readRoomKey(normalizedCode) ?? editorKeyFromUrl ?? ''"
      :room-limited="!store.joined"
      @close="shareOpen = false"
    />

    <NamePrompt
      :open="nameOpen"
      :current="currentName"
      :first-time="nameFirstTime"
      @confirm="applyName"
      @skip="skipName"
      @close="nameOpen = false"
    />
  </div>
</template>

<style scoped>
.room {
  display: flex;
  flex-direction: column;
  height: 100dvh;
  min-height: 0;
  background: var(--paper);
  overflow: hidden;
}

/* ------------------------------ 顶栏 ------------------------------ */
.room__header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-4);
  flex: none;
  height: var(--header-h);
  padding: 0 var(--space-4) 0 var(--space-3);
  background: var(--panel);
  border-bottom: 1px solid var(--line);
  z-index: var(--z-chrome);
}

.room__left,
.room__right {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  min-width: 0;
}

.room__right {
  gap: var(--space-3);
}

.room__brand {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  height: 40px;
  padding: 0 6px;
  border: 0;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--ink);
  cursor: pointer;
  transition: background-color var(--dur-fast) var(--ease);
}

.room__brand:hover {
  background: var(--surface-sunk);
}

.room__logo {
  display: grid;
  place-items: center;
  width: 26px;
  height: 26px;
  border-radius: 8px;
  background: var(--ink);
  color: var(--paper);
}

.room__wordmark {
  font-size: var(--text-base);
  font-weight: 600;
  letter-spacing: 0.03em;
}

.room__sep {
  width: 1px;
  height: 20px;
  background: var(--line);
  flex: none;
}

.room__ro {
  background: var(--brand-weak);
  color: var(--brand-deep);
}

/* 当前身份（头像 + 昵称）：点击改名。触控目标 ≥44px 高度 */
.room__me {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  height: 32px;
  max-width: 160px;
  padding: 0 10px 0 3px;
  border: 1px solid transparent;
  border-radius: 999px;
  background: transparent;
  color: var(--ink);
  font: inherit;
  font-size: var(--text-sm);
  cursor: pointer;
  transition:
    background-color var(--dur-fast) var(--ease),
    border-color var(--dur-fast) var(--ease);
}

.room__me:hover {
  background: var(--surface-sunk);
  border-color: var(--line);
}

.room__me:focus-visible {
  outline: 2px solid var(--brand);
  outline-offset: 1px;
}

.room__me-avatar {
  display: grid;
  place-items: center;
  flex: none;
  width: 26px;
  height: 26px;
  border-radius: 50%;
  color: #fff;
  font-size: var(--text-xs);
  font-weight: 600;
  line-height: 1;
  user-select: none;
}

.room__me-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* 身份徽标：沿用 tokens，不引入新色相（房主 = 品牌弱底，只读 = 沉底） */
.badge {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  flex: none;
  height: 18px;
  padding: 0 6px;
  border-radius: var(--radius-full);
  font-size: var(--text-2xs);
  font-weight: 500;
  line-height: 1;
  white-space: nowrap;
}

.badge--owner {
  background: var(--brand-weak);
  color: var(--brand-deep);
}

.badge--viewer {
  background: var(--surface-sunk);
  color: var(--ink-soft);
}

.room__share-label {
  display: inline-block;
}

.room__icon-btn {
  display: grid;
  place-items: center;
  width: 32px;
  height: 32px;
  border: 0;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--ink-soft);
  cursor: pointer;
}

.room__icon-btn:hover {
  background: var(--surface-sunk);
  color: var(--ink);
}

/* ------------------------------ 画布区 ------------------------------ */
.room__stage {
  position: relative;
  flex: 1;
  min-height: 0;
  overflow: hidden;
}

.room__zoom {
  position: absolute;
  right: 16px;
  bottom: 16px;
  z-index: var(--z-chrome);
}

.room__ro-banner {
  position: absolute;
  top: 12px;
  left: 50%;
  transform: translateX(-50%);
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 30px;
  padding: 0 12px;
  border-radius: var(--radius-full);
  background: rgba(255, 255, 255, 0.92);
  border: 1px solid var(--line);
  box-shadow: var(--shadow-sm);
  color: var(--ink-soft);
  font-size: var(--text-xs);
  white-space: nowrap;
  pointer-events: none;
  user-select: none;
}

.room__booting {
  position: absolute;
  top: 14px;
  left: 50%;
  transform: translateX(-50%);
  display: inline-flex;
  align-items: center;
  gap: 8px;
  height: 32px;
  padding: 0 14px;
  border-radius: var(--radius-full);
  background: var(--panel);
  border: 1px solid var(--line);
  box-shadow: var(--shadow-sm);
  font-size: var(--text-xs);
  color: var(--ink-soft);
  pointer-events: none;
}

.spin {
  animation: co-spin 0.9s linear infinite;
}

@keyframes co-spin {
  to {
    transform: rotate(360deg);
  }
}

/* ------------------------------ 错误门 ------------------------------ */
.room__gate {
  position: absolute;
  inset: 0;
  display: grid;
  place-content: center;
  justify-items: center;
  gap: 12px;
  padding: 32px 24px;
  text-align: center;
}

.room__gate-code {
  font-size: var(--text-xs);
  letter-spacing: 0.3em;
  color: var(--ink-soft);
  opacity: 0.5;
}

.room__gate-title {
  margin: 0;
  font-size: clamp(20px, 3vw, 26px);
  font-weight: 600;
  letter-spacing: -0.02em;
}

.room__gate-text {
  margin: 0 0 8px;
  font-size: var(--text-base);
  line-height: 1.75;
  color: var(--ink-soft);
  max-width: 34em;
}

/* ------------------------------ 移动端选中条 ------------------------------ */
.room__sel-bar {
  position: fixed;
  left: 50%;
  bottom: calc(var(--mobile-bar-h) + var(--safe-bottom) + 14px);
  transform: translateX(-50%);
  z-index: var(--z-chrome);
}

@media (min-width: 768px) {
  .room__sel-bar {
    display: none;
  }
}

/* ------------------------------ 响应式 ------------------------------ */
@media (max-width: 767px) {
  .room__header {
    height: var(--header-h-mobile);
    padding: 0 var(--space-2) 0 var(--space-1);
    padding-top: var(--safe-top);
    box-sizing: content-box;
    gap: var(--space-2);
  }

  .room__wordmark,
  .room__sep {
    display: none;
  }

  /* 移动端顶栏只有 390px：品牌标识让位给房间号与身份 */
  .room__brand {
    display: none;
  }

  /* 「只读模式」胶囊与身份徽标/只读横幅重复，移动端只保留后两者 */
  .room__ro {
    display: none;
  }

  /* 连接胶囊只留图标：完整文案在重连/离线时会变得很长 */
  :deep(.status-pill__label) {
    display: none;
  }

  /* 移动端也保留成员入口：所有者要能从底部抽屉管理权限 */
  .room__online-desktop {
    display: inline-flex;
  }

  .room__sep--right {
    display: none;
  }

  /* 移动端顶栏窄：只留头像，点击仍可改名 */
  .room__me {
    padding: 0 3px;
    border-color: transparent;
    gap: 4px;
  }

  .room__me-name {
    display: none;
  }

  /* 徽标在移动端只留图标形状，文字省略以节省宽度 */
  .badge {
    height: 17px;
    padding: 0 5px;
    font-size: 10px;
  }

  .room__right {
    gap: var(--space-2);
  }

  .room__share-label {
    display: none;
  }

  .room__ro {
    height: 24px;
    padding: 0 8px;
    font-size: var(--text-2xs);
  }

  .room__zoom {
    display: none;
  }

  /* 只读时移动端没有底部工具条，把缩放簇放回右下角（否则无法缩放） */
  .room__zoom--readonly {
    display: block;
    right: 12px;
    bottom: calc(var(--safe-bottom) + 12px);
  }

  .room__ro-banner {
    top: 10px;
    font-size: var(--text-2xs);
    height: 26px;
    padding: 0 10px;
    max-width: calc(100vw - 32px);
  }

  .room__booting {
    top: 10px;
    height: 28px;
    font-size: var(--text-2xs);
  }
}
</style>
