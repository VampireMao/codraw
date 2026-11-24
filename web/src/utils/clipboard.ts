/**
 * 剪贴板复制：唯一实现，RoomView / SharePanel 共用。
 *
 * 为什么不能简单用 `navigator.clipboard.writeText`：
 * 它只在**安全上下文**可用（HTTPS 或 localhost）。通过局域网 IP + HTTP
 * 访问时 `navigator.clipboard` 是 `undefined`，必须退回 `execCommand('copy')`。
 *
 * 兜底的几个细节都不能省（都是踩过的坑）：
 *  - textarea 必须在视口内（`fixed` + `top/left: 0`）。放到视口外或 `display:none`
 *    时浏览器不会真的选中内容，`execCommand` 返回 true 但剪贴板是空的。
 *  - 必须显式 `focus()` + `setSelectionRange(0, len)`，单靠 `select()` 在部分内核上不足。
 *  - 用完要还原用户原有选区，否则会打乱焦点位置。
 */

export type CopyResult = 'ok' | 'failed'

export async function copyToClipboard(text: string): Promise<CopyResult> {
  if (!text) return 'failed'

  // 路径 1：异步剪贴板 API（安全上下文，最可靠）
  if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      // writeText 成功即视为成功：读回校验会在无头浏览器、焦点切换、
      // 剪贴板管理器介入时读到旧值，产生假失败。
      return 'ok'
    } catch {
      // 权限被拒 / 文档非焦点：继续走兜底
    }
  }

  // 路径 2：execCommand 兜底（非安全上下文唯一可行方案）
  if (typeof document === 'undefined') return 'failed'

  const area = document.createElement('textarea')
  area.value = text
  area.setAttribute('readonly', '')
  area.style.cssText =
    'position:fixed;top:0;left:0;width:1px;height:1px;padding:0;margin:0;border:0;' +
    'opacity:0;font-size:12pt;'
  document.body.appendChild(area)

  const selection = document.getSelection()
  const previousRange = selection && selection.rangeCount > 0 ? selection.getRangeAt(0) : null

  let executed = false
  try {
    area.focus()
    area.select()
    area.setSelectionRange(0, text.length)
    executed = document.execCommand('copy')
  } catch {
    executed = false
  } finally {
    area.remove()
    if (previousRange && selection) {
      selection.removeAllRanges()
      selection.addRange(previousRange)
    }
  }

  return executed ? 'ok' : 'failed'
}
