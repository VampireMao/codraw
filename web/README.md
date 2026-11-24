# 同绘 CoDraw · Web（Vue 3 + Canvas 2D）

> 一块纸感的无限画布，打开链接就能一起画。
> 本目录是前端全部代码；后端在 `../backend`。
> **契约唯一事实源：`../docs/PROTOCOL.md`**（v1，已冻结）。

## 快速开始

```bash
# 环境：Node 24.18（不要用 26，部分依赖 engines 不满足）
export NVM_DIR="$HOME/.nvm" && . "$HOME/.nvm/nvm.sh" && nvm use 24.18.0

npm install
npm run dev          # http://localhost:5173，/api 与 /ws 代理到 :8080
npm run typecheck    # vue-tsc + tsc
npm run lint         # eslint（0 warning 门禁）
npm run test:run     # vitest 180 用例
npm run build        # 产物到 dist/
```

一键门禁（类型 → 单测 → lint → 构建 → 产物校验）：

```bash
bash ../scripts/test-web.sh
```

**本地联调**（两个终端）：

```bash
# 终端 A：后端（内存模式，零依赖）
cd ../backend
STORE=memory BROKER=memory APP_ENV=development \
  READONLY_SECRET="local-dev-only-readonly-secret-32bytes-plus" \
  ALLOWED_ORIGINS="http://localhost:5173" \
  go run ./cmd/server

# 终端 B：前端
npm run dev
```

> `STORE=memory` 时后端重启会丢房间（这是预期行为）。
> 需要跨重启验证持久化时用 `STORE=postgres`。

## 目录结构

```
web/
├── src/
│   ├── canvas/                  ← 画布引擎（纯 TS，不依赖 Vue）
│   │   ├── geometry.ts           点/矩形/距离/相交等纯函数
│   │   ├── viewport.ts           世界↔屏幕变换、锚点缩放、边界约束、fit
│   │   ├── smoothing.ts          点抽稀 + 二次贝塞尔路径生成
│   │   ├── text.ts               CJK 换行（含避头尾规则）
│   │   ├── shapes.ts             数据模型、命中测试、wire 序列化（kind 字段）
│   │   ├── renderer.ts           双 Canvas 分层、rAF 合帧、DPR、网格
│   │   ├── history.ts            撤销栈（场景快照）
│   │   ├── perf.ts               性能自测（重绘耗时，非 rAF 帧率）
│   │   └── tools/                每个工具实现同一套 Pointer 接口
│   ├── net/                     ← 协议层（Phase B 核心）
│   │   ├── types.ts              全部消息类型 + 错误码文案（PROTOCOL §3/§4/§8）
│   │   ├── envelope.ts           信封唯一构造入口 + 入站白名单校验
│   │   ├── seq.ts                序号状态机（缺口/缓冲/快照攒批/pending）——纯函数
│   │   ├── realtime.ts           协调器：握手时序、乐观渲染、ack 对账、重连补齐
│   │   ├── ws.ts                 连接层：心跳、退避重连、死链检测
│   │   ├── persistence.ts        IndexedDB：appliedSeq + pending + cid
│   │   ├── session.ts            依赖组装（store + ws + realtime + idb）
│   │   ├── rest.ts               REST 客户端（/api/rooms、readonly-token、snapshot）
│   │   └── roomCode.ts           31 字符房间码（排除 0 O 1 I L）
│   ├── stores/
│   │   ├── board.ts              Pinia：形状、选择、连接态、成员
│   │   ├── scenePort.ts          op → store 的翻译层
│   │   └── storage.ts            localStorage（昵称/密钥）+ sessionStorage（cid）
│   ├── components/
│   │   ├── board/                画布周边 UI（工具条、状态胶囊、分享面板）
│   │   └── ui/                   CoButton/CoTooltip/CoToast/CoModal/CoIcon
│   ├── views/                   HomeView / RoomView / PerfView / NotFoundView
│   ├── styles/                  tokens.css（UX-SPEC §4）、base.css、app.css
│   └── router/
├── tests/                      180 用例（viewport/shapes/smoothing/text/seq/envelope/realtime/ws/history/roomCode）
└── scripts/gen-icons.mjs       无依赖 PNG 图标生成
```

