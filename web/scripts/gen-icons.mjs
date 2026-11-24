/**
 * 生成 PWA PNG 图标（无外部依赖）。
 *
 * 做法：手写一个最小 PNG 编码器（zlib 用 node:zlib），
 * 用「点到路径距离」把 SVG 的曲线描边光栅化成抗锯齿位图。
 * 好处：CI 里不需要 sharp/canvas 这类原生依赖，构建可复现。
 */
import { deflateSync } from 'node:zlib'
import { writeFileSync, mkdirSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const outDir = resolve(here, '../public/icons')
mkdirSync(outDir, { recursive: true })

/* ---------------------------------- PNG ---------------------------------- */

function crc32(buf) {
  let c = ~0
  for (let i = 0; i < buf.length; i += 1) {
    c ^= buf[i]
    for (let k = 0; k < 8; k += 1) c = (c >>> 1) ^ (0xedb88320 & -(c & 1))
  }
  return ~c >>> 0
}

function chunk(type, data) {
  const len = Buffer.alloc(4)
  len.writeUInt32BE(data.length, 0)
  const typeBuf = Buffer.from(type, 'ascii')
  const crcBuf = Buffer.alloc(4)
  crcBuf.writeUInt32BE(crc32(Buffer.concat([typeBuf, data])), 0)
  return Buffer.concat([len, typeBuf, data, crcBuf])
}

function encodePng(width, height, rgba) {
  const ihdr = Buffer.alloc(13)
  ihdr.writeUInt32BE(width, 0)
  ihdr.writeUInt32BE(height, 4)
  ihdr[8] = 8 // bit depth
  ihdr[9] = 6 // color type RGBA
  ihdr[10] = 0
  ihdr[11] = 0
  ihdr[12] = 0

  const stride = width * 4
  const raw = Buffer.alloc((stride + 1) * height)
  for (let y = 0; y < height; y += 1) {
    raw[y * (stride + 1)] = 0 // filter: none
    rgba.copy(raw, y * (stride + 1) + 1, y * stride, y * stride + stride)
  }

  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ])
}

/* -------------------------------- 光栅化 -------------------------------- */

const hex = (h) => {
  const v = Number.parseInt(h.slice(1), 16)
  return [(v >> 16) & 255, (v >> 8) & 255, v & 255]
}

/** 点到线段的距离（像素坐标）。 */
function distSeg(px, py, ax, ay, bx, by) {
  const dx = bx - ax
  const dy = by - ay
  const lenSq = dx * dx + dy * dy
  let t = 0
  if (lenSq > 0) t = ((px - ax) * dx + (py - ay) * dy) / lenSq
  t = Math.max(0, Math.min(1, t))
  return Math.hypot(px - (ax + t * dx), py - (ay + t * dy))
}

/** 三次贝塞尔采样成折线，配合 distSeg 计算描边覆盖率。 */
function cubic(p0, p1, p2, p3, steps = 24) {
  const pts = []
  for (let i = 0; i <= steps; i += 1) {
    const t = i / steps
    const mt = 1 - t
    const x = mt * mt * mt * p0[0] + 3 * mt * mt * t * p1[0] + 3 * mt * t * t * p2[0] + t * t * t * p3[0]
    const y = mt * mt * mt * p0[1] + 3 * mt * mt * t * p1[1] + 3 * mt * t * t * p2[1] + t * t * t * p3[1]
    pts.push([x, y])
  }
  return pts
}

/**
 * 绘制一张图标。
 * @param {number} size     边长
 * @param {boolean} maskable 是否留出安全边距（maskable 需要 40% 内缩）
 */
