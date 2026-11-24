<script setup lang="ts">
/**
 * /perf 性能自测页（dev 或 ?debug=1 才可访问）。
 *
 * 指标说明：测的是「一次完整重绘（网格 + 可见图形 + overlay）耗时」，
 * 而不是 rAF 帧率——离屏/后台窗口的 rAF 会被浏览器限流到 1Hz，
 * 那是节流结果，不是画布性能。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import BoardCanvas from '@/components/board/BoardCanvas.vue'
import { useBoardStore } from '@/stores/board'
import { generateStrokes, measureCanvas, formatPerfReport, type PerfReport } from '@/canvas/perf'
import { CoButton, CoIcon } from '@/components/ui'

const store = useBoardStore()
const running = ref(false)
const report = ref<PerfReport | null>(null)
const mode = ref<'fit' | 'zoom800'>('fit')
const count = ref(1000)
const canvasRef = ref<InstanceType<typeof BoardCanvas> | null>(null)
const log = ref<string[]>([])

const summary = computed(() => (report.value ? formatPerfReport(report.value) : ''))
const available = computed(() => {
  const dev = import.meta.env.DEV
  const debug = new URLSearchParams(window.location.search).get('debug') === '1'
  return dev || debug
})

function fill(): void {
  store.setShapes(generateStrokes(count.value))
  log.value = [`已生成 ${count.value} 条笔迹`, ...log.value].slice(0, 6)
}

function run(): void {
  if (running.value) return
  running.value = true
  report.value = null
  if (store.shapes.length !== count.value) fill()

  requestAnimationFrame(() => {
    const canvas = canvasRef.value
    const vp = canvas?.getViewport()
    const renderer = canvas?.getRenderer()
    if (!vp || !renderer) {
      log.value = ['渲染器未就绪', ...log.value]
      running.value = false
      return
    }

    // 让所有图形都落在视口内（最坏情况：无裁剪收益）
    vp.fit(store.bounds ?? null, 8)
    if (mode.value === 'zoom800') vp.zoomAt({ x: vp.width / 2, y: vp.height / 2 }, 8)

    // 等一帧确保 canvas 尺寸已按当前视口调整
    requestAnimationFrame(() => {
      const result = measureCanvas(store.shapes, renderer, 40)
      report.value = result
      log.value = [formatPerfReport(result), ...log.value].slice(0, 6)
      running.value = false
    })
  })
}

onMounted(() => {
  if (available.value) fill()
})

onBeforeUnmount(() => {
  /* 无需清理 */
})
</script>

<template>
  <div class="perf">
    <BoardCanvas ref="canvasRef" :read-only="true" :connected="false" />

    <aside class="perf__panel">
      <header class="perf__head">
        <CoIcon name="spinner" :size="16" />
        <h1>画布性能自测</h1>
      </header>
      <p class="perf__desc">
        测「一次完整重绘」的耗时（网格 + 全部图形 + overlay），与 vsync 无关，是判断帧预算的正确指标。
      </p>

      <template v-if="available">
        <div class="perf__row">
          <label class="perf__label" for="perf-count">笔迹数</label>
          <select id="perf-count" v-model.number="count" class="perf__select" @change="fill">
            <option :value="200">200</option>
            <option :value="500">500</option>
            <option :value="1000">1000</option>
            <option :value="2000">2000</option>
          </select>
        </div>

        <div class="perf__row">
          <label class="perf__label" for="perf-mode">视口</label>
          <select id="perf-mode" v-model="mode" class="perf__select">
            <option value="fit">全部可见（最坏）</option>
            <option value="zoom800">放大 800%</option>
          </select>
        </div>

        <div class="perf__actions">
          <CoButton variant="primary" size="sm" :loading="running" @click="run">运行测量</CoButton>
          <CoButton variant="secondary" size="sm" @click="fill">重新生成</CoButton>
        </div>

        <div v-if="report" class="perf__result">
          <div class="perf__headline">
            <span class="perf__big">{{ report.perFrame.toFixed(2) }}<small>ms/帧</small></span>
            <span class="perf__budget" :class="{ 'is-bad': report.budgetUsed > 60 }">
              预算 {{ report.budgetUsed.toFixed(1) }}%
            </span>
          </div>
          <div class="perf__bar">
            <span
              class="perf__bar-fill"
              :class="{ 'is-warn': report.budgetUsed > 40, 'is-bad': report.budgetUsed > 70 }"
              :style="{ width: `${Math.min(100, report.budgetUsed)}%` }"
            />
          </div>
          <dl class="perf__grid">
            <dt>p50</dt>
            <dd>{{ report.p50.toFixed(2) }}ms</dd>
            <dt>p95</dt>
            <dd>{{ report.p95.toFixed(2) }}ms</dd>
            <dt>p99</dt>
            <dd>{{ report.p99.toFixed(2) }}ms</dd>
            <dt>最长</dt>
            <dd>{{ report.max.toFixed(2) }}ms</dd>
            <dt>理论上限</dt>
            <dd>{{ report.budgetFps.toFixed(0) }} fps</dd>
            <dt>规模</dt>
            <dd>{{ report.shapes }} 笔 / {{ report.points }} 点</dd>
          </dl>
          <code class="perf__code">{{ summary }}</code>
        </div>

        <ul v-if="log.length" class="perf__log">
          <li v-for="(line, i) in log" :key="i">{{ line }}</li>
        </ul>
      </template>

      <p v-else class="perf__denied">性能页仅在开发模式或 <code>?debug=1</code> 下可用。</p>
    </aside>
  </div>