## 协议实现（Phase B）

### 信封：唯一构造点

`net/envelope.ts` 的 `buildEnvelope()` 是**唯一**能产生出站信封的地方，自动补
`{v:1, roomId, clientId, ts}`。业务代码里没有第二处 `JSON.stringify` 信封
（`scripts/local-contract-check.sh` 会检查这一点）。

入站 `parseFrame()` 做三级校验：JSON 可解析 → `v === 1` → `type` 在服务端白名单内。
未知 `type` 返回 `unknown_type` 并且**只记录不报错**（协议 §2.3.1，避免错误风暴）。

### 时序：hello 与 joined 并发

```
WS open ──┬─ 客户端立即发 hello{lastSeq, epoch}   （不等 joined）
          └─ 服务端立即发 joined{you, room.seq, limits}
                    ↓
   joined.room.seq > appliedSeq ? resync.request : 直接 online
```

`WsClient.send()` 在 `joined` 之前**只允许 `hello`**（`allowBeforeJoined` 参数），
这是把协议时序编码进类型系统的做法，写错会直接返回 `false` 而不是发出去被服务端踢。

### 序号状态机（`net/seq.ts`，纯函数）

所有决策返回 `SeqEffect[]`，由调用方执行。三条规则：

| 情况 | 行为 |
|---|---|
| `seq <= appliedSeq` | 丢弃（重放去重） |
| `seq === appliedSeq + 1` | 应用，**并自动 flush 缓冲中连续的部分** |
| `seq > appliedSeq + 1` | 入 `buffer`，返回 `gap` 让上层发 `resync.request` |

缓冲里存的是**完整事件体**（不是只有 seq），所以补齐后能直接应用，不用回头找原始帧。
超过 512 条缺口直接转快照（`reset`），避免被异常服务端撑爆内存。

### 断线补齐（F6 的四条不变量）

1. **pending 落 IndexedDB**，不只是内存（刷新/崩溃不丢）
2. **重放复用原 `opId`**，顺序严格按 `createdAt`（服务端靠 `(clientId, opId)` 幂等）
3. **`appliedSeq` 同时持久化**，否则重连时不知道从那里补
4. **`appliedOpIds` 集合**做最后一道去重闸，同一 op 绝不二次应用

在线/离线分支收敛于一处：

```ts
submitOp(payload, shapeIds)
  ├─ 先 applyLocalOp（乐观上屏，60% 不透明度）
  ├─ 入 pending + 落盘
  └─ status === 'online' ? 立刻发 : 等 enterLiveIfReady() 重放
```

### 状态机与 UI 的对应

| store.status | 胶囊文案 | 触发 |
|---|---|---|
| `online` | 已连接 | joined 已到 **且** transport 连通 **且** 无待补缺口 |
| `connecting` | 连接中… | 首次建连 |
| `reconnecting` | 重连中 / 重连中 · 待同步 N 条 | 断线退避中（N = pending 数） |
| `offline` | 离线 / 离线 · 待同步 N 条 | 不可恢复错误或用户主动离开 |

**关键约束**：`enterLiveIfReady()` 会在 `transportUp === false` 时把状态从 `online`
降回 `reconnecting`。否则会出现「状态胶囊说已连接，但笔迹其实没发出去」——这是
面试现场最容易翻车的一类 bug。

### 心跳与死链

- 25s 发一次 `ping`（协议建议 20s，留余量）
- 45s 没收到**任何**帧 → 判定半开连接，主动断开重连
- `visibilitychange` 回前台立刻 `checkAlive()`：移动端切回来时 socket 往往已经死了
  但 `onclose` 还没触发，光等退避计时器会让用户对着死画布发呆

### 退避

