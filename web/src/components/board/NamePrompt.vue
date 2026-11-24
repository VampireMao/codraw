<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { CoButton, CoModal } from '@/components/ui'

/**
 * NamePrompt：让用户设置/修改昵称。
 *
 * 两种用法：
 *  1. 首次进入（分享链接）时自动弹出，因为默认昵称是「访客-xxxx」，别人认不出是谁；
 *  2. 顶栏点头像随时改名。
 *
 * 改名需要重新握手（协议没有 rename 消息），由调用方在 confirm 后重建会话。
 */
const props = withDefaults(
  defineProps<{
    open: boolean
    /** 当前昵称，用于预填与判断是否是自动生成的访客名 */
    current: string
    /** true = 首次进入的强制引导，不能直接关掉 */
    firstTime?: boolean
  }>(),
  { firstTime: false },
)

const emit = defineEmits<{
  confirm: [name: string]
  /** 选择「先这样，稍后再说」：仅首次引导会出现 */
  skip: []
  close: []
}>()

const MAX_LEN = 20
const value = ref('')
const inputEl = ref<HTMLInputElement | null>(null)
const error = ref('')

/** 自动生成的访客名（服务端也会这么拼），不应该预填进输入框。 */
const isGuestName = computed(() => /^访客-/.test(props.current))

watch(
  () => props.open,
  async (open) => {
    if (!open) return
    value.value = isGuestName.value ? '' : props.current
    error.value = ''
    await nextTick()
    inputEl.value?.focus()
    inputEl.value?.select()
  },
  { immediate: true },
)

function submit(): void {
  const name = value.value.trim()
  if (name.length === 0) {
    error.value = '请填写昵称，方便其他人认出你'
    inputEl.value?.focus()
    return
  }
  if (name.length > MAX_LEN) {
    error.value = `昵称最多 ${MAX_LEN} 个字符`
    return
  }
  error.value = ''
  emit('confirm', name)
}

function skip(): void {
  emit('skip')
}
</script>

<template>
  <CoModal
    :open="open"
    :title="firstTime ? '先给自己起个名字' : '修改昵称'"
    :description="
      firstTime
        ? '你的笔迹和光标会显示这个名字，其他人才能认出是你。'
        : '改名后会以新名字重新加入房间，画布内容不受影响。'
    "
    :dismissable="!firstTime"
    :width="400"
    @close="firstTime ? undefined : emit('close')"
  >
    <form class="name-prompt" @submit.prevent="submit">
      <label class="name-prompt__label" for="codraw-name">昵称</label>
      <input
        id="codraw-name"
        ref="inputEl"
        v-model="value"
        class="name-prompt__input"
        type="text"
        :maxlength="MAX_LEN"
        placeholder="例如：小林"
        autocomplete="nickname"
        enterkeyhint="done"
        @input="error = ''"
      />
      <p v-if="error" class="name-prompt__error" role="alert">{{ error }}</p>
      <p v-else class="name-prompt__hint">最多 {{ MAX_LEN }} 个字符，支持中文。</p>

      <div class="name-prompt__actions">
        <CoButton v-if="firstTime" variant="ghost" type="button" @click="skip">稍后再说</CoButton>
        <CoButton variant="primary" type="submit">{{ firstTime ? '进入白板' : '保存' }}</CoButton>
      </div>
    </form>
  </CoModal>
</template>

<style scoped>
.name-prompt {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 4px 0 0;
}

.name-prompt__label {
  font-size: var(--text-sm);
  color: var(--ink-soft);
}

.name-prompt__input {
  width: 100%;
  box-sizing: border-box;
  height: 44px;
  padding: 0 12px;
  border: 1px solid var(--line);
  border-radius: var(--radius-sm);
  background: var(--paper);
  color: var(--ink);
  font: inherit;
  font-size: var(--text-base);
  transition:
    border-color var(--dur-fast) var(--ease),
    box-shadow var(--dur-fast) var(--ease);
}

.name-prompt__input:focus {
  outline: none;
  border-color: var(--brand);
  box-shadow: 0 0 0 3px var(--brand-weak);
}

.name-prompt__hint,
.name-prompt__error {
  margin: 0;
  font-size: var(--text-xs);
  line-height: 1.5;
}

.name-prompt__hint {
  color: var(--ink-soft);
}

.name-prompt__error {
  color: var(--warn);
}

.name-prompt__actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  margin-top: 8px;
}
</style>
