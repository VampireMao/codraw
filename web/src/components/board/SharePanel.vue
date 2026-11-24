<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import QRCode from 'qrcode'
import { CoButton, CoIcon, CoModal, pushToast } from '@/components/ui'
import { formatRoomCode } from '@/net/roomCode'
import { issueReadonlyToken, revokeReadonlyToken, ApiError } from '@/net/rest'
import { errorText } from '@/net/types'
import { copyToClipboard } from '@/utils/clipboard'

const props = defineProps<{
  open: boolean
  code: string
  /** 编辑链接（含 editorKey） */
  editUrl: string
  /** 不含 token 的只读链接兜底地址 */
  viewUrl: string
  canShareEdit: boolean
  /** 编辑密钥；为空则无法签发只读 token（服务端 403） */
  editorKey: string
  /** 房间还没 joined 时禁用只读 token 操作 */
  roomLimited: boolean
}>()

const emit = defineEmits<{ close: [] }>()

type Tab = 'edit' | 'view'

const mode = ref<Tab>('edit')
const qrDataUrl = ref('')
const qrError = ref(false)
const copied = ref<Tab | 'code' | null>(null)
const issuing = ref(false)
const revoking = ref(false)

/** 服务端签发的只读链接（含 `?rt=`），优先展示它 */
const readonlyUrl = ref('')
const readonlyExpiresAt = ref(0)
const readonlyError = ref('')

const shownCode = computed(() => formatRoomCode(props.code))

const activeUrl = computed(() => {
  if (mode.value === 'view' && readonlyUrl.value) return readonlyUrl.value
  if (mode.value === 'edit' && props.canShareEdit) return props.editUrl
  return props.viewUrl
})

const expiresLabel = computed(() => {
  if (!readonlyExpiresAt.value) return ''
  const date = new Date(readonlyExpiresAt.value)
  const pad = (n: number): string => String(n).padStart(2, '0')
  return `${date.getMonth() + 1}月${date.getDate()}日 ${pad(date.getHours())}:${pad(date.getMinutes())} 失效`
})

let copyTimer = 0
onBeforeUnmount(() => {
  window.clearTimeout(copyTimer)
})

/* --------------------------------- 二维码 -------------------------------- */

async function renderQr(): Promise<void> {
  const url = activeUrl.value
  if (!url) {
    qrDataUrl.value = ''
    return
  }
  try {
    // 客户端生成：链接含 key/token，绝不发给第三方
    qrDataUrl.value = await QRCode.toDataURL(url, {
      errorCorrectionLevel: 'M',
      margin: 0,
      width: 320,
      color: { dark: '#16181D', light: '#FFFFFF' },
    })
    qrError.value = false
  } catch {
    qrError.value = true
    qrDataUrl.value = ''
  }
}

watch(
  () => [props.open, activeUrl.value] as const,
  () => {
    if (props.open) void renderQr()
  },
  { immediate: true },
)

watch(
  () => props.open,
  (open) => {
    if (!open) return
    if (props.canShareEdit) mode.value = 'edit'
    else mode.value = 'view'
  },
)

/* ------------------------------- 只读 token ------------------------------ */

/**
 * 首次切到「只读」tab 时向服务端申请 token（协议 §1.3）。
 * 幂等：已有未过期的 token 就不重复申请。
 */
async function ensureReadonlyToken(force = false): Promise<void> {
  if (!props.canShareEdit || !props.editorKey || issuing.value) return
  if (!force && readonlyUrl.value && readonlyExpiresAt.value > Date.now() + 60_000) return

  issuing.value = true
  readonlyError.value = ''
  try {
    const result = await issueReadonlyToken(props.code, props.editorKey, 86400)
    readonlyUrl.value = result.url
    readonlyExpiresAt.value = result.expiresAt
  } catch (error) {
    readonlyError.value = error instanceof ApiError ? errorText(error.code) : '只读链接生成失败，请稍后重试'
    readonlyUrl.value = ''
  } finally {
    issuing.value = false
  }
}

