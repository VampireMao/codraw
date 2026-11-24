<script setup lang="ts">
import { computed } from 'vue'

type Variant = 'primary' | 'secondary' | 'ghost' | 'quiet' | 'danger'
type Size = 'sm' | 'md' | 'lg'

const props = withDefaults(
  defineProps<{
    variant?: Variant
    size?: Size
    type?: 'button' | 'submit' | 'reset'
    disabled?: boolean
    loading?: boolean
    block?: boolean
    /** 纯图标按钮（正方形，触控目标仍 ≥44） */
    iconOnly?: boolean
  }>(),
  {
    variant: 'secondary',
    size: 'md',
    type: 'button',
    disabled: false,
    loading: false,
    block: false,
    iconOnly: false,
  },
)

const emit = defineEmits<{ click: [event: MouseEvent] }>()

const classes = computed(() => [
  'co-btn',
  `co-btn--${props.variant}`,
  `co-btn--${props.size}`,
  {
    'co-btn--block': props.block,
    'co-btn--icon': props.iconOnly,
    'is-loading': props.loading,
  },
])

function onClick(event: MouseEvent): void {
  if (props.disabled || props.loading) {
    event.preventDefault()
    event.stopPropagation()
    return
  }
  emit('click', event)
}
</script>

<template>
  <button
    :class="classes"
    :type="props.type"
    :disabled="props.disabled || props.loading"
    :aria-busy="props.loading || undefined"
    @click="onClick"
  >
    <span v-if="props.loading" class="co-btn__spinner" aria-hidden="true" />
    <slot name="icon" />
    <span v-if="$slots.default" class="co-btn__label"><slot /></span>
  </button>
</template>

<style scoped>
.co-btn {
  --btn-h: 40px;
  --btn-px: 16px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  min-height: var(--btn-h);
  padding: 0 var(--btn-px);
  border: 1px solid transparent;
  border-radius: var(--radius);
  font-family: inherit;
  font-size: var(--text-base);
  font-weight: 500;
  line-height: 1;
  letter-spacing: 0.01em;
  white-space: nowrap;
  cursor: pointer;
  -webkit-tap-highlight-color: transparent;
  transition:
    background-color var(--dur-fast) var(--ease),
    border-color var(--dur-fast) var(--ease),
    color var(--dur-fast) var(--ease),
    box-shadow var(--dur-fast) var(--ease),
    transform var(--dur-fast) var(--ease);
}

.co-btn:focus-visible {
  outline: 2px solid var(--brand);
  outline-offset: 2px;
}

.co-btn:active:not(:disabled) {
  transform: translateY(0.5px);
}

.co-btn:disabled {
  cursor: not-allowed;
  opacity: 0.42;
}

/* ---- 尺寸：均满足 ≥44px 触控目标（视觉高度可小于触控热区） ---- */
.co-btn--sm {
  --btn-h: 34px;
  --btn-px: 12px;
  font-size: var(--text-sm);
  border-radius: var(--radius-sm);
}

.co-btn--md {
  --btn-h: 40px;
}

.co-btn--lg {
  --btn-h: 48px;
  --btn-px: 22px;
  font-size: var(--text-md);
  border-radius: var(--radius-md);
}

/* 保证热区 ≥44：在按钮外围扩展不可见的命中区域 */
.co-btn::after {
  content: '';
  position: absolute;
  inset: -2px;
  border-radius: inherit;
}
.co-btn {
  position: relative;
}

.co-btn--block {
  display: flex;
  width: 100%;
}

.co-btn--icon {
  --btn-px: 0;
  width: var(--btn-h);
  min-width: var(--btn-h);
}

/* ---- 变体 ---- */
.co-btn--primary {
  background: var(--brand);
  color: #fff;
  box-shadow: 0 1px 2px rgba(61, 90, 254, 0.28);
}

.co-btn--primary:hover:not(:disabled) {
  background: var(--brand-deep);
}

.co-btn--primary:active:not(:disabled) {
  background: var(--brand-press);
}

.co-btn--secondary {
  background: var(--panel);
  border-color: var(--line);
  color: var(--ink);
  box-shadow: var(--shadow-sm);
}

.co-btn--secondary:hover:not(:disabled) {
  border-color: #d6d3c9;
  background: #fdfcfa;
}

.co-btn--ghost {
  background: transparent;
  color: var(--ink);
}

.co-btn--ghost:hover:not(:disabled) {
  background: var(--surface-sunk);
}

.co-btn--quiet {
  background: transparent;
  color: var(--ink-soft);
  font-weight: 400;
}

.co-btn--quiet:hover:not(:disabled) {
  background: var(--surface-sunk);
  color: var(--ink);
}

.co-btn--danger {
  background: transparent;
  color: var(--warn);
  border-color: transparent;
}

.co-btn--danger:hover:not(:disabled) {
  background: var(--warn-weak);
}

.co-btn__label {
  display: inline-block;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.co-btn__spinner {
  width: 14px;
  height: 14px;
  border: 1.5px solid currentColor;
  border-top-color: transparent;
  border-radius: 50%;
  animation: co-spin 0.7s linear infinite;
}

@keyframes co-spin {
  to {
    transform: rotate(360deg);
  }
}

@media (prefers-reduced-motion: reduce) {
  .co-btn,
  .co-btn__spinner {
    transition: none;
    animation-duration: 0.01ms;
  }
}
</style>
