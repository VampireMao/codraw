<script setup lang="ts">
import { computed } from 'vue'
import { CoIcon, CoTooltip } from '@/components/ui'
import ToolButton from './ToolButton.vue'
import ColorPicker from './ColorPicker.vue'
import type { ToolId } from '@/canvas/tools/types'
import { useBoardStore } from '@/stores/board'

/**
 * Toolbar：一套 DOM，两种排版。
 * 桌面 → 左侧悬浮竖条（56px 宽，圆角 12，--shadow），共 9 个 44px 按钮，
 *        总高 44*9 + 分组间隙 ≈ 420px，在 600px 高的窗口里也不会溢出。
 * 移动 → 底部横条（60px + 安全区，UX-SPEC §6）。
 * 缩放按钮不进侧栏：桌面在右下角独立成组（ZoomCluster），移动端隐藏。
 */
const store = useBoardStore()

const tools: {
  id: ToolId
  icon: 'cursor' | 'pen' | 'rect' | 'arrow' | 'note' | 'eraser'
  label: string
  key: string
}[] = [
  { id: 'select', icon: 'cursor', label: '选择', key: 'V' },
  { id: 'pen', icon: 'pen', label: '画笔', key: 'P' },
  { id: 'rect', icon: 'rect', label: '矩形', key: 'R' },
  { id: 'arrow', icon: 'arrow', label: '箭头', key: 'A' },
  { id: 'note', icon: 'note', label: '便签', key: 'N' },
  { id: 'eraser', icon: 'eraser', label: '橡皮', key: 'E' },
]

const canEdit = computed(() => store.canEdit)

/** 撤销/重做的可用状态，由 BoardCanvas 通过 history 事件上报 */
withDefaults(defineProps<{ history?: { canUndo: boolean; canRedo: boolean } }>(), {
  history: () => ({ canUndo: false, canRedo: false }),
})

const emit = defineEmits<{
  undo: []
  redo: []
  fit: []
}>()
</script>

<template>
  <div class="toolbar" :class="{ 'toolbar--readonly': !canEdit }" role="toolbar" aria-label="绘图工具">
    <div v-if="canEdit" class="toolbar__group">
      <ToolButton
        v-for="item in tools"
        :key="item.id"
        :tool="item.id"
        :active="store.tool"
        :label="item.label"
        :shortcut="item.key"
        :icon="item.icon"
        @select="store.setTool($event)"
      />
    </div>

    <div v-if="canEdit" class="toolbar__divider" aria-hidden="true" />

    <div v-if="canEdit" class="toolbar__group">
      <ColorPicker />
    </div>

    <div v-if="canEdit" class="toolbar__divider" aria-hidden="true" />

    <div v-if="canEdit" class="toolbar__group">
      <CoTooltip text="撤销" shortcut="Ctrl+Z" placement="right">
        <button
          type="button"
          class="tool-btn"
          aria-label="撤销"
          :disabled="!history.canUndo"
          @click="emit('undo')"
        >
          <CoIcon name="undo" :size="20" />
        </button>
      </CoTooltip>
      <CoTooltip text="重做" shortcut="Ctrl+Shift+Z" placement="right">
        <button
          type="button"
          class="tool-btn tool-btn--desktop-only"
          aria-label="重做"
          :disabled="!history.canRedo"
          @click="emit('redo')"
        >
          <CoIcon name="redo" :size="20" />
        </button>
      </CoTooltip>
    </div>

    <div v-if="canEdit" class="toolbar__divider" aria-hidden="true" />

    <div class="toolbar__group">
      <CoTooltip text="适配视图" shortcut="0" placement="right">
        <button type="button" class="tool-btn" aria-label="适配视图" @click="emit('fit')">
          <CoIcon name="fit" :size="20" />
        </button>
      </CoTooltip>
    </div>
  </div>
</template>

<style scoped>
.toolbar {
  --btn: 44px;
  position: absolute;
  top: 50%;
  left: 16px;
  transform: translateY(-50%);
  z-index: var(--z-chrome);
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 2px;
  padding: 6px;
  border-radius: var(--radius-md);
  background: var(--panel);
  border: 1px solid var(--line);
  box-shadow: var(--shadow);
  /* 窗口很矮时能收缩滚动，绝不越过顶栏/底边 */
  max-height: calc(100% - 32px);
  overflow-y: auto;
  scrollbar-width: none;
}

.toolbar::-webkit-scrollbar {
  display: none;
}

.toolbar__group {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.toolbar__divider {
  width: 28px;
  height: 1px;
  margin: 3px 0;
  background: var(--line);
  flex: none;
}

/* 与 ToolButton 内部 .tool-btn 保持同一视觉规格（scoped 边界内重复声明是必要的） */
.tool-btn {
  position: relative;
  display: grid;
  place-items: center;
  width: var(--btn);
  height: var(--btn);
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

.tool-btn:disabled {
  opacity: 0.34;
  cursor: default;
}

.tool-btn:focus-visible {
  outline: 2px solid var(--brand);
  outline-offset: -2px;
}

/* ---------------- 移动端：底部横条 ---------------- */
@media (max-width: 767px) {
  .toolbar {
    top: auto;
    bottom: 0;
    left: 0;
    right: 0;
    transform: none;
    flex-direction: row;
    gap: 0;
    width: auto;
    max-height: none;
    height: calc(var(--mobile-bar-h) + var(--safe-bottom));
    padding: 0 6px var(--safe-bottom);
    border-radius: 0;
    border: 0;
    border-top: 1px solid var(--line);
    box-shadow: var(--shadow-up);
    justify-content: space-between;
    overflow: visible;
  }

  .toolbar__group {
    flex-direction: row;
    gap: 0;
    flex: 1;
    justify-content: space-around;
  }

  .tool-btn--desktop-only {
    display: none;
  }

  .toolbar__divider {
    width: 1px;
    height: 22px;
    margin: 0 2px;
    flex: none;
  }
}
</style>