watch(mode, (next) => {
  if (next === 'view') void ensureReadonlyToken()
})

/** 撤销：让所有已签发的只读链接立刻失效（协议 §1.4）。 */
async function revoke(): Promise<void> {
  if (!props.editorKey || revoking.value) return
  revoking.value = true
  try {
    await revokeReadonlyToken(props.code, props.editorKey, true)
    readonlyUrl.value = ''
    readonlyExpiresAt.value = 0
    pushToast('已撤销全部只读链接', 'ok')
  } catch (error) {
    const message = error instanceof ApiError ? errorText(error.code) : '撤销失败，请稍后重试'
    pushToast(message, 'warn')
  } finally {
    revoking.value = false
  }
}

/* --------------------------------- 复制 --------------------------------- */

async function copy(url: string, which: Tab | 'code'): Promise<void> {
  if (!url) return
  const result = await copyToClipboard(url)
  if (result !== 'ok') {
    pushToast('复制失败，请手动选中链接', 'warn')
    return
  }
  copied.value = which
  pushToast('链接已复制', 'ok')
  window.clearTimeout(copyTimer)
  copyTimer = window.setTimeout(() => {
    copied.value = null
  }, 1800)
}

function downloadQr(): void {
  if (!qrDataUrl.value) return
  const link = document.createElement('a')
  link.href = qrDataUrl.value
  link.download = `codraw-${props.code}-${mode.value}.png`
  link.click()
}
</script>

<template>
  <CoModal
    :open="props.open"
    title="邀请协作"
    :description="'房间 ' + shownCode + ' · 扫码或用链接加入'"
    :width="440"
    @close="emit('close')"
  >
    <div v-if="props.canShareEdit" class="share__tabs" role="tablist" aria-label="链接类型">
      <button
        type="button"
        role="tab"
        class="share__tab"
        :class="{ 'is-active': mode === 'edit' }"
        :aria-selected="mode === 'edit'"
        @click="mode = 'edit'"
      >
        可编辑
      </button>
      <button
        type="button"
        role="tab"
        class="share__tab"
        :class="{ 'is-active': mode === 'view' }"
        :aria-selected="mode === 'view'"
        @click="mode = 'view'"
      >
        只读
      </button>
    </div>

    <div class="share__qr">
      <img
        v-if="qrDataUrl"
        :src="qrDataUrl"
        class="share__qr-img"
        width="180"
        height="180"
        :alt="'房间 ' + shownCode + ' 的二维码'"
      />
      <div v-else-if="issuing" class="share__qr-fallback">
        <CoIcon name="spinner" :size="24" class="spin" />
        <span>正在生成只读链接…</span>
      </div>
      <div v-else-if="qrError" class="share__qr-fallback">
        <CoIcon name="qr" :size="28" />
        <span>二维码生成失败，可直接复制链接</span>
      </div>
      <div v-else class="share__qr-fallback">
        <CoIcon name="qr" :size="28" />
        <span>{{ readonlyError || '暂无可用链接' }}</span>
      </div>
    </div>

    <p class="share__hint">
      <template v-if="mode === 'edit' && props.canShareEdit">
        拿到这个链接的人可以绘制。请只发给可信的人。
      </template>
      <template v-else>
        只读链接：可缩放浏览，不能修改。{{ expiresLabel ? `（${expiresLabel}）` : '' }}
      </template>
    </p>

    <p v-if="readonlyError" class="share__error">{{ readonlyError }}</p>

    <div class="share__field">
      <input
        class="share__input mono"
        :value="activeUrl"
        readonly
        aria-label="分享链接"
        @focus="($event.target as HTMLInputElement).select()"
      />
      <CoButton variant="secondary" size="sm" icon-only aria-label="复制链接" @click="copy(activeUrl, mode)">
        <CoIcon :name="copied === mode ? 'check' : 'copy'" :size="16" />
      </CoButton>
    </div>

    <!-- 只读 token 操作 -->
    <div v-if="props.canShareEdit && mode === 'view'" class="share__token">
      <CoButton variant="quiet" size="sm" :loading="issuing" @click="ensureReadonlyToken(true)">
        重新生成
      </CoButton>
      <CoButton variant="quiet" size="sm" :loading="revoking" @click="revoke">撤销所有只读链接</CoButton>
    </div>

    <p v-if="!props.editorKey && props.canShareEdit" class="share__note">
      当前浏览器没有编辑密钥，无法签发可撤销的只读链接。用创建房间时保存的编辑链接重新打开即可。
    </p>

    <details class="share__code-box">
      <summary class="share__summary">房间码</summary>
      <div class="share__code-row">
        <span class="share__code mono">{{ shownCode }}</span>
        <CoButton variant="quiet" size="sm" @click="copy(shownCode, 'code')">复制</CoButton>
      </div>
    </details>

    <template #footer>
      <CoButton v-if="qrDataUrl" variant="quiet" size="sm" @click="downloadQr">下载二维码</CoButton>
      <CoButton variant="primary" @click="emit('close')">完成</CoButton>
    </template>
  </CoModal>
