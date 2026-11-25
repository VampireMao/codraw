# 权限模型 · 房主与成员授权 v2

> 状态：**已定稿（PM）**｜替代 `PROTOCOL.md` §3.1 中「key 决定一切」的旧模型
> 实施者：Go 专家（`backend/**`）、Vue 专家（`web/**`）
> 冲突时以本文件为准；本文件未覆盖的细节回落到 `PROTOCOL.md`

---

## 1. 角色

| 角色 | 能做什么 | 怎么获得 |
|---|---|---|
| `owner` | 一切：编辑、授权他人、转移房主 | 创建房间时自动成为；或原房主转移 |
| `editor` | 编辑画布 | 被房主授权 |
| `viewer` | 只看（可平移缩放、可改自己的昵称） | **默认** |

> `owner` 隐含 `editor` 的全部能力。前端判断"能不能画"要用 `canEdit = owner || editor`。

---

## 2. 房主归属规则（核心状态机）

### 2.1 初始归属

- 创建房间的**第一个连接**成为 `owner`，与是否持有 `editorKey` 无关
- `editorKey` 从此**失去提升权限的作用**，退化为"邀请凭证"：持有它的人进入后仍是 `viewer`，需要房主授权才能编辑
  - 理由：如果 `editorKey` 仍能直接给 editor，房主就无法"独占"，第 2 条需求不成立
- **例外**：如果房间已有 `owner`，持有 `editorKey` 的人进入**不夺权**，只是普通 viewer

### 2.2 断线继承

房主离开（主动退出、断线、被踢）时，按**入场顺序**移交给下一个人：

```
owner 离开
  → 候选队列 = 当前在线成员，按 joinedAt 升序
  → 第一个仍在线的成员成为新 owner
  → 无人在线：owner 置空（房间回到"无主"状态）
```

**"无主"状态下的新进入者**：谁先进谁成为 owner（等价于 2.1）。

> 断线重连：同一 `clientId` 重新加入，**优先恢复原角色**（见 §4.4）。所以房主短暂掉线后重连，如果期间没被移交，会拿回 owner。

### 2.3 手动转移

- 只有当前 `owner` 能发起：`POST` 或 WS 消息 `room.transfer`，目标必须是当前在线成员
- 成功后：原 owner 降为 `editor`，目标升为 `owner`
- 全场广播新的 owner

### 2.4 同一 clientId 多开

同一浏览器多标签页共享 `clientId`（sessionStorage）。规则：

- 视为**同一成员**，只有一条在线记录
- 角色以该 `clientId` 为准，多个标签页共享
- 关闭其中一个标签页**不触发移交**（还有其他连接在）

---

## 3. 成员授权

### 3.1 授权（房主 → 任意在线成员）

```json
{
  "v": 1,
  "type": "room.grant",
  "roomId": "K7M2QX",
  "clientId": "c_owner",
  "ts": 1732358400000,
  "payload": {
    "targetClientId": "c_abc123",
    "role": "editor"
  }
}
```

- `role` 只允许 `editor` 或 `viewer`（不能授权 `owner`，转移必须用 `room.transfer`）
- 非 owner 发送 → `error{code:"forbidden"}`，**断开连接**（越权操作比普通错误严重）
- 目标不在线 → `error{code:"target_offline"}`，不断连接
- 成功后广播：

```json
{
  "type": "member.role",
  "seq": 42,
  "payload": { "clientId": "c_abc123", "role": "editor", "grantedBy": "c_owner" }
}
```

**`member.role` 必须带 `seq`**：它改变房间可观察状态（谁有权写），必须能被断线补齐。

### 3.2 撤销

`room.grant` 传 `role: "viewer"` 即为撤销。被撤销者：

- 立即失去写权限（后续 `op.add` 返回 `ack{rejected, forbidden}`）
- **正在进行的本地手势不回滚**，但该次提交会被拒，前端按既有逻辑回滚并提示
- 连接**不断开**

### 3.3 房主不能给自己授权

`targetClientId === 自己` → `error{code:"invalid_grant"}`。降级自己请用转移。

---

## 4. 连接与握手

### 4.1 握手参数

`GET /ws?room={code}&cid={clientId}&name={nickname}&key={editorKey|viewerKey}&rt={readonlyToken}&st={sessionToken}`

- `cid` 是客户端候选值，服务端**不信任**它；首次连接会生成权威 `clientId` 并通过 `joined.you.clientId` 返回。
- `st` 是服务端签发的会话 token（`docs/PROTOCOL.md` §2.1.1）。重连必须携带，否则视为新访客。
- 会话 token 只绑定 `clientId`，不改变角色；owner/editor 仍按 §4.2 判定。

