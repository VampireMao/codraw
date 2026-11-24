<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { CoButton, CoIcon, pushToast } from '@/components/ui'
import { isValidRoomCode, normalizeRoomCode, roomPath } from '@/net/roomCode'
import { createRoom } from '@/net/rest'
import { readName, writeName, writeRoomKey } from '@/stores/storage'
import { useBoardStore } from '@/stores/board'

const router = useRouter()
const store = useBoardStore()

const codeInput = ref('')
const creating = ref(false)
const name = ref(readName())

const normalized = computed(() => normalizeRoomCode(codeInput.value))
const codeValid = computed(() => isValidRoomCode(normalized.value))

/**
 * 新建白板（UX-SPEC §7.1：点击立刻进入房间，不再二次确认）。
 *
 * 真实路径：`POST /api/rooms` → 服务端签发房间码 + 编辑密钥 → 直接进房间。
 * 编辑密钥只在创建响应出现一次，所以**先落 localStorage 再跳转**，
 * 否则刷新一次就永远丢失编辑权限。
 */
async function createBoard(): Promise<void> {
  if (creating.value) return
  // 先写昵称（建房间后立即进入，没机会再填）
  writeName(name.value)
  creating.value = true
  try {
    const result = await createRoom('未命名白板')
    writeRoomKey(result.room.code, result.keys.editorKey)
    void router.push({ name: 'room', params: { code: result.room.code } })
  } catch (error) {
    creating.value = false
    const message = error instanceof Error && error.message ? error.message : '网络异常'
    pushToast(`建房失败：${message}`, 'warn', 3600)
  }
}

function join(): void {
  if (!codeValid.value) {
    pushToast('请输入 6 位房间码', 'warn')
    return
  }
  writeName(name.value)
  void router.push(roomPath(normalized.value))
}

function onCodeInput(event: Event): void {
  const target = event.target
  if (!(target instanceof HTMLInputElement)) return
  // 自动转大写并限制 8 字符（含连字符）
  codeInput.value = target.value.toUpperCase().slice(0, 8)
}

function saveName(): void {
  writeName(name.value)
}

onMounted(() => {
  store.setTool('select')
})
</script>