`0.5 → 1 → 2 → 4 → 8 → 10s`，每档 ±20% 抖动（避免断网恢复时全房间同时重连）。
`1008`（策略违规）不重试；连续 3 次失败后走一次 REST 探测，确认房间是否还存在
（浏览器不让读 WS 握手失败原因，只能这样补语义）。

## 画布引擎设计

### 1. 坐标系统：只有 scale + translate

```
screen = world * scale + translate
world  = (screen - translate) / scale
```

不用矩阵：画布只有缩放与平移，反解是一行除法。缩放范围 10%–800%，
`zoomAt(anchor, scale)` 保证**锚点下的世界点不动**：

```ts
const world = screenToWorld(anchor)
this.scale = nextScale
this.x = anchor.x - world.x * nextScale
this.y = anchor.y - world.y * nextScale
```

边界约束只限制「视口中心距世界原点 ≤ ±5e5」，而不是硬夹 `translate`：
硬夹 translate 会让用户在高缩放下「贴边卡死」，夹中心则永远能继续画。

### 2. 渲染：双 Canvas 分层，而不是脏矩形

| | base 层 | overlay 层 |
|---|---|---|
| 内容 | 网格 + 已提交图形 | 当前笔迹草稿、选框、选中手柄、他人光标 |
| 重绘时机 | 内容/视口/选中变化 | 每帧可重绘（内容少） |

**为什么不分脏矩形**：自由笔的脏区就是整条笔迹包围盒。多人同时画时脏区频繁重叠，
要合并、裁剪、处理重入（绘制中新笔画又产生新脏区），代码量与 bug 面远大于收益；
而「整层重绘 + Path2D 缓存 + 视口裁剪」在 1000 条笔迹下实测仍有 92% 余量。
反过来分层带来确定性收益：落笔时 base 层位图完全不动，每帧成本只与当前这一笔相关。

### 3. 三个关键缓存

1. **Path2D 缓存**（key = shape.id，失效条件 = `rev` 变化）
2. **文本布局缓存**（key = 文本前 8 字 + 宽度 + 字号）
3. **网格 pattern 缓存**（key = 平铺像素尺寸）

`rev` 由所有 mutation 统一推进，天然是缓存版本号——这也是协议里 `rev` 字段的额外价值。

### 4. 自由笔：抽稀 + 二次贝塞尔

- **抽稀**：相邻采样点距离 < 1.2 世界单位就丢弃（在线低通滤波），减少 40%–60% 点
- **平滑**：以「相邻两点中点」为曲线端点做 `quadraticCurveTo`，控制点是原始采样点。
  天然 C1 连续、不增加存储、计算量比 Catmull-Rom 低
- **压感**：`pointerType === 'pen'` 时记录 `pressure` 到第三个分量

不用 RDP（道格拉斯-普克）：RDP 需要完整笔画才能决定保留哪些点，不适合落笔即绘。

### 5. 便签与中文输入法

便签用 HTML `<textarea>` 浮层（不是 canvas 内直接输入）：浏览器原生 IME 处理
`compositionstart/end`，候选词窗、拼音下划线、中文标点全部正确。
失焦提交，Esc 取消，`⌘/Ctrl+↵` 快速完成。空便签提交时自动丢弃。

协议里便签是 `kind:"text"` + `data.variant:"note"`（不新增 kind 枚举，PROTOCOL §5.2）。

CJK 换行在 `canvas/text.ts` 里做了最小可用的**避头尾**：
「。」「，」不允许出现在行首（悬挂在上一行末尾，最多 2 个），
「（」「《」不允许留在行尾；拉丁词不在中间断开。

### 6. 指针事件与多端

- 全站只用 **Pointer Events**，`touch-action: none`
- 单指绘制；第二根手指落下立即转捏合，并**丢弃进行中的笔迹**（避免误触断笔）
- 双指缩放同时做 `zoomAt` 与 `panBy(中心位移)`，所以「捏合 + 双指平移」同时生效
- 空格 / 中键 / 只读态 → 强制平移

### 7. 撤销：场景快照

