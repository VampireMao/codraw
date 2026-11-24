<script setup lang="ts">
import { useRouter } from 'vue-router'
import { CoButton, CoIcon } from '@/components/ui'

const router = useRouter()

function goHome(): void {
  void router.push({ name: 'home' })
}
</script>

<template>
  <div class="nf">
    <div class="nf__inner">
      <span class="nf__code mono">404</span>
      <h1 class="nf__title">房间不存在或已过期</h1>
      <p class="nf__text">
        链接可能输错了一位，或者这个房间已经过期被清理。
        <br />
        重新建一个白板，几秒钟的事。
      </p>
      <div class="nf__actions">
        <CoButton variant="primary" size="lg" @click="goHome">
          <template #icon>
            <CoIcon name="plus" :size="18" />
          </template>
          新建一个白板
        </CoButton>
        <CoButton variant="quiet" size="lg" @click="goHome">返回首页</CoButton>
      </div>
    </div>
  </div>
</template>

<style scoped>
.nf {
  display: grid;
  place-items: center;
  min-height: 100%;
  padding: 40px 24px;
  background: var(--paper);
}

.nf__inner {
  max-width: 480px;
  text-align: center;
}

.nf__code {
  display: inline-block;
  margin-bottom: 20px;
  font-size: var(--text-xs);
  letter-spacing: 0.3em;
  color: var(--ink-soft);
  opacity: 0.5;
}

.nf__title {
  margin: 0 0 14px;
  font-size: clamp(22px, 3.4vw, 30px);
  font-weight: 600;
  letter-spacing: -0.02em;
  color: var(--ink);
}

.nf__text {
  margin: 0;
  font-size: var(--text-base);
  line-height: 1.75;
  color: var(--ink-soft);
}

.nf__actions {
  display: flex;
  justify-content: center;
  align-items: center;
  gap: var(--space-2);
  margin-top: 32px;
  flex-wrap: wrap;
}

@media (max-width: 767px) {
  .nf__actions {
    flex-direction: column;
    align-items: stretch;
  }
}
</style>