### 4.2 角色判定顺序（服务端，严格按此顺序）

```
1. rt 存在且有效          → viewer（只读 token 永远只读，不可被授权提升）
2. 房间已有 owner 记录，且 cid == ownerClientId
                          → owner
3. grants 中存在 (room, cid) → 用 grant 的 role
4. 房间无 owner            → owner（§2.1 / §2.2 的"无主"分支）
5. 其他                   → viewer
```

> 注意第 1 条优先于第 2 条：拿着只读 token 进来的人，即使 `cid` 和房主相同（例如房主自己点了自己的只读链接），也必须是 viewer。这是安全底线。
>
> **`cid` 必须是服务端签发或校验过的身份**：在开启会话 token 后，没有有效 `st` 的连接会拿到一个全新的 `clientId`，因此 URL 里的 `cid` 无法命中第 2/3 条。这是防止“看到房主 clientId 就冒充房主”的关键，详见 `docs/PROTOCOL.md` §2.1.1。

### 4.3 `joined.payload.you` 新增字段

```json
"you": {
  "clientId": "c_abc123",
  "name": "小林",
  "color": "#3d5afe",
  "role": "owner",           // ← owner | editor | viewer
  "isOwner": true            // ← 便捷布尔，等价于 role === "owner"
}
```

`joined.payload` 另新增：

```json
"ownerClientId": "c_abc123"   // 当前房主，可能为 null（无主）
```

### 4.4 断线重连的角色恢复

- 房间内存保留 `lastRoles: map[clientId]role`（TTL = `ROOM_TTL`，或至少 10 分钟）
- 重连时先查 `lastRoles`，命中则恢复原角色**并跳过 §4.2 的第 4、5 条**
- 但**已被撤销的授权不能恢复**：`lastRoles` 在每次 `room.grant` 时同步更新，撤销即写入 `viewer`

> **实现修正（2026-11-24）**：`lastRoles` **只影响当前会话的连接**，不影响重新加入。
> 因为"退出再进"属于新的一次入场，按 §1 的「默认只读」原则应回到 viewer。
> 具体地：`roleOfCid`（鉴权用）查 `lastRoles`，`roleForJoin`（握手用）**不查**。
>
> 这样既保留了「转移房主后原房主当前会话仍是 editor」的平滑体验，
> 又保证「退出再进是 viewer」的安全默认。
>
> 同时：`room.transfer` **不再把原房主写成持久 grant**。转移是会话内的降级，
> 不是授权；否则原房主退出再进会带着 editor 权限回来。

---

## 5. 服务端权威校验（不可省略）

`op.add` 的鉴权改为：

```go
canWrite := role == "owner" || role == "editor"
if !canWrite { return ack{rejected, forbidden} }
```

**写权限必须在每次 op 判定，不能只在握手时缓存**——否则撤销授权不生效。

---

## 6. 前端行为

### 6.1 权限相关 UI

| 位置 | owner 看到 | editor 看到 | viewer 看到 |
|---|---|---|---|
| 顶栏身份区 | 头像 + 名字 + **「房主」徽标** | 头像 + 名字 | 头像 + 名字 + 「只读」徽标 |
| 在线成员列表 | 每人右侧有权限按钮（editor/viewer 切换）+ 「转移房主」 | 只读列表 | 只读列表 |
| 工具栏 | 全部工具 | 全部工具 | 只保留平移/缩放/适配（现状已实现）|
| 只读横幅 | 不显示 | 不显示 | 显示「只读模式 · 房主可授予你编辑权限」 |

### 6.2 权限变更的即时响应

收到 `member.role` 且 `clientId === 自己` 时：

- 升为 editor/owner → 立即显示工具栏，toast「你已获得编辑权限」
- 降为 viewer → 立即隐藏工具栏、清空选区，toast「你已被设为只读」

收到 `ownerClientId` 变化时：

- 若自己成为 owner → toast「你已成为房主」，成员列表出现管理按钮
- 若原 owner 变成别人 → 移除自己的管理按钮

### 6.3 空态

- 无主房间（`ownerClientId === null`）：不显示房主徽标，成员列表无管理按钮

---

## 7. 数据持久化

| 数据 | 存储 | 说明 |
|---|---|---|
| `ownerClientId` | rooms 表新增列 + 内存 | 重启后恢复 |
| `(room, cid) → role` | 新表 `room_grants` | 重启后恢复，避免授权丢失 |
| `joinedAt`（入场顺序） | 内存 | 继承顺序用，重启后按重连顺序重建即可 |

