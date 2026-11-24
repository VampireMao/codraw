<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { CoIcon, CoTooltip } from '@/components/ui'
import type { PresenceUser } from '@/net/types'

/**
 * 在线成员栈 + 成员管理面板（PERMISSIONS §6.1）。
 *
 * 两种形态：
 *  - 非 owner：头像栈 + 只读列表（每人带角色标记），点击没有任何管理入口；
 *  - owner：点头像栈打开成员面板，每行有 editor/viewer 切换 + 「转移房主」。
 *
 * **面板必须 Teleport 到 body + fixed 定位**：顶栏虽然没有 overflow 裁剪，
 * 但面板要覆盖在画布之上，且移动端要变成底部抽屉——用 fixed 是最省事且
 * 不会被任何祖先 `overflow` 裁掉的做法（颜色选择器的教训）。
 */
const props = defineProps<{
  members: readonly PresenceUser[]
  online: number
  /** 当前用户是否为房主（决定是否显示管理能力） */
  isOwner: boolean
  /** 自己的 clientId（列表里标记「你」） */
  selfId: string
  /** 当前房主 clientId；null = 无主房间 */
  ownerClientId: string | null
}>()

const emit = defineEmits<{
  /** 授权/撤销：role 只会是 editor 或 viewer */
  grant: [clientId: string, role: 'editor' | 'viewer']
  transfer: [clientId: string]
}>()

/** 最多显示 4 个头像，其余折叠成 +N；按 color 上色（服务端分配）。 */
const visible = computed(() => props.members.slice(0, 4))
const overflow = computed(() => Math.max(0, props.members.length - visible.value.length))
const label = computed(() => `${props.online} 人在线`)
const manageable = computed(() => props.isOwner && props.members.length > 0)

const open = ref(false)
const triggerEl = ref<HTMLElement | null>(null)
const panelEl = ref<HTMLElement | null>(null)
const panelStyle = ref<Record<string, string>>({})

function initial(name: string): string {
  const trimmed = name.trim()
  if (!trimmed) return '?'
  // 中文取首字，英文取首字母大写
  return [...trimmed][0]?.toUpperCase() ?? '?'
}

function isOwnerMember(member: PresenceUser): boolean {
  return member.role === 'owner' || member.clientId === props.ownerClientId
}

function roleLabel(member: PresenceUser): string {
  if (isOwnerMember(member)) return '房主'
  return member.role === 'editor' ? '可编辑' : '只读'
}

/** 面板宽度（与 CSS 保持一致），用于视口夹取。 */
const PANEL_WIDTH = 344
const PANEL_GAP = 8

function positionPanel(): void {
  const el = triggerEl.value
  if (!el) return
  // 移动端是底部抽屉：由 CSS 控制，不写 inline 坐标（否则 !important 满天飞）
  if (window.matchMedia('(max-width: 767px)').matches) {
    panelStyle.value = {}
    return
  }
  const rect = el.getBoundingClientRect()
  const width = Math.min(PANEL_WIDTH, window.innerWidth - 16)
  // 优先右对齐到触发按钮，超出视口时夹取
  const left = Math.min(Math.max(8, rect.right - width), window.innerWidth - width - 8)
  const top = rect.bottom + PANEL_GAP
  panelStyle.value = { left: `${left}px`, top: `${top}px`, width: `${width}px` }
}

function toggle(): void {
  if (!props.members.length) return
  open.value = !open.value
  if (open.value) {
    // 面板打开时鼠标仍停在触发器上，tooltip 会盖住面板标题——主动失焦
    triggerEl.value?.blur()
    // 先让 DOM 更新再量尺寸（面板 v-if 挂载后才有 rect）
    void nextTick(positionPanel)
  }
}
function close(): void {
  open.value = false
}

function grant(member: PresenceUser, role: 'editor' | 'viewer'): void {
  if (!props.isOwner) return
  if (isOwnerMember(member)) return
  if (member.role === role) return
  emit('grant', member.clientId, role)
}

