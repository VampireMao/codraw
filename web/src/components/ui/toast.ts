/**
 * Toast：单例队列，挂载一次即可在任意位置 `useToast().push()`。
 * 位置固定底部居中（移动端抬到工具条上方），不遮挡画布交互。
 */
import { defineComponent, h, ref, Teleport, TransitionGroup } from 'vue'

export type ToastTone = 'info' | 'ok' | 'warn'

export interface ToastItem {
  id: number
  message: string
  tone: ToastTone
}

const items = ref<ToastItem[]>([])
let seq = 0
const timers = new Map<number, number>()

export function pushToast(message: string, tone: ToastTone = 'info', duration = 2400): void {
  if (!message) return
  seq += 1
  const id = seq
  // 同文案去重：连续点复制不该叠 5 条
  const existing = items.value.find((item) => item.message === message)
  if (existing) {
    window.clearTimeout(timers.get(existing.id) ?? 0)
    timers.set(
      existing.id,
      window.setTimeout(() => dismissToast(existing.id), duration),
    )
    return
  }
  items.value = [...items.value.slice(-2), { id, message, tone }]
  timers.set(
    id,
    window.setTimeout(() => dismissToast(id), duration),
  )
}

export function dismissToast(id: number): void {
  items.value = items.value.filter((item) => item.id !== id)
  const timer = timers.get(id)
  if (timer) window.clearTimeout(timer)
  timers.delete(id)
}

/** 供测试/演示手动清空 */
export function clearToasts(): void {
  for (const timer of timers.values()) window.clearTimeout(timer)
  timers.clear()
  items.value = []
}

export const CoToastHost = defineComponent({
  name: 'CoToastHost',
  setup() {
    return () =>
      h(Teleport, { to: 'body' }, [
        h(
          'div',
          { class: 'co-toast-host', role: 'status', 'aria-live': 'polite' },
          h(TransitionGroup, { name: 'co-toast' }, () =>
            items.value.map((item) =>
              h(
                'div',
                {
                  key: item.id,
                  class: ['co-toast', `co-toast--${item.tone}`],
                  onClick: () => dismissToast(item.id),
                },
                [
                  h('span', { class: 'co-toast__dot', 'aria-hidden': 'true' }),
                  h('span', { class: 'co-toast__text' }, item.message),
                ],
              ),
            ),
          ),
        ),
      ])
  },
})
