<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import CoIcon from './CoIcon.vue'

/**
 * CoModal：原生 <dialog> 实现。
 * 好处：焦点陷阱、Esc 关闭、`::backdrop`、惰性内容都由浏览器负责，
 * 比自己拼 div + 手写 focus trap 少 100 行且行为更正确。
 */
const props = withDefaults(
  defineProps<{
    open: boolean
    title?: string
    description?: string
    /** 点击遮罩是否关闭 */
    dismissable?: boolean
    width?: number
  }>(),
  { dismissable: true, width: 440, title: '', description: '' },
)

const emit = defineEmits<{ close: [] }>()

const dialogEl = ref<HTMLDialogElement | null>(null)

function sync(next: boolean): void {
  const el = dialogEl.value
  if (!el) return
  if (next && !el.open) {
    el.showModal()
  } else if (!next && el.open) {
    el.close()
  }
}

onMounted(() => sync(props.open))
onBeforeUnmount(() => {
  const el = dialogEl.value
  if (el?.open) el.close()
})

defineExpose({ sync })

function onCancel(event: Event): void {
  event.preventDefault()
  if (props.dismissable) emit('close')
}

function onBackdrop(event: MouseEvent): void {
  if (!props.dismissable) return
  const el = dialogEl.value
  if (!el) return
  // 只有点在 dialog 元素本身（即 backdrop 区域）才算关闭
  if (event.target === el) emit('close')
}

function onClose(): void {
  if (props.open) emit('close')
}

watch(
  () => props.open,
  (next) => sync(next),
)
</script>

<template>
  <dialog
    ref="dialogEl"
    class="co-modal"
    :style="{ '--modal-w': `${props.width}px` }"
    @cancel="onCancel"
    @click="onBackdrop"
    @close="onClose"
  >
    <div class="co-modal__surface">
      <header v-if="props.title || $slots.header" class="co-modal__header">
        <div class="co-modal__heading">
          <slot name="header">
            <h2 class="co-modal__title">{{ props.title }}</h2>
            <p v-if="props.description" class="co-modal__desc">{{ props.description }}</p>
          </slot>
        </div>
        <button class="co-modal__close" type="button" aria-label="关闭" @click="emit('close')">
          <CoIcon name="close" :size="18" />
        </button>
      </header>

      <div class="co-modal__body">
        <slot />
      </div>

      <footer v-if="$slots.footer" class="co-modal__footer">
        <slot name="footer" />
      </footer>
    </div>
  </dialog>
</template>

<style scoped>
.co-modal {
  width: min(var(--modal-w), calc(100vw - 32px));
  max-height: min(88vh, 720px);
  padding: 0;
  border: 0;
  border-radius: var(--radius-lg);
  background: var(--panel);
  color: var(--ink);
  box-shadow: var(--shadow);
  overflow: visible;
}

.co-modal::backdrop {
  background: var(--overlay);
  backdrop-filter: none;
}

.co-modal[open] {
  animation: co-modal-in var(--dur) var(--ease-out);
}

.co-modal__surface {
  display: flex;
  flex-direction: column;
  max-height: inherit;
  border-radius: inherit;
  overflow: hidden;
}

.co-modal__header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
  padding: var(--space-6) var(--space-6) var(--space-3);
}

.co-modal__heading {
  min-width: 0;
}

.co-modal__title {
  margin: 0;
  font-size: var(--text-md);
  font-weight: 600;
  line-height: var(--leading-tight);
  letter-spacing: 0.01em;
}

.co-modal__desc {
  margin: 6px 0 0;
  font-size: var(--text-sm);
  line-height: var(--leading);
  color: var(--ink-soft);
}

.co-modal__close {
  display: grid;
  place-items: center;
  flex: none;
  width: 32px;
  height: 32px;
  margin: -4px -4px 0 0;
  border: 0;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--ink-soft);
  cursor: pointer;
  transition:
    background-color var(--dur-fast) var(--ease),
    color var(--dur-fast) var(--ease);
}

.co-modal__close:hover {
  background: var(--surface-sunk);
  color: var(--ink);
}

.co-modal__body {
  padding: 0 var(--space-6) var(--space-6);
  overflow-y: auto;
  overscroll-behavior: contain;
}

.co-modal__footer {
  display: flex;
  justify-content: flex-end;
  gap: var(--space-2);
  padding: var(--space-4) var(--space-6);
  border-top: 1px solid var(--line);
  background: #fcfcfa;
}

@keyframes co-modal-in {
  from {
    opacity: 0;
    transform: translateY(8px) scale(0.99);
  }
}

@media (prefers-reduced-motion: reduce) {
  .co-modal[open] {
    animation: none;
  }
}
</style>