function transfer(member: PresenceUser): void {
  if (!props.isOwner) return
  if (isOwnerMember(member)) return
  emit('transfer', member.clientId)
  close()
}

/** 点击面板外收起（capture 阶段监听，避免被画布吃掉）。 */
function onDocPointerDown(event: PointerEvent): void {
  if (!open.value) return
  const target = event.target as HTMLElement | null
  if (!target) return
  if (triggerEl.value?.contains(target)) return
  if (panelEl.value?.contains(target)) return
  close()
}

function onKey(event: KeyboardEvent): void {
  if (event.key === 'Escape' && open.value) {
    close()
    triggerEl.value?.focus()
  }
}

watch(open, (isOpen) => {
  if (isOpen) {
    document.addEventListener('pointerdown', onDocPointerDown, true)
    window.addEventListener('keydown', onKey)
    window.addEventListener('resize', positionPanel)
    window.addEventListener('scroll', positionPanel, true)
  } else {
    document.removeEventListener('pointerdown', onDocPointerDown, true)
    window.removeEventListener('keydown', onKey)
    window.removeEventListener('resize', positionPanel)
    window.removeEventListener('scroll', positionPanel, true)
  }
})

/** 不再拥有管理权（被移交/降级）时自动收起面板，避免残留操作入口。 */
watch(
  () => props.isOwner,
  (owner) => {
    if (!owner) close()
  },
)

onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', onDocPointerDown, true)
  window.removeEventListener('keydown', onKey)
  window.removeEventListener('resize', positionPanel)
  window.removeEventListener('scroll', positionPanel, true)
})

defineExpose({ close })
</script>

<template>
  <div class="online-stack">
    <CoTooltip
      :text="manageable ? '成员与权限' : members.map((m) => m.name || '访客').join('、') || '仅你在线'"
      placement="bottom"
      :disabled="open"
    >
      <button
        ref="triggerEl"
        type="button"
        class="online"
        :class="{ 'online--interactive': members.length > 0 }"
        :aria-label="manageable ? `${label}，管理成员权限` : label"
        :aria-expanded="members.length > 0 ? open : undefined"
        :aria-haspopup="manageable ? 'dialog' : undefined"
        @click="toggle"
      >
        <span class="online__count">{{ label }}</span>
        <ul v-if="visible.length > 0" class="online__avatars" aria-hidden="true">
          <li
            v-for="member in visible"
            :key="member.clientId"
            class="online__avatar"
            :class="{ 'online__avatar--owner': isOwnerMember(member) }"
            :style="{ background: member.color }"
          >
            {{ initial(member.name) }}
          </li>
          <li v-if="overflow > 0" class="online__avatar online__avatar--more">+{{ overflow }}</li>
        </ul>
        <CoIcon v-if="manageable" name="chevron-right" :size="12" class="online__chev" aria-hidden="true" />
      </button>
    </CoTooltip>

    <Teleport to="body">
      <div
        v-if="open"
        ref="panelEl"
        class="members"
        role="dialog"
        :aria-label="isOwner ? '成员权限管理' : '在线成员'"
        :style="panelStyle"
      >
        <header class="members__header">
          <h2 class="members__title">在线成员</h2>
          <button type="button" class="members__close" aria-label="关闭" @click="close">
            <CoIcon name="close" :size="16" />
          </button>
        </header>

        <ul class="members__list">
          <li v-for="member in members" :key="member.clientId" class="members__row">
            <span class="members__avatar" :style="{ background: member.color }" aria-hidden="true">
              {{ initial(member.name) }}
            </span>

            <span class="members__identity">
              <span class="members__name">
                {{ member.name || '访客' }}
                <span v-if="member.clientId === selfId" class="members__you">（你）</span>
              </span>
              <span v-if="isOwnerMember(member)" class="badge badge--owner">
                <CoIcon name="crown" :size="11" />
                房主
              </span>
              <span v-else-if="member.role === 'viewer'" class="badge badge--viewer">只读</span>
            </span>

            <span v-if="isOwner && !isOwnerMember(member)" class="members__actions">
              <span class="members__role-group" role="group" :aria-label="`${member.name || '访客'} 的权限`">
                <button
                  type="button"
                  class="members__role"
                  :class="{ 'members__role--active': member.role === 'editor' }"
                  :aria-pressed="member.role === 'editor'"
                  @click="grant(member, 'editor')"
                >
                  可编辑
                </button>
                <button
                  type="button"
                  class="members__role"
                  :class="{ 'members__role--active': member.role === 'viewer' }"
                  :aria-pressed="member.role === 'viewer'"
                  @click="grant(member, 'viewer')"
                >
                  只读
                </button>
              </span>
              <button type="button" class="members__transfer" @click="transfer(member)">转移房主</button>
            </span>

            <span v-else-if="!isOwnerMember(member)" class="members__role-label">{{
              roleLabel(member)
            }}</span>
          </li>
        </ul>

        <p v-if="isOwner" class="members__hint">点权限即可实时生效；「转移房主」后你会降为可编辑。</p>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.online-stack {
  display: inline-flex;
}