</template>

<style scoped>
.perf {
  position: relative;
  height: 100dvh;
  overflow: hidden;
  background: var(--paper);
}

.perf__panel {
  position: absolute;
  top: 16px;
  right: 16px;
  z-index: var(--z-chrome);
  width: 312px;
  max-height: calc(100% - 32px);
  overflow-y: auto;
  padding: 16px;
  border: 1px solid var(--line);
  border-radius: var(--radius-md);
  background: var(--panel);
  box-shadow: var(--shadow);
}

.perf__head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
  color: var(--brand);
}

.perf__head h1 {
  margin: 0;
  font-size: var(--text-base);
  color: var(--ink);
}

.perf__desc {
  margin: 0 0 14px;
  font-size: var(--text-xs);
  line-height: 1.65;
  color: var(--ink-soft);
}

.perf__row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 8px;
}

.perf__label {
  font-size: var(--text-xs);
  color: var(--ink-soft);
}

.perf__select {
  height: 30px;
  padding: 0 8px;
  border: 1px solid var(--line);
  border-radius: var(--radius-xs);
  background: #fff;
  font-size: var(--text-xs);
  color: var(--ink);
}

.perf__actions {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin: 14px 0 4px;
}

.perf__result {
  margin-top: 14px;
  padding-top: 14px;
  border-top: 1px solid var(--line);
}

.perf__headline {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
}

.perf__big {
  font-family: var(--font-mono);
  font-size: 26px;
  font-weight: 500;
  line-height: 1.1;
  color: var(--ink);
}

.perf__big small {
  margin-left: 4px;
  font-size: 11px;
  font-weight: 400;
  color: var(--ink-soft);
}

.perf__budget {
  font-size: var(--text-xs);
  color: var(--ok);
  font-weight: 500;
}

.perf__budget.is-bad {
  color: var(--warn);
}

.perf__bar {
  height: 4px;
  margin: 8px 0 12px;
  border-radius: 2px;
  background: var(--surface-sunk);
  overflow: hidden;
}

.perf__bar-fill {
  display: block;
  height: 100%;
  border-radius: 2px;
  background: var(--ok);
  transition: width var(--dur) var(--ease);
}

.perf__bar-fill.is-warn {
  background: #f59f00;
}

.perf__bar-fill.is-bad {
  background: var(--warn);
}

.perf__grid {
  display: grid;
  grid-template-columns: auto 1fr;
  gap: 3px 12px;
  margin: 0 0 10px;
  font-size: var(--text-xs);
}

.perf__grid dt {
  color: var(--ink-soft);
}

.perf__grid dd {
  margin: 0;
  font-family: var(--font-mono);
  text-align: right;
  color: var(--ink);
}

.perf__code {
  display: block;
  padding: 8px;
  border-radius: var(--radius-xs);
  background: var(--surface-sunk);
  font-family: var(--font-mono);
  font-size: 10px;
  line-height: 1.65;
  color: var(--ink-soft);
  word-break: break-word;
}

.perf__log {
  margin-top: 12px;
  display: flex;
  flex-direction: column;
  gap: 3px;
  font-family: var(--font-mono);
  font-size: 10px;
  line-height: 1.5;
  color: var(--ink-soft);
}

.perf__log li {
  padding-bottom: 3px;
  border-bottom: 1px dashed var(--line);
}

.perf__denied {
  margin: 0;
  font-size: var(--text-xs);
  color: var(--ink-soft);
}

.perf__denied code {
  font-family: var(--font-mono);
  padding: 1px 4px;
  border-radius: 3px;
  background: var(--surface-sunk);
}
</style>
