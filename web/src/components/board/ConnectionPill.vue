<script setup lang="ts">
import { computed } from 'vue'
import { CoTooltip } from '@/components/ui'
import CoIcon from '@/components/ui/CoIcon.vue'
import type { ConnectionStatus, Role } from '@/net/types'

/**
 * 连接状态胶囊（UX-SPEC §3 / F9）。
 *
 * 四态 + 只读覆盖（只读优先显示，因为它是身份而不是连接问题）：
 *   online      已连接
 *   connecting  连接中…
 *   reconnecting 重连中 / 离线 · 待同步 N 条
 *   offline     离线
 */
const props = defineProps<{
  status: ConnectionStatus
  pending: number
  role: Role
}>()

interface PillView {
  tone: 'ok' | 'warn' | 'sync' | 'neutral'
  label: string
  icon: 'wifi' | 'wifi-off' | 'spinner' | 'eye'
  tooltip: string
}

const view = computed<PillView>(() => {
  if (props.role === 'viewer') {
    return {
      tone: 'neutral',
      label: '只读模式',
      icon: 'eye',
      tooltip: '只读模式 · 房主可授予你编辑权限',
    }
  }
  switch (props.status) {
    case 'online':
      return { tone: 'ok', label: '已连接', icon: 'wifi', tooltip: '实时同步已开启' }
    case 'connecting':
      return { tone: 'sync', label: '连接中…', icon: 'spinner', tooltip: '正在建立连接' }
    case 'reconnecting': {
      const count = props.pending
      return {
        tone: 'warn',
        label: count > 0 ? `重连中 · 待同步 ${count} 条` : '重连中…',
        icon: 'wifi-off',
        tooltip: count > 0 ? '网络恢复后会自动补齐，你的笔迹已保存在本地' : '正在尝试重新连接',
      }
    }
    case 'offline': {
      const count = props.pending
      return {
        tone: 'warn',
        label: count > 0 ? `离线 · 待同步 ${count} 条` : '离线',
        icon: 'wifi-off',
        tooltip: '离线可继续绘制，恢复网络后自动同步',
      }
    }
    default:
      return { tone: 'neutral', label: '未连接', icon: 'wifi-off', tooltip: '未建立连接' }
  }
})

const spinning = computed(() => view.value.icon === 'spinner')
</script>

<template>
  <CoTooltip :text="view.tooltip" placement="bottom">
    <span class="status-pill" :class="`status-pill--${view.tone}`" role="status" aria-live="polite">
      <span v-if="view.tone === 'ok'" class="status-pill__dot" aria-hidden="true" />
      <CoIcon v-else :name="view.icon" :size="13" :class="{ spin: spinning }" />
      <span class="status-pill__label">{{ view.label }}</span>
    </span>
  </CoTooltip>
</template>

<style scoped>
.status-pill__label {
  white-space: nowrap;
}

.status-pill--neutral {
  background: var(--brand-weak);
  color: var(--brand-deep);
}

.spin {
  animation: co-spin 0.9s linear infinite;
}

@keyframes co-spin {
  to {
    transform: rotate(360deg);
  }
}

@media (max-width: 767px) {
  .status-pill {
    height: 24px;
    padding: 0 8px;
    font-size: var(--text-2xs);
  }
}
</style>