一次手势开始时拍快照，结束时比对 `id + rev` 序列，有净变化才入栈。
**拖动 60fps 的路径完全不碰历史栈**，只在手指抬起时入栈一次。
撤销后把受影响的 shape 重新发 `update`/`delete`（协议没有逆操作，靠 seq 顺序 LWW 收敛）。

### 8. 视觉细节

- 网格 `rgba(22,24,29,.13)` 单点，间距随缩放跳档到 15–46px，永远不密不疏
- 便签顶部 3px 暖色条 + 1px 低对比描边，暗示「这是一张可以拖的纸」
- 选中框 1.5px、四角 7px 手柄**按 1/scale 反向缩放**，任何缩放级别都是同一视觉尺寸
- 他人光标是填充箭头 + 白描边 + 同色圆角昵称牌
- 工具条按钮 44×44（≥44 触控底线），图标统一 1.5px 描边、`currentColor`

## 性能自测

### 方法一：Node 基准（CPU 侧热路径）

```bash
npx tsx scripts/bench-canvas.mts
```

实测（WSL2 / Node 24.18，1000 条 × 60 点 = 60000 点）：

| 项目 | mean | p95 |
|---|---|---|
| 单帧：变换 + 视口裁剪 1000 图形 | 0.008ms | 0.048ms |
| 单帧：缩放步进 + 裁剪 | 0.006ms | 0.008ms |
| 点击命中测试（最坏扫 1000 个） | 0.012ms | 0.041ms |
| Path2D 缓存查找 1000 次 | 0.017ms | 0.023ms |
| 序列化 1000 shape | 0.771ms | 1.473ms |
| 场景包围盒 | 0.030ms | 0.050ms |
| 撤销快照（深拷贝 1000 条） | 0.765ms | 2.239ms |

### 方法二：浏览器内真实重绘耗时（`/perf`）

**为什么不测 rAF 帧率**：窗口不可见 / 离屏时 rAF 被浏览器限流到 1Hz，测出来的
「帧率」是节流结果，不是画布性能（实测同一份代码会得出 1.1fps vs 1.2ms/帧 两个数）。
正确指标是**一次完整重绘（网格 + 全部可见图形 + overlay）的耗时**。

打开 `http://localhost:5173/perf`（仅 dev 或 `?debug=1`），点「运行测量」：

| 场景 | 重绘耗时 | 占 60fps 预算 |
|---|---|---|
| 1000 条笔迹 / 59,652 点（全部可见） | **1.10ms** | 6.6% |
| 2000 条笔迹 / 119,400 点（全部可见） | **2.30ms** | 13.8% |
| 1000 条笔迹（800% 放大，视口裁剪生效） | 0.04ms | 0.2% |

即 1000 笔迹下仍有 93% 的帧预算留给光栅化与合成。

## PWA

- manifest：名称「同绘 CoDraw」、主题色 `#3D5AFE`、背景 `#F7F6F2`、`display: standalone`
- 图标由 `scripts/gen-icons.mjs` 生成：手写 PNG 编码器（`node:zlib` deflate）
  + 点到折线距离光栅化 + 解析式抗锯齿，**不需要 sharp/canvas 原生依赖**，CI 可复现
- Workbox 预缓存全部静态资源，`navigateFallback` 排除 `/api/*` 与 `/ws`
- 只对房间元信息做 `NetworkFirst`（允许离线回显房间名），其余 API 不缓存

## 契约对齐（Phase B 第一优先级）

五项已全部落实，`bash ../scripts/local-contract-check.sh` 可验证：

| # | 要求 | 落地位置 |
|---|---|---|
| 1 | 房间码 31 字符（去 L） | `net/roomCode.ts` + `tests/roomCode.test.ts` 断言「绝不生成 L」 |
| 2 | 新信封 + 拆 hello/joined | `net/types.ts`、`net/envelope.ts` |
| 3 | 路由 `/api/rooms` + readonly-token | `net/rest.ts` |
| 4 | 统一 `send(type,payload)` | `net/envelope.ts` + `net/ws.ts` |
| 5 | 未知 type 白名单忽略 | `net/envelope.ts#parseFrame` |

