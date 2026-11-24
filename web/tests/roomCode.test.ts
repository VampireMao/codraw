import { describe, expect, it } from 'vitest'
import {
  formatRoomCode,
  generateRoomCode,
  isValidRoomCode,
  normalizeRoomCode,
  roomPath,
  ROOM_CODE_ALPHABET,
  ROOM_CODE_PATTERN,
} from '@/net/roomCode'
import { errorText } from '@/net/types'

describe('房间码', () => {
  it('字符集是 31 个且绝不含 L（契约硬修正）', () => {
    expect(ROOM_CODE_ALPHABET).toHaveLength(31)
    expect(new Set(ROOM_CODE_ALPHABET).size).toBe(31)
    expect(ROOM_CODE_ALPHABET).not.toContain('L')
    for (const banned of ['0', 'O', '1', 'I', 'L']) {
      expect(ROOM_CODE_ALPHABET).not.toContain(banned)
    }
  })

  it('生成的码永远合法，且一个 L 都不会出现', () => {
    for (let i = 0; i < 500; i += 1) {
      const code = generateRoomCode()
      expect(code).toHaveLength(6)
      expect(ROOM_CODE_PATTERN.test(code)).toBe(true)
      expect(code).not.toMatch(/[0O1IL]/)
      for (const char of code) expect(ROOM_CODE_ALPHABET).toContain(char)
    }
  })

  it('生成结果有足够熵（400 次几乎不重复）', () => {
    const set = new Set<string>()
    for (let i = 0; i < 400; i += 1) set.add(generateRoomCode())
    expect(set.size).toBeGreaterThan(395)
  })

  it('normalize 去连字符/空格并转大写', () => {
    expect(normalizeRoomCode('abc-def')).toBe('ABCDEF')
    expect(normalizeRoomCode(' ab cdef ')).toBe('ABCDEF')
    expect(normalizeRoomCode('AbC-dEf')).toBe('ABCDEF')
  })

  it('校验大小写不敏感，长度与字符集都必须匹配', () => {
    expect(isValidRoomCode('K7M2QX')).toBe(true)
    expect(isValidRoomCode('k7m2qx')).toBe(true)
    expect(isValidRoomCode('K7M-2QX')).toBe(true)
    expect(isValidRoomCode('K7M2Q')).toBe(false)
    expect(isValidRoomCode('K7M2QXZ')).toBe(false)
    expect(isValidRoomCode('K7M2Q0')).toBe(false) // 0 不在字符集
    expect(isValidRoomCode('K7M2QI')).toBe(false) // I 不在字符集
    expect(isValidRoomCode('K7M2QL')).toBe(false) // L 已被契约移除
    expect(isValidRoomCode('K7M2QO')).toBe(false) // O 不在字符集
  })

  it('展示为 ABC-DEF，存储与 URL 不带连字符', () => {
    expect(formatRoomCode('ABCDEF')).toBe('ABC-DEF')
    expect(formatRoomCode('abc-def')).toBe('ABC-DEF')
    expect(roomPath('abc-def')).toBe('/r/ABCDEF')
    expect(roomPath('K7M2QX')).toBe('/r/K7M2QX')
  })
})

describe('错误文案', () => {
  it('已知 code 有中文文案', () => {
    expect(errorText('forbidden')).toContain('只读')
    expect(errorText('room_full')).toContain('人数已满')
  })

  it('未知 code 走兜底，不返回 undefined', () => {
    expect(errorText('some_future_code')).toBe('网络异常，请稍后再试')
  })
})
