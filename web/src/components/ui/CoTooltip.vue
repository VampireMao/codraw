<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'

/**
 * CoTooltip：桌面端 hover + 键盘 focus 显示，移动端不显示（UX 要求）。
 * 用 fixed 定位 + 视口夹取，避免画布容器 overflow 裁剪。
 */
const props = withDefaults(
  defineProps<{
    text: string
    placement?: 'top' | 'bottom' | 'right' | 'left'
    shortcut?: string
    disabled?: boolean
  }>(),
  { placement: 'top', disabled: false, shortcut: '' },
)

const trigger = ref<HTMLElement | null>(null)
const visible = ref(false)
const style = ref<Record<string, string>>({})

let hideTimer = 0

/** 粗指针 = 触屏；不渲染 tooltip（UX-SPEC §6 要求移动端不显示） */
function isCoarsePointer(): boolean {
  return window.matchMedia('(hover: none), (pointer: coarse)').matches
}

function show(): void {
  if (props.disabled || isCoarsePointer() || !props.text) return
  window.clearTimeout(hideTimer)
  visible.value = true
  void nextTick(position)
}

function hide(): void {
  window.clearTimeout(hideTimer)
  hideTimer = window.setTimeout(() => {
    visible.value = false
  }, 60)
}

function position(): void {
  const el = trigger.value
  if (!el) return
  const rect = el.getBoundingClientRect()
  const gap = 10
  const map: Record<string, Record<string, string>> = {
    top: {
      left: `${rect.left + rect.width / 2}px`,
      top: `${rect.top - gap}px`,
      transform: 'translate(-50%, -100%)',
    },
    bottom: {
      left: `${rect.left + rect.width / 2}px`,
      top: `${rect.bottom + gap}px`,
      transform: 'translate(-50%, 0)',
    },
    right: {
      left: `${rect.right + gap}px`,
      top: `${rect.top + rect.height / 2}px`,
      transform: 'translate(0, -50%)',
    },
    left: {
      left: `${rect.left - gap}px`,
      top: `${rect.top + rect.height / 2}px`,
      transform: 'translate(-100%, -50%)',
    },
  }
  style.value = map[props.placement] ?? map.top ?? {}
}

onBeforeUnmount(() => window.clearTimeout(hideTimer))

watch(
  () => props.text,
  () => {
    if (visible.value) void nextTick(position)
  },
)
</script>

<template>
  <span
    ref="trigger"
    class="co-tooltip-trigger"
    @pointerenter="show"
    @pointerleave="hide"
    @focusin="show"
    @focusout="hide"
    @pointerdown="hide"
  >
    <slot />
  </span>
  <Teleport to="body">
    <span v-if="visible" class="co-tooltip" :style="style" role="tooltip">
      {{ props.text }}
      <kbd v-if="props.shortcut" class="co-tooltip__key">{{ props.shortcut }}</kbd>
    </span>
  </Teleport>
</template>

<style scoped>
.co-tooltip-trigger {
  display: inline-flex;
}

.co-tooltip {
  position: fixed;
  z-index: var(--z-toast);
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 6px 9px;
  border-radius: var(--radius-xs);
  background: var(--ink);
  color: #fff;
  font-size: var(--text-xs);
  line-height: 1.4;
  font-weight: 400;
  white-space: nowrap;
  box-shadow: var(--shadow-md);
  pointer-events: none;
  animation: co-tooltip-in var(--dur-fast) var(--ease-out);
}

.co-tooltip__key {
  padding: 1px 4px;
  border-radius: 3px;
  background: rgba(255, 255, 255, 0.16);
  font-family: var(--font-mono);
  font-size: 10px;
  line-height: 1.4;
  letter-spacing: 0.04em;
}

@keyframes co-tooltip-in {
  from {
    opacity: 0;
  }
}

@media (hover: none), (pointer: coarse) {
  .co-tooltip {
    display: none;
  }
}
</style>