## 错误与降级路径

面试讲「异常处理」时的直接素材：**每条错误码在前端都有确定的表现，没有一条是静默失败**。
实现集中在 `net/realtime.ts` 的 `handleError()` / `handleAck()` 与 `net/ws.ts` 的重连策略。

### 写操作被拒（`ack{status:"rejected"}`）

`ack` 只发给发起者，所以这里是「我自己这一笔被服务端拒了」——必须回滚 + 告知。

| `payload.code` | 前端表现 | 是否重试 |
|---|---|---|
| `forbidden` | **回滚该 op 的乐观渲染** + toast「只读链接无法编辑，向房主申请编辑权限」 | 否（重试必然再拒） |
| `invalid_op` | 回滚 + toast「操作无效，已撤销」 | 否 |
| `shape_too_large` | 回滚 + toast（提示这一笔太大） | 否（应分段，当前未做自动分段） |
| `shape_not_found` | 回滚 + toast「图形已不存在，正在重新同步」 | 否（随后靠 resync 收敛） |
| `shape_deleted` | 回滚 + toast「图形已被删除」 | 否 |
| `duplicate_shape` | 回滚 + toast「图形重复，已重新生成」 | 否（当前不自动换 id 重试） |
| `room_full_shapes` | 回滚 + toast「房间图形已达上限」 | 否（用户需清理） |
| `rate_limited` | 回滚 + toast「操作太快了，稍等一秒」 | 否（客户端已有 op 节流，正常不会触发） |
| 未知 code | 回滚 + toast「操作被拒绝，已撤销」 | 否 |

**关键点**：回滚只删「仍处于 `pending` 且属于这条 op」的图形（`scenePort.rollbackOp`），
不会误删服务端已确认的同 id 图形；pending 队列同步出队，`appliedSeq` 不变。

### 服务端 `error` 帧

| `payload.code` | 前端表现 | 连接 |
|---|---|---|
| `room_not_found` | 整页切「房间不存在或已过期」+ 新建白板按钮 | 停止重连 |
| `forbidden` | toast「只读链接无法编辑…」，UI 保持只读 | **保持**（协议要求不断线） |
| `snapshot_too_large` | toast「房间内容较多，正在改用分页加载」→ **自动走 REST 分页** | 保持 |
| `rate_limited` | toast + 按 `retryAfterMs` 退避 | 保持 |
| `unsupported_version` | toast「页面版本过旧，请刷新」 | 服务端会关，不重连 |
| `message_too_large` | toast「这一笔太长」 | 保持（连续 3 次服务端才关） |
| `unauthenticated` | toast「身份信息失效，请重新进入房间」 | 停止重连 |
| `internal` / 其他 `fatal:false` | 仅在 `fatal:true` 时 toast，其余静默记录 | 保持 |
| 未知 code | 兜底文案「网络异常，请稍后再试」 | 保持（**不崩、不断线**） |

### `snapshot_too_large` 的完整降级链

```
joined → resync.request
   ├─ 服务端能增量 → resync.events（连续应用）
   ├─ 服务端发分块快照 → resync.snapshot（收齐才替换）
   └─ 图形 > 5000，服务端拒绝分块 → error{snapshot_too_large}
          ↓
      GET /api/rooms/{code}/snapshot?after=…&limit=500  （按 shape.id 字典序翻页）
          ↓
      拉完 → commitSnapshot()   ← 与 WS 分块快照**同一条落地路径**
```

分页实现有 4 个保护：**页数上限 40**（防异常服务端死循环）、**并发重入只拉一次**、
**失败给明确 toast**、**未注入 loader 时提示刷新**（绝不静默卡住）。
拉完的 `seq` 会写进 `appliedSeq`，后续 op 仍走正常的缺口判定。

### 连接层错误与重连