</template>

<style scoped>
.share__tabs {
  display: inline-flex;
  gap: 2px;
  margin-bottom: var(--space-4);
  padding: 3px;
  border-radius: var(--radius-sm);
  background: var(--surface-sunk);
}

.share__tab {
  min-height: 32px;
  padding: 0 14px;
  border: 0;
  border-radius: 6px;
  background: transparent;
  color: var(--ink-soft);
  font-size: var(--text-sm);
  font-weight: 500;
  cursor: pointer;
  transition:
    background-color var(--dur-fast) var(--ease),
    color var(--dur-fast) var(--ease);
}

.share__tab.is-active {
  background: var(--panel);
  color: var(--ink);
  box-shadow: var(--shadow-sm);
}

.share__qr {
  display: grid;
  place-items: center;
  width: 208px;
  height: 208px;
  margin: 0 auto;
  padding: 14px;
  border: 1px solid var(--line);
  border-radius: var(--radius-md);
  background: #fff;
}

.share__qr-img {
  width: 180px;
  height: 180px;
  image-rendering: pixelated;
}

.share__qr-fallback {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  padding: 0 16px;
  font-size: var(--text-xs);
  color: var(--ink-soft);
  text-align: center;
}

.share__hint {
  margin: var(--space-3) 0 var(--space-3);
  font-size: var(--text-xs);
  line-height: 1.6;
  color: var(--ink-soft);
  text-align: center;
}

.share__error {
  margin: 0 0 var(--space-2);
  font-size: var(--text-xs);
  line-height: 1.6;
  color: var(--warn);
  text-align: center;
}

.share__field {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.share__input {
  flex: 1;
  min-width: 0;
  height: 38px;
  padding: 0 10px;
  border: 1px solid var(--line);
  border-radius: var(--radius-sm);
  background: #fcfcfa;
  color: var(--ink-soft);
  font-size: var(--text-xs);
  text-overflow: ellipsis;
  outline: none;
}

.share__input:focus {
  border-color: var(--brand);
  background: #fff;
}

.share__token {
  display: flex;
  justify-content: center;
  gap: var(--space-2);
  margin-top: var(--space-2);
}

.share__note {
  margin: var(--space-3) 0 0;
  padding: 8px 10px;
  border-radius: var(--radius-xs);
  background: var(--surface-sunk);
  font-size: var(--text-2xs);
  line-height: 1.6;
  color: var(--ink-soft);
}

.share__code-box {
  margin-top: var(--space-4);
  border-top: 1px solid var(--line);
  padding-top: var(--space-3);
}

.share__summary {
  font-size: var(--text-xs);
  color: var(--ink-soft);
  cursor: pointer;
  user-select: none;
}

.share__code-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: 8px;
}

.share__code {
  font-size: var(--text-md);
  font-weight: 500;
  letter-spacing: 0.18em;
}

.spin {
  animation: co-spin 0.9s linear infinite;
}

@keyframes co-spin {
  to {
    transform: rotate(360deg);
  }
}
</style>