<template>
  <div class="home">
    <header class="home__nav">
      <div class="home__brand">
        <span class="home__logo" aria-hidden="true">
          <svg width="22" height="22" viewBox="0 0 24 24" fill="none" aria-hidden="true">
            <path
              d="M4.5 17.5c2.4-1 3.4-3.2 4.6-5.6 1-2 2-3.6 3.4-3.6 1.5 0 2 1.4 2.8 3.2.9 2 2 4.2 4.2 5.2"
              stroke="currentColor"
              stroke-width="1.8"
              stroke-linecap="round"
            />
            <circle cx="5" cy="18" r="1.6" fill="currentColor" />
            <circle cx="19.4" cy="17.4" r="1.6" fill="currentColor" />
          </svg>
        </span>
        <span class="home__wordmark">同绘 <span class="home__wordmark-en">CoDraw</span></span>
      </div>
      <span class="home__badge">多人实时白板 · 无需注册</span>
    </header>

    <main class="home__main">
      <section class="hero">
        <h1 class="hero__title">打开链接，<br class="hero__br" />一起把想法画出来</h1>
        <p class="hero__lede">
          同绘是一块无限大的纸。建一个房间，把链接发给人，<br class="hero__br" />
          手机和电脑就能同时落笔——断网也不丢。
        </p>

        <div class="hero__actions">
          <CoButton variant="primary" size="lg" :loading="creating" @click="createBoard">
            新建白板
            <template #icon>
              <CoIcon name="plus" :size="18" />
            </template>
          </CoButton>

          <form class="join" @submit.prevent="join">
            <label class="visually-hidden" for="room-code">房间码</label>
            <input
              id="room-code"
              class="join__input mono"
              type="text"
              inputmode="text"
              autocomplete="off"
              autocapitalize="characters"
              spellcheck="false"
              :placeholder="'ABC-DEF'"
              :value="codeInput"
              maxlength="8"
              @input="onCodeInput"
              @keydown.enter.prevent="join"
            />
            <CoButton variant="secondary" size="lg" type="submit" :disabled="!codeValid">加入</CoButton>
          </form>
        </div>

        <p class="hero__fineprint">
          没有账号，没有安装。房间码区分大小写无妨——<span class="mono">ABC-DEF</span> 与
          <span class="mono">abcdef</span> 是同一间。
        </p>
      </section>

      <section class="features" aria-label="核心特性">
        <article class="feature">
          <span class="feature__icon" aria-hidden="true">
            <CoIcon name="users" :size="20" />
          </span>
          <h2 class="feature__title">实时协作</h2>
          <p class="feature__text">谁在画、画到哪儿，一眼看见。光标带名字，不用喊「你看到我画的了吗」。</p>
        </article>

        <article class="feature">
          <span class="feature__icon" aria-hidden="true">
            <CoIcon name="wifi-off" :size="20" />
          </span>
          <h2 class="feature__title">断网续画</h2>
          <p class="feature__text">地铁里也能接着画。网络回来，笔迹按顺序补齐，一条不丢、一条不重。</p>
        </article>

        <article class="feature">
          <span class="feature__icon" aria-hidden="true">
            <CoIcon name="eye" :size="20" />
          </span>
          <h2 class="feature__title">免注册分享</h2>
          <p class="feature__text">一条只读链接，给谁都能看。想动手？把编辑链接单独发给他。</p>
        </article>
      </section>

      <section class="name-hint" aria-label="昵称设置">
        <span class="name-hint__label">别人会看到你的名字</span>
        <input
          v-model="name"
          class="name-hint__input"
          type="text"
          maxlength="20"
          placeholder="给自己起个昵称（可留空）"
          @blur="saveName"
          @keydown.enter="saveName"
        />
      </section>
    </main>

    <footer class="home__footer">
      <span>同绘 CoDraw · 纸感极简的协作白板</span>
      <span class="home__footer-sep" aria-hidden="true">·</span>
      <span class="mono">v1.0</span>
    </footer>
  </div>
</template>

<style scoped>
.home {
  display: flex;
  flex-direction: column;
  min-height: 100%;
  background: var(--paper);
}

/* ---------- 导航 ---------- */
.home__nav {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-4);
  padding: 22px clamp(20px, 5vw, 56px);
}

.home__brand {
  display: inline-flex;
  align-items: center;
  gap: 9px;
  color: var(--ink);
}

.home__logo {
  display: grid;
  place-items: center;
  width: 30px;
  height: 30px;
  border-radius: 9px;
  background: var(--ink);
  color: var(--paper);
}

.home__wordmark {
  font-size: var(--text-md);
  font-weight: 600;
  letter-spacing: 0.02em;
}

.home__wordmark-en {
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  font-weight: 400;
  letter-spacing: 0.08em;
  color: var(--ink-soft);
  margin-left: 2px;
}

.home__badge {
  font-size: var(--text-xs);
  color: var(--ink-soft);
  letter-spacing: 0.02em;
}

/* ---------- 主体 ---------- */
.home__main {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: clamp(48px, 8vh, 88px);
  width: 100%;
  max-width: 940px;
  margin: 0 auto;
  padding: clamp(32px, 7vh, 88px) clamp(20px, 5vw, 56px) 64px;
}

/* ---------- Hero ---------- */
.hero {
  max-width: 660px;
}

.hero__title {
  margin: 0;
  font-size: clamp(34px, 5.4vw, 56px);
  font-weight: 650;
  line-height: 1.22;
  letter-spacing: -0.025em;
  color: var(--ink);
}

.hero__lede {
  margin: 22px 0 0;
  max-width: 46em;
  font-size: var(--text-md);
  line-height: 1.7;
  color: var(--ink-soft);
}

.hero__actions {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin-top: 34px;
  flex-wrap: wrap;
}

.hero__fineprint {
  margin: 18px 0 0;
  font-size: var(--text-xs);
  line-height: 1.7;
  color: var(--ink-soft);
  opacity: 0.72;
}