.online {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  height: 32px;
  padding: 0 6px;
  border: 1px solid transparent;
  border-radius: var(--radius-full);
  background: transparent;
  font: inherit;
  color: inherit;
  cursor: default;
  -webkit-tap-highlight-color: transparent;
  transition:
    background-color var(--dur-fast) var(--ease),
    border-color var(--dur-fast) var(--ease);
}

.online--interactive {
  cursor: pointer;
}

.online--interactive:hover {
  background: var(--surface-sunk);
  border-color: var(--line);
}

.online:focus-visible {
  outline: 2px solid var(--brand);
  outline-offset: 1px;
}

.online__count {
  font-size: var(--text-xs);
  line-height: 1;
  color: var(--ink-soft);
  white-space: nowrap;
}

.online__avatars {
  display: flex;
  align-items: center;
  padding-left: 2px;
  margin: 0;
  list-style: none;
}

.online__avatar {
  display: grid;
  place-items: center;
  width: 22px;
  height: 22px;
  margin-left: -6px;
  border-radius: 50%;
  border: 2px solid var(--panel);
  color: #fff;
  font-size: 10px;
  font-weight: 600;
  line-height: 1;
  letter-spacing: 0;
  text-transform: uppercase;
  box-sizing: content-box;
}

.online__avatar:first-child {
  margin-left: 0;
}

.online__avatar--owner {
  box-shadow: 0 0 0 1.5px var(--brand);
}

.online__avatar--more {
  background: var(--surface-sunk);
  color: var(--ink-soft);
  font-weight: 500;
}

.online__chev {
  color: var(--ink-soft);
  transform: rotate(90deg);
  flex: none;
}

/* ------------------------------ 成员面板 ------------------------------ */
/*
 * Teleport 到 body，坐标由 positionPanel() 用 fixed 计算。
 * scoped 属性会随节点一起移动，所以这里的样式仍然生效。
 */
.members {
  position: fixed;
  z-index: var(--z-popover, 40);
  display: flex;
  flex-direction: column;
  max-height: min(60vh, 420px);
  box-sizing: border-box;
  border-radius: var(--radius-md);
  background: var(--panel);
  border: 1px solid var(--line);
  box-shadow: var(--shadow-md, var(--shadow));
  overflow: hidden;
}

.members__header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
  padding: 10px 8px 10px 14px;
  border-bottom: 1px solid var(--line);
  flex: none;
}

.members__title {
  margin: 0;
  font-size: var(--text-sm);
  font-weight: 600;
}

.members__close {
  display: grid;
  place-items: center;
  width: 28px;
  height: 28px;
  border: 0;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--ink-soft);
  cursor: pointer;
}

.members__close:hover {
  background: var(--surface-sunk);
  color: var(--ink);
}

.members__list {
  margin: 0;
  padding: 6px;
  list-style: none;
  overflow-y: auto;
  overscroll-behavior: contain;
}

.members__row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  min-height: 44px;
  padding: 4px 8px;
  border-radius: var(--radius-sm);
}

