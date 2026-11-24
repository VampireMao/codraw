/**
 * CJK 文本换行（含最小可用的避头尾规则）。
 *
 * 为什么不能直接逐字塞：
 *   中文没有空格，只能按字符断行；但按字符硬断会让「。」「，」出现在行首——
 *   出版物明确禁止，一眼就显廉价。
 *   这里只实现两条规则，但覆盖绝大多数真实中文：
 *     - 行首禁止（收尾符）：悬挂在上一行末尾（允许该行略超宽，最多悬 2 个）
 *     - 行尾禁止（起首符）：推到下一行
 *   再补一条拉丁规则：不在单词中间断开。
 */

/** 不能出现在行首的字符（收尾符）。 */
const NO_START = '。，、；：？！）〕】》」』＂’％‰℃·…—～”'

/** 不能出现在行尾的字符（起首符）。 */
const NO_END = '（〔【《「『＂‘＄￥＃@“'

/** 最多连续悬挂几个标点（「？！」「……」这类需要 2 个） */
const MAX_HANG = 2

const isLatinWordChar = (char: string | undefined): boolean => !!char && /[A-Za-z0-9_'’-]/.test(char)

export interface WrapOptions {
  /** 可用宽度 */
  maxWidth: number
  /** 宽度测量函数 */
  measure: (text: string) => number
}

/**
 * 计算断行位置：返回「上一行应保留的字符数」。
 * 返回 0 表示必须在行首断开（硬断）。
 */
function breakIndex(
  line: string,
  nextChar: string,
  measure: (t: string) => number,
  maxWidth: number,
): number {
  // 1) 起首符不能留在行尾 → 推到下一行
  let keep = line.length
  for (let guard = 0; guard < 4 && keep > 1; guard += 1) {
    const tail = line[keep - 1]
    if (tail && NO_END.includes(tail)) keep -= 1
    else break
  }

  // 2) 拉丁词不劈开：回退到词首。单词本身长过一行时才允许硬断。
  if (isLatinWordChar(line[keep - 1]) && isLatinWordChar(nextChar)) {
    let cut = keep
    while (cut > 0 && isLatinWordChar(line[cut - 1])) cut -= 1
    const prefix = line.slice(0, cut).trimEnd()
    if (prefix !== '') {
      const word = line.slice(cut) + nextChar
      if (measure(word) <= maxWidth) keep = prefix.length
    }
  }

  return keep
}

/**
 * 按可用宽度把文本折成行。保留硬换行；空段落保留为空行。
 */
export function wrapText(text: string, options: WrapOptions): string[] {
  const { maxWidth, measure } = options
  const lines: string[] = []

  for (const paragraph of text.split('\n')) {
    if (paragraph === '') {
      lines.push('')
      continue
    }
    if (maxWidth <= 0) {
      lines.push(paragraph)
      continue
    }

    let line = ''
    for (const char of paragraph) {
      const candidate = line + char
      if (line !== '' && measure(candidate) > maxWidth) {
        // 收尾符悬挂：直接挂在上一行末尾，不换行
        if (NO_START.includes(char)) {
          let hung = 0
          for (let i = line.length - 1; i >= 0 && NO_START.includes(line[i] ?? ''); i -= 1) hung += 1
          if (hung < MAX_HANG) {
            line = candidate
            continue
          }
        }

        const keep = breakIndex(line, char, measure, maxWidth)
        if (keep <= 0) {
          lines.push(line)
          line = char
          continue
        }
        lines.push(line.slice(0, keep).trimEnd())
        line = line.slice(keep).trimStart() + char
        continue
      }
      line = candidate
    }
    lines.push(line)
  }

  return lines
}

/** 便签/文本框的文本可用区域（世界坐标）。 */
export function textContentRect(
  box: { x: number; y: number; w: number; h: number },
  variant: 'note' | 'plain',
): { x: number; y: number; w: number; h: number } {
  if (variant === 'note') {
    return { x: box.x + 14, y: box.y + 16, w: Math.max(1, box.w - 28), h: Math.max(1, box.h - 32) }
  }
  return { x: box.x, y: box.y, w: Math.max(1, box.w), h: Math.max(1, box.h) }
}
