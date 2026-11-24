/**
 * 房间码工具。契约来源：docs/PROTOCOL.md §0（冻结）。
 *
 * 字符集 **31 个**：`23456789ABCDEFGHJKMNPQRSTUVWXYZ`
 * 即排除易混淆的 `0 O 1 I L`（注意：旧 API.md 声称 32 个但实际含 L，已按
 * docs/CONTRACT-ALIGNMENT.md §2.1 第 3 项硬修正）。
 *
 * 展示分组 `ABC-DEF`，存储与 URL 不带连字符；入参大小写不敏感。
 */

export const ROOM_CODE_ALPHABET = '23456789ABCDEFGHJKMNPQRSTUVWXYZ'
export const ROOM_CODE_LENGTH = 6
/** 31 字符集，6 位（等价于服务端 DDL 的 ^[2-9A-HJKMNP-Z]{6}$） */
export const ROOM_CODE_PATTERN = /^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{6}$/

/** 编译期自检：字符集必须恰好 31 位且无重复。 */
if (new Set(ROOM_CODE_ALPHABET).size !== 31) {
  throw new Error(`房间码字符集必须为 31 个不重复字符，当前 ${new Set(ROOM_CODE_ALPHABET).size}`)
}

/** 生成房间码。密码学随机；Phase B 正式房间由服务端签发，这里只用于本地预览。 */
export function generateRoomCode(): string {
  const bytes = new Uint8Array(ROOM_CODE_LENGTH)
  if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') {
    crypto.getRandomValues(bytes)
  } else {
    for (let i = 0; i < bytes.length; i += 1) bytes[i] = Math.floor(Math.random() * 256)
  }
  let code = ''
  for (let i = 0; i < bytes.length; i += 1) {
    code += ROOM_CODE_ALPHABET[(bytes[i] ?? 0) % ROOM_CODE_ALPHABET.length]
  }
  return code
}

/** 输入清洗：去空格与连字符、转大写。 */
export function normalizeRoomCode(input: string): string {
  return input.replace(/[\s-]/g, '').toUpperCase()
}

export function isValidRoomCode(input: string): boolean {
  return ROOM_CODE_PATTERN.test(normalizeRoomCode(input))
}

/** ABCDEF → ABC-DEF（仅展示）。 */
export function formatRoomCode(code: string): string {
  const normalized = normalizeRoomCode(code)
  if (normalized.length <= 3) return normalized
  return `${normalized.slice(0, 3)}-${normalized.slice(3)}`
}

export function roomPath(code: string): string {
  return `/r/${normalizeRoomCode(code)}`
}
