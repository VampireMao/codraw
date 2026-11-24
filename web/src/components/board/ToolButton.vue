<script setup lang="ts">
import type { IconName } from '@/components/ui/icons'
import { computed } from 'vue'
import CoIcon from '@/components/ui/CoIcon.vue'
import CoTooltip from '@/components/ui/CoTooltip.vue'
import type { ToolId } from '@/canvas/tools/types'

/**
 * 工具按钮的视觉规格统一在此处：
 * 桌面端 56px 侧栏里是 44×44 的方按钮；移动端 60px 底栏里也是 44×44。
 * 两者共用同一个 class，靠父容器变量控制尺寸，避免两套组件。
 */
const props = defineProps<{
  tool: ToolId
  active: ToolId
  label: string
  icon: IconName
  shortcut?: string
  disabled?: boolean
  /** 桌面侧栏用 right 提示，移动端不显示提示 */
  tooltipSide?: 'right' | 'top'
}>()

const emit = defineEmits<{ select: [tool: ToolId] }>()

const isActive = computed(() => props.tool === props.active)
</script>

<template>
  <CoTooltip :text="props.label" :shortcut="props.shortcut" :placement="props.tooltipSide ?? 'right'">
    <button
      type="button"
      class="tool-btn"
      :class="{ 'tool-btn--active': isActive }"
      :aria-label="props.label"
      :aria-pressed="isActive"
      :disabled="props.disabled"
      @click="emit('select', props.tool)"
    >
      <CoIcon :name="props.icon" :size="20" />
    </button>
  </CoTooltip>
</template>

<style scoped>
.tool-btn {
  position: relative;
  display: grid;
  place-items: center;
  width: 44px;
  height: 44px;
  border: 0;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--ink-soft);
  cursor: pointer;
  -webkit-tap-highlight-color: transparent;
  transition:
    background-color var(--dur-fast) var(--ease),
    color var(--dur-fast) var(--ease);
}

.tool-btn:hover:not(:disabled) {
  background: var(--surface-sunk);
  color: var(--ink);
}

.tool-btn:focus-visible {
  outline: 2px solid var(--brand);
  outline-offset: -2px;
}

.tool-btn--active {
  background: var(--brand-weak);
  color: var(--brand);
}

.tool-btn--active:hover:not(:disabled) {
  background: #e3e8ff;
  color: var(--brand);
}

.tool-btn:disabled {
  opacity: 0.32;
  cursor: not-allowed;
}
</style>