.members__row:hover {
  background: var(--surface-sunk);
}

/* 权限操作整组换行，避免「转移房主」被挤到下一行后单独占位 */
.members__actions {
  display: flex;
  align-items: center;
  gap: 6px;
  flex: 0 0 auto;
  margin-left: auto;
}

.members__avatar {
  display: grid;
  place-items: center;
  flex: none;
  width: 28px;
  height: 28px;
  border-radius: 50%;
  color: #fff;
  font-size: var(--text-xs);
  font-weight: 600;
  line-height: 1;
}

.members__identity {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  flex: 1 1 auto;
}

.members__name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: var(--text-sm);
}

.members__you {
  color: var(--ink-soft);
  font-size: var(--text-2xs);
}

.badge {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  flex: none;
  height: 18px;
  padding: 0 6px;
  border-radius: var(--radius-full);
  font-size: var(--text-2xs);
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

.members__role-group {
  display: inline-flex;
  flex: none;
  padding: 2px;
  border-radius: var(--radius-full);
  background: var(--surface-sunk);
}

.members__role {
  height: 32px;
  min-width: 52px;
  padding: 0 10px;
  border: 0;
  border-radius: var(--radius-full);
  background: transparent;
  color: var(--ink-soft);
  font: inherit;
  font-size: var(--text-2xs);
  cursor: pointer;
  transition:
    background-color var(--dur-fast) var(--ease),
    color var(--dur-fast) var(--ease);
}

.members__role:hover {
  color: var(--ink);
}

.members__role--active {
  background: var(--panel);
  color: var(--ink);
  font-weight: 600;
  box-shadow: var(--shadow-sm);
}

.members__role:focus-visible {
  outline: 2px solid var(--brand);
  outline-offset: -2px;
}

.members__transfer {
  flex: none;
  height: 32px;
  padding: 0 10px;
  border: 1px solid var(--line);
  border-radius: var(--radius-full);
  background: transparent;
  color: var(--ink-soft);
  font: inherit;
  font-size: var(--text-2xs);
  cursor: pointer;
  transition:
    border-color var(--dur-fast) var(--ease),
    color var(--dur-fast) var(--ease);
}

.members__transfer:hover {
  border-color: var(--brand);
  color: var(--brand-deep);
}

.members__transfer:focus-visible {
  outline: 2px solid var(--brand);
  outline-offset: 1px;
}

.members__role-label {
  flex: none;
  font-size: var(--text-2xs);
  color: var(--ink-soft);
}

.members__hint {
  flex: none;
  margin: 0;
  padding: 8px 14px 12px;
  border-top: 1px solid var(--line);
  font-size: var(--text-2xs);
  line-height: 1.5;
  color: var(--ink-soft);
}

/* ------------------------------ 移动端：底部抽屉 ------------------------------ */
@media (max-width: 767px) {
  .online__avatars,
  .online__chev {
    display: none;
  }

  .online__count {
    font-size: var(--text-2xs);
  }

  .online {
    height: 44px;
    padding: 0 8px;
  }

  /* 移动端是底部抽屉：fixed 四边贴合，坐标由 CSS 接管 */
  .members {
    left: 0 !important;
    right: 0;
    top: auto !important;
    bottom: 0;
    width: auto !important;
    max-height: min(72vh, 560px);
    border-radius: var(--radius-lg) var(--radius-lg) 0 0;
    border-bottom: 0;
    padding-bottom: env(safe-area-inset-bottom, 0px);
    animation: members-sheet-in var(--dur) var(--ease-out);
  }

  .members__row {
    min-height: 52px;
  }

  .members__role {
    height: 44px;
    min-width: 60px;
  }

  .members__transfer {
    height: 44px;
  }

  .members__close {
    width: 44px;
    height: 44px;
  }
}

@keyframes members-sheet-in {
  from {
    transform: translateY(12px);
    opacity: 0;
  }
}

@media (prefers-reduced-motion: reduce) {
  .members {
    animation: none;
  }
}
</style>