DDL：

```sql
ALTER TABLE rooms ADD COLUMN owner_client_id TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS room_grants (
    room_id     TEXT NOT NULL,
    client_id   TEXT NOT NULL,
    role        TEXT NOT NULL CHECK (role IN ('editor','viewer')),
    granted_by  TEXT NOT NULL,
    updated_at  BIGINT NOT NULL,
    PRIMARY KEY (room_id, client_id)
);
```

> `STORE=memory` 时这些只存内存，语义相同（重启即丢，符合既有设计）。

---

## 8. 边界与错误码

| 场景 | 结果 |
|---|---|
| 非 owner 发 `room.grant` | `error{forbidden}` + **断开**（4003）|
| 非 owner 发 `room.transfer` | `error{forbidden}` + **断开**（4003）|
| 授权目标不在线 | `error{target_offline}`，不断连接 |
| 授权目标是只读链接连接 | `error{target_readonly}`，不断连接；对方永远不会被提升 |
| 授权 `role: "owner"` | `error{invalid_grant}`，提示用 transfer |
| 授权自己 | `error{invalid_grant}` |
| 转移给不在线的人 | `error{target_offline}` |
| 转移给只读链接连接 | `error{target_readonly}`；否则会出现“owner 不能管理”的死锁 |
| viewer 发 `op.add` | `ack{rejected, forbidden}`，**不断连接**（现状保持）|
| 被撤销后继续发 `op.add` | 同上 |

## 8.1 HTTP 接口的鉴权（重要，容易漏）

`POST /api/rooms/{code}/readonly-token` 与 `.../revoke` 目前用 `editorKey` 鉴权。
新模型下 `editorKey` 不再是"编辑权"，但**仍可作为房主凭证**用于这两件事：

| 接口 | 鉴权方式 |
|---|---|
| 签发 / 撤销只读 token | 仍接受 `editorKey`（视为房主操作） |
| 查询房间 `GET /api/rooms/{code}?key=` | 返回该 key 对应的**当前**角色；`editorKey` 一律返回 `viewer`（它不再代表编辑权） |

> 为什么不改成"只有当前 owner 能签发"：owner 身份是连接级的（绑 `clientId`），
> 而这两个接口是无状态的 HTTP 调用。用 `editorKey` 作为"房主凭据"是务实选择：
> 它只发给建房者，且不能用来提升他人的画布权限（那必须走 `room.grant`）。
> 这个取舍写在这里，避免实现者两边猜。


---

## 9. 验收标准（PM 会逐条实测）

| # | 场景 | 期望 |
|---|---|---|
| P1 | 创建房间 | 自己 role=owner，有房主徽标 |
| P2 | 填房号加入（无 key） | role=viewer，只读横幅，工具栏隐藏 |
| P3 | 持编辑链接加入 | 仍是 viewer（不自动夺权）|
| P4 | 房主授权 B 为 editor | B 立即解锁工具栏；B 能画；房主看到 B 角色变化 |
| P5 | 房主撤销 B | B 立即只读；B 的 op 被拒；连接不断 |
| P6 | 房主转移给 B | B 成 owner；原房主变 editor；B 看到管理按钮 |
| P7 | 房主关闭页面 | 按入场顺序移交；第二个进入者成为 owner |
| P8 | 房主刷新页面 | 重连后恢复 owner（未被移交时）|
| P9 | viewer 尝试 `room.grant` | 被拒 + 断线 + 有明确提示 |
| P10 | 无主房间首个进入者 | 自动成为 owner |
| P11 | 只读 token 进入 | 永远是 viewer，即使被 grant 也不提升 |
| P12 | 授权后刷新 | 角色保持（grants 持久化）|

---

## 10. 实施建议（给两位专家）

**Go 专家**：
- `room` actor 内维护 `ownerClientID`、`grants map[string]string`、`joinOrder []string`
- `Member` 接口已有 `User()`，让 role 可变（`atomic.Pointer` 或经 room goroutine 单写）
- 断线继承要在 `presence.leave` 处理路径里做，注意"最后一个连接"判定（同 cid 多标签页）

**Vue 专家**：
- `store.role` 已是响应式；新增 `store.isOwner` computed 与 `store.ownerClientId`
- 成员列表加管理 UI；注意移动端空间，用底部抽屉或长按菜单
- 收到 `member.role` 要能动态增删工具栏，不能只靠 `props.readOnly`

**共同**：`member.role` 带 `seq`，走既有 resync 通道，不要新开广播路径。
