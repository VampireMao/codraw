<script setup lang="ts">
import { computed } from 'vue'
import { CoIcon, CoTooltip } from '@/components/ui'

const props = defineProps<{
  zoom: number
}>()

const emit = defineEmits<{ zoom: [percent: number]; fit: [] }>()

const canZoomOut = computed(() => props.zoom > 10)
const canZoomIn = computed(() => props.zoom < 800)
</script>

<template>
  <div class="zoom-cluster" role="group" aria-label="缩放控制">
    <CoTooltip text="缩小" shortcut="−" placement="top">
      <button
        type="button"
        class="zoom-cluster__btn"
        aria-label="缩小"
        :disabled="!canZoomOut"
        @click="emit('zoom', Math.max(10, props.zoom - 25))"
      >
        <CoIcon name="zoom-out" :size="17" />
      </button>
    </CoTooltip>

    <button
      type="button"
      class="zoom-cluster__value mono"
      aria-label="当前缩放，点击恢复 100%"
      @click="emit('zoom', 100)"
    >
      {{ props.zoom }}%
    </button>

    <CoTooltip text="放大" shortcut="+" placement="top">
      <button
        type="button"
        class="zoom-cluster__btn"
        aria-label="放大"
        :disabled="!canZoomIn"
        @click="emit('zoom', Math.min(800, props.zoom + 25))"
      >
        <CoIcon name="zoom-in" :size="17" />
      </button>
    </CoTooltip>

    <span class="zoom-cluster__sep" aria-hidden="true" />

    <CoTooltip text="适配视图" shortcut="0" placement="top">
      <button type="button" class="zoom-cluster__btn" aria-label="适配视图" @click="emit('fit')">
        <CoIcon name="fit" :size="17" />
      </button>
    </CoTooltip>
  </div>
</template>

<style scoped>
.zoom-cluster {
  display: inline-flex;
  align-items: center;
  gap: 1px;
  height: 38px;
  padding: 0 4px;
  border-radius: var(--radius-md);
  border: 1px solid var(--line);
  background: var(--panel);
  box-shadow: var(--shadow);
}

.zoom-cluster__btn {
  display: grid;
  place-items: center;
  width: 30px;
  height: 30px;
  border: 0;
  border-radius: var(--radius-xs);
  background: transparent;
  color: var(--ink-soft);
  cursor: pointer;
  transition:
    background-color var(--dur-fast) var(--ease),
    color var(--dur-fast) var(--ease);
}

.zoom-cluster__btn:hover:not(:disabled) {
  background: var(--surface-sunk);
  color: var(--ink);
}

.zoom-cluster__btn:disabled {
  opacity: 0.3;
  cursor: not-allowed;
}

.zoom-cluster__value {
  min-width: 50px;
  padding: 0 2px;
  border: 0;
  background: transparent;
  font-size: var(--text-xs);
  color: var(--ink-soft);
  text-align: center;
  cursor: pointer;
  border-radius: var(--radius-xs);
  transition: color var(--dur-fast) var(--ease);
}

.zoom-cluster__value:hover {
  color: var(--ink);
}

.zoom-cluster__sep {
  width: 1px;
  height: 18px;
  margin: 0 3px;
  background: var(--line);
}

@media (max-width: 767px) {
  .zoom-cluster {
    display: none;
  }
}
</style>