| 情况 | 表现 | 重试 |
|---|---|---|
| 网络断开 / `1006` | 胶囊「重连中 · 待同步 N 条」 | 是，`0.5→1→2→4→8→10s` ±20% 抖动 |
| `1008` 策略违规（Origin/鉴权） | 胶囊「离线」 | **否**（重试无意义） |
| 握手 HTTP `429 room_full` | 整页「房间人数已满，请稍后再试」 | 否 |
| 握手 HTTP `503 server_busy` | 提示后按退避重试 | 是 |
| 连续 3 次重连失败 | 触发一次 REST 探测房间是否存在 | 房间不存在则停 |
| 45s 收不到任何帧 | 判定半开连接，主动断开重连 | 是 |
| 页面回到前台 | `checkAlive()` 立刻检查（不等退避计时器） | 视情况 |

### 客户端主动节流（减少与服务端限流正面冲突）

| 项目 | 策略 | 理由 |
|---|---|---|
| `presence.cursor` | 50ms 一条（20Hz），且未 online 不发 | 协议 §7 上限 20/s |
| `resync.request` | **1 秒 1 次，trailing 合并**（首个立刻发，窗口内后续合并为末尾 1 次） | 服务端限 2 次/秒；丢弃会漏缺口导致卡在重连中 |
| `op.add` | 拖动期间不逐帧发，**一次手势一条 update** | 60fps 下每帧一条会瞬间打爆限流 |
| IndexedDB 落盘 | 400ms 节流合并 | 落笔时 pending 每条都变，不节流会打爆 IDB 事务 |

## 测试覆盖（190 用例）


| 文件 | 用例数 | 覆盖点 |
|---|---|---|
| `viewport.test.ts` | 11 | 世界↔屏幕往返、锚点缩放不动点、10–800% 钳制、fit、resize 保持中心 |
| `shapes.test.ts` | 29 | 构造/命中测试/序列化 round-trip/非法输入不崩/样式钳制 |
| `smoothing.test.ts` | 10 | 抽稀阈值、贝塞尔控制点、闭包、空输入 |
| `text.test.ts` | 12 | CJK 折行、避头尾、拉丁词不断开、硬换行 |
| `seq.test.ts` | 34 | **缺口缓冲/乱序补齐/去重/快照攒批/epoch/pending 队列/resync 节流/完整断线剧本** |
| `envelope.test.ts` | 19 | 信封自动补字段、UTF-8 字节上限、白名单、未知 type 忽略 |
| `realtime.test.ts` | 31 | **握手时序/乐观渲染/ack 三态/重连重放/快照替换/epoch 作废** |
| `snapshotFallback.test.ts` | 10 | **REST 分页拼接/页数上限/并发重入只拉一次/失败提示/seq 续接** |
| `ws.test.ts` | 24 | 握手 URL、心跳、死链、退避序列、前台恢复、fatal 不重试 |
| `history.test.ts` | 6 | 撤销栈对称性、上限、订阅 |
| `roomCode.test.ts` | 8 | 31 字符集、绝不生成 L、大小写不敏感 |

## 已知取舍 / 没做的事

- **不做像素擦除**：协议 `delete` 是整条软删除（UX-SPEC §7.7 也明确不做）
- **不做椭圆工具**：形状模型与渲染都已支持 `ellipse`，工具条按 UX-SPEC 只放 6 个
- **不做导出 PNG 入口**：`Renderer.exportViewportPng()` 已实现，属 P1
- **撤销是本地 + 补发**：不是服务端逆操作，依赖 seq 顺序的 LWW 收敛
- **只读 token 不本地缓存**（PM 裁决）：每次进房从 URL 读——撤销后必须立刻失效，缓存反而制造不一致
- **`STORE=memory` 重启丢房间是设计而非缺陷**（ARCHITECTURE §2.1）：持久化演示用 `STORE=postgres`
- **`room.meta` 改标题未做**：P0 不需要，已列入 P1
- **`/api/v1/meta` 版本协商未接**：3 天工期无旧版本残留，属真实产品才需要的机制
- **`shape_too_large` 不自动分段**：只回滚 + 提示；自动按点数切段属 P1