function drawIcon(size, maskable) {
  const buf = Buffer.alloc(size * size * 4)
  const S = size / 64 // 设计稿基于 64×64
  const ink = hex('#16181D')
  const paper = hex('#F7F6F2')
  const brand = hex('#3D5AFE')

  // maskable：背景铺满，图形缩到安全区内
  const inset = maskable ? 0.16 : 0
  const scale = maskable ? (1 - inset * 2) / 1 : 1
  const shift = maskable ? 64 * inset : 0
  const mapX = (x) => (shift + x * scale) * S
  const mapY = (y) => (shift + y * scale) * S
  const radius = 14 * scale * S

  // 曲线（设计稿坐标）：M13 46 C 与 favicon.svg 一致
  const strokePts = []
  const seg1 = cubic([13, 46], [19.4, 43.3], [22, 37.5], [25.2, 31.1])
  const seg2 = cubic([25.2, 31.1], [27.8, 25.8], [30.5, 21.5], [34.2, 21.5])
  const seg3 = cubic([34.2, 21.5], [38.2, 21.5], [39.5, 25.2], [41.6, 30])
  const seg4 = cubic([41.6, 30], [44, 35.3], [46.9, 41.2], [52.8, 43.9])
  for (const seg of [seg1, seg2, seg3, seg4]) {
    for (let i = 0; i < seg.length; i += 1) {
      const prev = strokePts[strokePts.length - 1]
      if (!prev || prev[0] !== seg[i][0] || prev[1] !== seg[i][1]) strokePts.push(seg[i])
    }
  }

  const strokeW = 4.2 * scale * S
  const dots = [
    { x: 13, y: 47, r: 4.2, color: brand },
    { x: 51, y: 45.6, r: 4.2, color: paper },
  ]

  // 圆角矩形背景的内缩判定
  const pad = 0
  const bx = pad
  const by = pad
  const bw = size - pad * 2
  const bh = size - pad * 2
  const rr = maskable ? 0 : radius

  for (let y = 0; y < size; y += 1) {
    for (let x = 0; x < size; x += 1) {
      const px = x + 0.5
      const py = y + 0.5

      // 背景：圆角矩形（maskable 用全幅）
      let bgA = 0
      if (rr <= 0.5) {
        bgA = 1
      } else {
        const dx = Math.max(bx + rr - px, 0, px - (bx + bw - rr))
        const dy = Math.max(by + rr - py, 0, py - (by + bh - rr))
        const d = Math.hypot(dx, dy)
        bgA = Math.max(0, Math.min(1, rr - d + 0.5))
        if (dx === 0 && dy === 0) bgA = 1
      }
      if (bgA <= 0) continue

      let r = ink[0]
      let g = ink[1]
      let b = ink[2]

      // 描边曲线：像素到折线距离 < 半笔宽 → paper 色
      let minD = Infinity
      for (let i = 1; i < strokePts.length; i += 1) {
        const d = distSeg(
          px,
          py,
          mapX(strokePts[i - 1][0]),
          mapY(strokePts[i - 1][1]),
          mapX(strokePts[i][0]),
          mapY(strokePts[i][1]),
        )
        if (d < minD) minD = d
      }
      const strokeA = Math.max(0, Math.min(1, strokeW / 2 - minD + 0.5))
      if (strokeA > 0) {
        r = r + (paper[0] - r) * strokeA
        g = g + (paper[1] - g) * strokeA
        b = b + (paper[2] - b) * strokeA
      }

      for (const dot of dots) {
        const d = Math.hypot(px - mapX(dot.x), py - mapY(dot.y))
        const rad = dot.r * scale * S
        const a = Math.max(0, Math.min(1, rad - d + 0.5))
        if (a > 0) {
          r = r + (dot.color[0] - r) * a
          g = g + (dot.color[1] - g) * a
          b = b + (dot.color[2] - b) * a
        }
      }

      const o = (y * size + x) * 4
      buf[o] = Math.round(r)
      buf[o + 1] = Math.round(g)
      buf[o + 2] = Math.round(b)
      buf[o + 3] = Math.round(bgA * 255)
    }
  }

  return encodePng(size, size, buf)
}

const targets = [
  { file: 'icon-192.png', size: 192, maskable: false },
  { file: 'icon-512.png', size: 512, maskable: false },
  { file: 'icon-maskable-512.png', size: 512, maskable: true },
  { file: 'apple-touch-icon.png', size: 180, maskable: true },
]

for (const t of targets) {
  const png = drawIcon(t.size, t.maskable)
  writeFileSync(resolve(outDir, t.file), png)
  console.log(`✓ ${t.file} (${t.size}×${t.size}, ${(png.length / 1024).toFixed(1)} KB)`)
}

// 顺带输出 apple-touch-icon 到 public 根（index.html 里引用不到 /icons 下的同名）
writeFileSync(resolve(outDir, '../apple-touch-icon.png'), drawIcon(180, true))
console.log('✓ apple-touch-icon.png (public 根)')
