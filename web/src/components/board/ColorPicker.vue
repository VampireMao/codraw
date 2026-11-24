<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { CoTooltip } from '@/components/ui'
import { INK_COLORS } from '@/canvas/shapes'
import { useBoardStore } from '@/stores/board'

/**
 * ColorPicker：画笔颜色选择。
 *
 * 默认色来自 `store.effectiveInkColor()`——进入房间时被设成自己的头像色
 * （服务端按 clientId 分配），所以每个人默认就是不同的颜色。
 *
 * **为什么面板要 Teleport 到 body**：工具栏是 `overflow-y: auto` 的容器，
 * 面板定位到工具栏外侧会被直接裁掉（DOM 里有、屏幕上看不见）。
 * 用 fixed + Teleport 既避开裁剪，也不受画布容器影响。
 */
const store = useBoardStore()
const open = ref(false)
const triggerEl = ref<HTMLElement | null>(null)
const panelStyle = ref<Record<string, string>>({})

const current = computed(() => store.effectiveInkColor())

/** 面板尺寸与触发按钮对齐的间距（与 CSS 中的 gap 保持一致）。 */
const GAP = 10

function positionPanel(): void {
  const el = triggerEl.value
  if (!el) return
  const rect = el.getBoundingClientRect()
  const panelWidth = 128
  const panelHeight = 128

  // 桌面：面板在工具栏右侧；右侧空间不足时翻到左侧。
  const spaceRight = window.innerWidth - rect.right
  const openLeft = spaceRight < panelWidth + GAP + 8

  const left = openLeft ? rect.left - panelWidth - GAP : rect.right + GAP
  // 垂直方向以按钮为中心，并夹取在视口内。
  const top = Math.min(
    Math.max(8, rect.top + rect.height / 2 - panelHeight / 2),
    window.innerHeight - panelHeight - 8,
  )

  panelStyle.value = {
    left: `${Math.max(8, left)}px`,
    top: `${top}px`,
  }
}

function toggle(): void {
  open.value = !open.value
  if (open.value) {
    // 先让 DOM 更新再量尺寸，否则面板可能还没挂载。
    requestAnimationFrame(positionPanel)
  }
}

function pick(color: string): void {
  store.setInkColor(color)
  open.value = false
}

function close(): void {
  open.value = false
}

/** 点击面板外部时收起。 */
function onDocPointerDown(e: PointerEvent): void {
  if (!open.value) return
  const target = e.target as HTMLElement | null
  if (!target) return
  if (triggerEl.value?.contains(target)) return
  const panel = document.querySelector('.color__panel')
  if (panel?.contains(target)) return
  close()
}

function onKey(e: KeyboardEvent): void {
  if (e.key === 'Escape' && open.value) close()
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

onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', onDocPointerDown, true)
  window.removeEventListener('keydown', onKey)
  window.removeEventListener('resize', positionPanel)
  window.removeEventListener('scroll', positionPanel, true)
})
</script>

<template>
  <div class="color">
    <CoTooltip text="画笔颜色" placement="right">
      <button
        ref="triggerEl"
        type="button"
        class="color__trigger"
        :aria-label="`画笔颜色 ${current}`"
        :aria-expanded="open"
        aria-haspopup="listbox"
        @click="toggle"
      >
        <span class="color__swatch" :style="{ background: current }" />
      </button>
    </CoTooltip>

    <Teleport to="body">
      <div
        v-if="open"
        class="color__panel"
        role="listbox"
        aria-label="颜色选择"
        :style="panelStyle"
        @pointerdown.stop
      >
        <button
          v-for="color in INK_COLORS"
          :key="color"
          type="button"
          class="color__item"
          role="option"
          :aria-selected="color === current"
          :aria-label="color"
          :class="{ 'color__item--active': color === current }"
          :style="{ background: color }"
          @click="pick(color)"
        >
          <svg v-if="color === current" viewBox="0 0 16 16" class="color__check" aria-hidden="true">
            <path
              d="M3.5 8.5l3 3 6-6.5"
              fill="none"
              stroke="#fff"
              stroke-width="2"
              stroke-linecap="round"
              stroke-linejoin="round"
            />
          </svg>
        </button>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.color {
  position: relative;
}

.color__trigger {
  display: grid;
  place-items: center;
  width: var(--btn, 44px);
  height: var(--btn, 44px);
  border: 0;
  border-radius: var(--radius-sm);
  background: transparent;
  cursor: pointer;
  -webkit-tap-highlight-color: transparent;
}

.color__trigger:hover {
  background: var(--surface-sunk);
}

.color__trigger:focus-visible {
  outline: 2px solid var(--brand);
  outline-offset: -2px;
}

.color__swatch {
  width: 22px;
  height: 22px;
  border-radius: 50%;
  border: 2px solid var(--panel);
  box-shadow: 0 0 0 1px var(--line);
}

/*
 * 面板被 Teleport 到 body，所以这里是全局坐标（fixed）。
 * scoped 样式仍会作用到它，因为 scoped 属性会跟着节点一起移动。
 */
.color__panel {
  position: fixed;
  z-index: var(--z-popover, 40);
  display: grid;
  grid-template-columns: repeat(3, 30px);
  gap: 8px;
  padding: 10px;
  width: 128px;
  box-sizing: border-box;
  border-radius: var(--radius-md);
  background: var(--panel);
  border: 1px solid var(--line);
  box-shadow: var(--shadow-md, var(--shadow));
}

.color__item {
  position: relative;
  display: grid;
  place-items: center;
  width: 30px;
  height: 30px;
  border: 0;
  border-radius: 50%;
  cursor: pointer;
  padding: 0;
  box-shadow: 0 0 0 1px rgb(0 0 0 / 8%);
  transition: transform var(--dur-fast) var(--ease);
}

.color__item:hover {
  transform: scale(1.1);
}

.color__item--active {
  box-shadow:
    0 0 0 2px var(--panel),
    0 0 0 4px currentColor;
}

.color__item:focus-visible {
  outline: 2px solid var(--ink);
  outline-offset: 2px;
}

.color__check {
  width: 16px;
  height: 16px;
}
</style>
