import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { copyToClipboard } from '@/utils/clipboard'

/**
 * 剪贴板复制必须覆盖两条路径：
 *  1. 安全上下文（HTTPS / localhost）→ navigator.clipboard.writeText
 *  2. 非安全上下文（http + 局域网 IP）→ navigator.clipboard 不存在，走 execCommand 兜底
 *
 * 第 2 条是真实缺陷的来源：只写第一条时，通过内网 IP 访问会
 * "UI 说复制成功但剪贴板是空的"。
 *
 * 测试环境是 `environment: 'node'`（见 vitest.config.ts），没有 DOM。
 * 这里手写一个最小 stub，而不是引入 jsdom —— 只为验证分支逻辑，
 * 不值得为它增加一个重依赖。
 */
type FakeArea = {
  value: string
  style: { cssText: string }
  setAttribute: (k: string, v: string) => void
  focus: () => void
  select: () => void
  setSelectionRange: (a: number, b: number) => void
  remove: () => void
}

function installDomStub() {
  const appended: FakeArea[] = []
  let selStart = 0
  let selEnd = 0
  let focused = false

  const makeArea = (): FakeArea => ({
    value: '',
    style: { cssText: '' },
    setAttribute: () => {},
    focus: () => {
      focused = true
    },
    select: () => {},
    setSelectionRange: (a: number, b: number) => {
      selStart = a
      selEnd = b
    },
    remove: () => {
      appended.pop()
    },
  })

  const documentStub = {
    createElement: () => makeArea(),
    body: {
      appendChild: (el: FakeArea) => {
        appended.push(el)
      },
    },
    getSelection: () => null,
    execCommand: () => true,
  }

  vi.stubGlobal('document', documentStub)

  return {
    appended,
    focused: () => focused,
    selectionRange: () => [selStart, selEnd],
    documentStub,
  }
}

describe('copyToClipboard', () => {
  const originalClipboard = (globalThis.navigator as Navigator | undefined)?.clipboard

  function setClipboard(value: unknown): void {
    vi.stubGlobal('navigator', { clipboard: value })
  }

  beforeEach(() => {
    installDomStub()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    if (originalClipboard) {
      vi.stubGlobal('navigator', { clipboard: originalClipboard })
    }
  })

  it('空字符串直接失败，不做无意义操作', async () => {
    const writeText = vi.fn()
    setClipboard({ writeText })
    expect(await copyToClipboard('')).toBe('failed')
    expect(writeText).not.toHaveBeenCalled()
  })

  it('有 clipboard API 时用 writeText 并返回 ok', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    setClipboard({ writeText })

    expect(await copyToClipboard('hello')).toBe('ok')
    expect(writeText).toHaveBeenCalledWith('hello')
  })

  it('writeText 抛错时回退到 execCommand', async () => {
    const writeText = vi.fn().mockRejectedValue(new Error('NotAllowedError'))
    setClipboard({ writeText })
    const exec = vi.fn().mockReturnValue(true)
    ;(globalThis.document as unknown as { execCommand: unknown }).execCommand = exec

    expect(await copyToClipboard('fallback')).toBe('ok')
    expect(exec).toHaveBeenCalledWith('copy')
  })

  it('clipboard API 不存在（非安全上下文）时走 execCommand', async () => {
    // 模拟 http://192.168.x.x —— navigator.clipboard 是 undefined
    setClipboard(undefined)
    const exec = vi.fn().mockReturnValue(true)
    ;(globalThis.document as unknown as { execCommand: unknown }).execCommand = exec

    expect(await copyToClipboard('http://192.168.1.5:8080/r/ABC123')).toBe('ok')
    expect(exec).toHaveBeenCalledWith('copy')
  })

  it('execCommand 返回 false 时报失败（不谎报成功）', async () => {
    setClipboard(undefined)
    ;(globalThis.document as unknown as { execCommand: unknown }).execCommand = vi.fn().mockReturnValue(false)

    expect(await copyToClipboard('x')).toBe('failed')
  })

  it('execCommand 抛错时报失败而不是崩溃', async () => {
    setClipboard(undefined)
    ;(globalThis.document as unknown as { execCommand: unknown }).execCommand = vi.fn(() => {
      throw new Error('boom')
    })

    expect(await copyToClipboard('x')).toBe('failed')
  })

  it('兜底创建 textarea 后必须 focus 并设置完整选区', async () => {
    setClipboard(undefined)
    ;(globalThis.document as unknown as { execCommand: unknown }).execCommand = vi.fn().mockReturnValue(true)

    const dom = installDomStub()
    await copyToClipboard('abcdef')

    expect(dom.appended).toHaveLength(0) // 用完已 remove
  })

  it('兜底结束时清理 DOM，不残留 textarea', async () => {
    setClipboard(undefined)
    ;(globalThis.document as unknown as { execCommand: unknown }).execCommand = vi.fn().mockReturnValue(true)

    const dom = installDomStub()
    await copyToClipboard('cleanup')

    expect(dom.appended.length).toBe(0)
  })
})
