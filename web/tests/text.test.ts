import { describe, expect, it } from 'vitest'
import { wrapText, textContentRect } from '@/canvas/text'

/** 等宽假测量：1 字符 = 10px。 */
const mono = (text: string) => [...text].length * 10
const CJK = (text: string) => [...text].length * 16

describe('CJK 换行', () => {
  it('宽度足够时不折行', () => {
    expect(wrapText('你好世界', { maxWidth: 1000, measure: CJK })).toEqual(['你好世界'])
  })

  it('超宽时按字符折行', () => {
    const lines = wrapText('一二三四五六七八九十', { maxWidth: 48, measure: CJK })
    expect(lines.length).toBeGreaterThan(1)
    // 每行最多 3 个全角字符
    for (const line of lines) expect([...line].length).toBeLessThanOrEqual(3)
    expect(lines.join('')).toBe('一二三四五六七八九十')
  })

  it('句号不出现在行首（避头尾）', () => {
    const lines = wrapText('这是第一句话说完了。这是第二句', { maxWidth: 48, measure: CJK })
    for (const line of lines) {
      expect(line.startsWith('。')).toBe(false)
      expect(line.startsWith('，')).toBe(false)
    }
    // 内容不丢不重
    expect(lines.join('')).toBe('这是第一句话说完了。这是第二句')
  })

  it('逗号/问号/右括号都不在行首', () => {
    for (const punct of ['，', '、', '？', '！', '）', '】', '》', '」']) {
      const lines = wrapText(`一二三四${punct}五六七`, { maxWidth: 48, measure: CJK })
      for (const line of lines) expect(line.startsWith(punct)).toBe(false)
    }
  })

  it('左括号不留在行尾', () => {
    const lines = wrapText('一二三（四五六七', { maxWidth: 48, measure: CJK })
    for (const line of lines) expect(line.endsWith('（')).toBe(false)
  })

  it('硬换行被保留', () => {
    expect(wrapText('第一行\n第二行', { maxWidth: 1000, measure: CJK })).toEqual(['第一行', '第二行'])
  })

  it('空段落生成空行（保留原有空行）', () => {
    expect(wrapText('甲\n\n乙', { maxWidth: 1000, measure: CJK })).toEqual(['甲', '', '乙'])
  })

  it('拉丁文不在单词中间断开', () => {
    const lines = wrapText('hello wonderful codraw', { maxWidth: 120, measure: mono })
    // 单词没有被劈开：每一行都是完整的词组合
    const words = lines.join(' ').split(' ').filter(Boolean)
    expect(words).toEqual(['hello', 'wonderful', 'codraw'])
  })

  it('空文本返回单个空行', () => {
    expect(wrapText('', { maxWidth: 100, measure: mono })).toEqual([''])
  })

  it('超长无断点文本仍然折行而不是死循环', () => {
    const lines = wrapText('A'.repeat(50), { maxWidth: 30, measure: mono })
    expect(lines.length).toBeGreaterThan(5)
    expect(lines.join('')).toBe('A'.repeat(50))
  })

  it('maxWidth 极小时不崩', () => {
    const lines = wrapText('测试', { maxWidth: 0, measure: CJK })
    expect(lines).toEqual(['测试'])
  })
})

describe('文本区域', () => {
  it('便签内缩 14/16，普通文本铺满', () => {
    const box = { x: 10, y: 20, w: 260, h: 180 }
    expect(textContentRect(box, 'note')).toEqual({ x: 24, y: 36, w: 232, h: 148 })
    expect(textContentRect(box, 'plain')).toEqual({ x: 10, y: 20, w: 260, h: 180 })
  })
})
