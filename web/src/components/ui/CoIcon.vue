<script setup lang="ts">
import { computed } from 'vue'
import { ICON_PATHS, type IconName } from './icons'

/**
 * CoIcon：内联 SVG，统一 1.5px 描边（不用图标库、不用 emoji）。
 * 用 currentColor 描边，颜色随父级文字色，天然支持禁用态与主题切换。
 */
const props = withDefaults(
  defineProps<{
    name: IconName
    size?: number
    /** 描边宽度，默认 1.5 */
    strokeWidth?: number
  }>(),
  { size: 20, strokeWidth: 1.5 },
)

const paths = computed(() => ICON_PATHS[props.name] ?? [])
</script>

<template>
  <svg
    class="co-icon"
    :width="props.size"
    :height="props.size"
    viewBox="0 0 24 24"
    fill="none"
    :stroke-width="props.strokeWidth"
    stroke="currentColor"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
  >
    <path
      v-for="(path, index) in paths"
      :key="index"
      :d="path.d"
      :fill="path.fill ? 'currentColor' : 'none'"
      :stroke="path.fill ? 'none' : 'currentColor'"
    />
  </svg>
</template>

<style scoped>
.co-icon {
  display: block;
  flex: none;
  overflow: visible;
}
</style>