/* ---------- 加入房间 ---------- */
.join {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: 4px;
  border: 1px solid var(--line);
  border-radius: var(--radius-md);
  background: var(--panel);
  box-shadow: var(--shadow-sm);
  transition:
    border-color var(--dur) var(--ease),
    box-shadow var(--dur) var(--ease);
}

.join:focus-within {
  border-color: #c5c2b7;
  box-shadow: 0 2px 10px rgba(20, 22, 28, 0.07);
}

.join__input {
  width: 128px;
  height: 40px;
  padding: 0 8px 0 12px;
  border: 0;
  background: transparent;
  font-size: var(--text-base);
  letter-spacing: 0.18em;
  color: var(--ink);
  outline: none;
  text-transform: uppercase;
}

.join__input::placeholder {
  color: var(--ink-soft);
  opacity: 0.45;
  letter-spacing: 0.18em;
}

/* ---------- 特性 ---------- */
.features {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: clamp(20px, 3vw, 36px);
  padding-top: 8px;
  border-top: 1px solid var(--line);
}

.feature {
  padding-top: 24px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.feature__icon {
  display: grid;
  place-items: center;
  width: 34px;
  height: 34px;
  margin-bottom: 8px;
  border-radius: var(--radius-sm);
  background: var(--brand-weak);
  color: var(--brand);
}

.feature__title {
  margin: 0;
  font-size: var(--text-base);
  font-weight: 600;
  letter-spacing: 0.01em;
}

.feature__text {
  margin: 0;
  font-size: var(--text-sm);
  line-height: 1.75;
  color: var(--ink-soft);
}

/* ---------- 昵称 ---------- */
.name-hint {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  max-width: 440px;
  padding-top: 8px;
  border-top: 1px dashed var(--line);
}

.name-hint__label {
  flex: none;
  font-size: var(--text-xs);
  color: var(--ink-soft);
}

.name-hint__input {
  flex: 1;
  min-width: 0;
  height: 36px;
  padding: 0 10px;
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  background: var(--surface-sunk);
  font-size: var(--text-sm);
  color: var(--ink);
  outline: none;
  transition:
    border-color var(--dur-fast) var(--ease),
    background-color var(--dur-fast) var(--ease);
}

.name-hint__input:hover {
  background: #ebe9e2;
}

.name-hint__input:focus {
  border-color: var(--brand);
  background: var(--panel);
}

/* ---------- 页脚 ---------- */
.home__footer {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 20px clamp(20px, 5vw, 56px) 28px;
  font-size: var(--text-xs);
  color: var(--ink-soft);
  opacity: 0.62;
}

.home__footer-sep {
  opacity: 0.5;
}

/* ---------- 响应式 ---------- */
@media (max-width: 767px) {
  .home__badge {
    display: none;
  }

  .hero__br {
    display: none;
  }

  .hero__title {
    font-size: clamp(28px, 8.2vw, 40px);
    line-height: 1.28;
  }

  .hero__lede {
    font-size: var(--text-base);
    margin-top: 16px;
  }

  .hero__actions {
    flex-direction: column;
    align-items: stretch;
    gap: var(--space-2);
    margin-top: 26px;
  }

  .hero__actions > :first-child {
    width: 100%;
  }

  .join {
    justify-content: space-between;
  }

  .join__input {
    flex: 1;
    width: auto;
  }

  .features {
    grid-template-columns: 1fr;
    gap: 0;
  }

  .feature {
    padding: 20px 0;
    border-bottom: 1px solid var(--line);
    display: grid;
    grid-template-columns: 34px 1fr;
    grid-template-areas:
      'icon title'
      'icon text';
    column-gap: var(--space-3);
    align-items: start;
  }

  .feature:last-child {
    border-bottom: 0;
  }

  .feature__icon {
    grid-area: icon;
    margin: 1px 0 0;
  }

  .feature__title {
    grid-area: title;
  }

  .feature__text {
    grid-area: text;
  }

  .name-hint {
    flex-direction: column;
    align-items: stretch;
    gap: 8px;
    max-width: none;
    padding-top: 16px;
  }

  .name-hint__input {
    height: 44px;
  }

  .home__main {
    gap: 40px;
    padding-top: 24px;
  }
}
</style>
