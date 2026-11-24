<script setup lang="ts">
import { computed } from 'vue'
import { CoIcon } from '@/components/ui'

/**
 * 移动端选中后的浮动小条（UX-SPEC §7.5）：只出现「删除」与「取消」。
 * 位置由父级通过 style 传入，组件本身不测量几何。
 */
const props = defineProps<{
  count: number
}>()

const emit = defineEmits<{ delete: []; cancel: [] }>()

const label = computed(() => (props.count > 1 ? `删除 ${props.count} 个` : '删除'))
</script>

<template>
  <div class="sel-bar" role="group" aria-label="选中操作">
    <button type="button" class="sel-bar__btn sel-bar__btn--danger" @click="emit('delete')">
      <CoIcon name="trash" :size="16" />
      <span>{{ label }}</span>
    </button>
    <span class="sel-bar__sep" aria-hidden="true" />
    <button type="button" class="sel-bar__btn" @click="emit('cancel')">取消</button>
  </div>
</template>

<style scoped>
.sel-bar {
  display: inline-flex;
  align-items: center;
  height: 44px;
  padding: 0 4px;
  border-radius: var(--radius-full);
  background: var(--ink);
  color: #fff;
  box-shadow: var(--shadow-md);
  animation: sel-bar-in var(--dur) var(--ease-out);
}

.sel-bar__btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 36px;
  padding: 0 14px;
  border: 0;
  border-radius: var(--radius-full);
  background: transparent;
  color: inherit;
  font-size: var(--text-sm);
  font-weight: 500;
  white-space: nowrap;
  cursor: pointer;
}

.sel-bar__btn--danger {
  color: #ffb4a2;
}

.sel-bar__btn:hover {
  background: rgba(255, 255, 255, 0.1);
}

.sel-bar__sep {
  width: 1px;
  height: 18px;
  background: rgba(255, 255, 255, 0.18);
}

@keyframes sel-bar-in {
  from {
    opacity: 0;
    transform: translate(-50%, 6px);
  }
}
</style>
