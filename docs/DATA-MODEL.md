# 同绘 CoDraw · 数据模型

> 负责人：架构师｜对应 `STORE=postgres` 模式。`STORE=memory` 时本文件不适用（进程内 map 等价实现）。
> 数据库：PostgreSQL 16｜驱动：`jackc/pgx/v5`（连接池 + `pgxpool`）｜**不用 ORM**。

---

## 1. 设计原则

1. **事件日志是真相源（source of truth）**，快照只是**加速读取的缓存**，任何时候都可以从 `events` 重建。
2. 所有事件**按房间、按 seq 单调**，`(room_code, seq)` 是天然主键，也是幂等的锚点。
3. 软删除（`deleted`）而非物理删除图形；tombstone 必须保留，否则重连的客户端无法得知"这个图形没了"。
4. **不存明文密钥**，只存 `sha256(key)`。
5. 所有时间字段为 `timestamptz`，由数据库 `now()` 产生，**不信任客户端时间**。

---

## 2. DDL

> **单一事实源：`docs/sql/0001_init.sql`**（已在本机 Postgres 16 实测执行通过）。
> 后端实现时把该文件**原样**复制到 `backend/internal/store/postgres/migrations/0001_init.sql` 并 `go:embed`，**不要手抄**。
> 下表仅解释设计意图，实际 DDL 以 SQL 文件为准。

| 表 | 作用 | 关键约束 |
|---|---|---|
| `rooms` | 房间元信息 + 密钥哈希 + seq 水位 | PK `code`；`code ~ '^[2-9A-HJKMNP-Z]{6}$'` |
| `events` | **真相源**：持久化事件日志 | PK `(room_code, seq)`；唯一 `(room_code, client_id, op_id)` |
| `snapshots` | 加速读取的压缩快照（可重建） | PK `(room_code, seq)`；`codec ∈ {zstd,gzip,none}` |
| `members` | 可选：最近来访者 | PK `(room_code, user_id)` |

```sql
-- 摘录：events 的核心定义（完整版见 docs/sql/0001_init.sql）
CREATE TABLE events (
    room_code   text        NOT NULL REFERENCES rooms(code) ON DELETE CASCADE,
    seq         bigint      NOT NULL,
    op_id       text        NOT NULL,
    client_id   text        NOT NULL,
    author_id   text        NOT NULL,
    action      text        NOT NULL,
    shape_id    text        NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (room_code, seq),
    CONSTRAINT events_action_enum CHECK (action IN ('create','update','delete','append','meta')),
    CONSTRAINT events_seq_pos CHECK (seq > 0)
);

CREATE UNIQUE INDEX uq_events_idem ON events (room_code, client_id, op_id);
```

> `rooms.code` 的 CHECK 用 `^[2-9A-HJKMNP-Z]{6}$`：排除 `0 O 1 I L` 后共 31 个字符（`2-9` 八个数字 + `ABCDEFGHJKMNPQRSTUVWXYZ` 二十三个字母）。写正则时注意 **`H` 与 `J` 之间没有 `I`**，**`K` 与 `M` 之间没有 `L`**。

### 2.1 为什么 `events.payload` 用 `jsonb` 而不是拆列

| 方案 | 优点 | 缺点 |
|---|---|---|
| **`jsonb`（采用）** | 协议演进零迁移；一条 op 原子写入；可直接回放 | 不能按字段建索引（本场景不需要） |
| 拆成 `shape_id/kind/x/y/...` | 可 SQL 查询几何 | 每种 kind 字段不同 → 宽表 + 大量 NULL；协议一改就迁移 |

白板的查询模式只有两种：**按 (room, seq) 范围扫** 和 **按 room 取最新快照**。都不需要拆字段。`jsonb` 的 200B/条开销可接受。

### 2.2 冗余字段的一致性

`rooms.shape_count` 与 `rooms.last_seq` 是冗余，**必须在同一事务内**与 `events` 插入一起更新：

```sql
BEGIN;
  INSERT INTO events (room_code, seq, op_id, client_id, author_id, action, shape_id, payload)
  VALUES ($1, $2, $3, $4, $5, $6, $7, $8);
  UPDATE rooms
     SET last_seq = GREATEST(last_seq, $2),
         shape_count = shape_count + $9,      -- create:+1 / delete:-1 / 其它:0
         last_active_at = now()
   WHERE code = $1;
COMMIT;
```

若有偏差，**以 `events` 为准**，可用下面的巡检 SQL 修复：

```sql
-- 巡检：shape_count 漂移（以 events 重放为准）
-- 只统计 create/delete 事件，且排除已删除的 shape
WITH live AS (
  SELECT room_code,
         count(*) FILTER (WHERE action = 'create' AND (payload->>'deleted')::bool IS NOT TRUE) AS created_live,
         count(*) FILTER (WHERE action = 'delete') AS deleted_cnt
    FROM events
   GROUP BY room_code
)
SELECT r.code,
       r.shape_count AS recorded,
       coalesce(l.created_live, 0) AS created_live,
       coalesce(l.deleted_cnt, 0)  AS deleted_cnt
  FROM rooms r
  LEFT JOIN live l ON l.room_code = r.code
 WHERE r.shape_count <> coalesce(l.created_live, 0);
```

> 注意：`delete` 是软删（`payload.deleted = true`），所以活图形数 ≈ `create` 条数 − `delete` 条数。上面的查询只做**粗校验**（`create` 数就是曾经创建过的总数）；精确值应以当前快照为准。**任何偏差都以 `events` 重放结果为准，不要以 `shape_count` 为准。**

---

## 3. 索引与查询对照

| 查询 | SQL 形态 | 命中索引 |
|---|---|---|
| 增量补发 | `WHERE room_code=$1 AND seq > $2 ORDER BY seq LIMIT $3` | PK `(room_code, seq)` |
| 幂等去重 | `WHERE room_code=$1 AND client_id=$2 AND op_id=$3` | `uq_events_idem` |
| 取最新快照 | `WHERE room_code=$1 ORDER BY seq DESC LIMIT 1` | `idx_snapshots_latest` |
| 建房查重 | `SELECT 1 FROM rooms WHERE code=$1` | PK |
| 过期清理 | `DELETE FROM rooms WHERE expires_at < now()` | `idx_rooms_expires` |
| 历史回放（P1） | `WHERE room_code=$1 ORDER BY seq LIMIT/OFFSET` | PK |

> 单房间 5000 条热日志时，补发查询是 **index range scan，<1ms**。这是"seq 连续补齐"能做到 p95 < 80ms 的底层原因之一。

---

## 4. 快照压缩：为什么选 zstd

| 维度 | zstd | gzip |
|---|---|---|
| 压缩率（JSON 图形数组） | **~6–8x** | ~4–5x |
| 压缩速度 | ~400MB/s | ~80MB/s |
| 解压速度 | ~1.5GB/s | ~200MB/s |
| Go 依赖 | `klauspost/compress/zstd`（纯 Go，无 CGO） | 标准库 `compress/gzip` |
| 二进制体积 | +~2MB | 0 |

**结论：选 zstd（`codec='zstd'`）**。理由：
1. 快照是**读多写少**（每次重连都可能读，每 500 op 才写一次），解压速度比压缩率更重要，zstd 两项都赢。
2. 500 图形 ≈ 250KB JSON → zstd 后 ~35KB，**单次读取省 200KB 传输**，对移动端明显。
3. `klauspost/compress` 是纯 Go，静态二进制不受影响；2MB 的体积代价在 60MB 预算内可接受。
4. 保留 `codec` 字段，未来若想换 gzip（例如为省依赖）**无需迁移数据**，读时按 `codec` 分支解压即可。

压缩格式（`snapshots.data` 解压后）：

```json
{
  "epoch": 1,
  "seq": 4821,
  "shapes": [ { "id": "sh_...", "kind": "freehand", "points": [], "...": "..." } ]
}
```

即**与 `resync.snapshot.payload.shapes` 完全同构**，服务端读快照后可直接切块下发，**无需二次转换**。

---

## 5. 迁移方式：启动时自动 migrate，SQL 内嵌

```
backend/internal/store/postgres/migrations/
  0001_init.sql
  0002_xxx.sql
```

```go
//go:embed migrations/*.sql
var migrationsFS embed.FS
```

**执行规则**（`migrate.go`，约 80 行，不引第三方迁移库）：

1. 启动时获取 **Postgres advisory lock** `pg_advisory_lock(0x636F6472)`（`codr`），避免多实例并发迁移。
2. `CREATE TABLE IF NOT EXISTS schema_migrations (version int PRIMARY KEY, applied_at timestamptz DEFAULT now(), checksum text)`。
3. 按文件名排序，跳过已应用的 `version`；**每个文件在独立事务内执行**（DDL 在 PG 中事务安全）。
4. 执行后写入 `schema_migrations`（含 `sha256` 校验和）；校验和不匹配 → **启动失败并报错**（防止有人手改已应用的文件）。
5. 释放锁，继续启动。

**为什么不用 golang-migrate / goose**：多一个依赖、多一套 CLI，而我们的迁移是"只增不删"的线性脚本，80 行足够，且能在 `go test` 里直接测。

**向前兼容约束**：迁移只做 `CREATE TABLE IF NOT EXISTS` / `ADD COLUMN IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS`；**禁止**在同一次发布中删除列（先停用，下个版本再删）。

---

## 6. 保留策略

| 数据 | 保留 | 清理方式 |
|---|---|---|
| `events`（热） | 每房间最近 **5000** 条 或 **30 天** | 后台任务：`DELETE FROM events WHERE room_code=$1 AND seq <= (SELECT last_seq-5000 ...)`，**且 seq 必须 ≤ 最新快照的 seq**（否则客户端无法补齐） |
| `events`（冷） | 可选归档到 `events_archive`（同构表，无索引） | P1 |
| `snapshots` | 每房间最近 **3** 个 | 插入新快照后删更早的 |
| `rooms` | `expires_at` 到期（默认 `ROOM_TTL=720h`） | 每日一次 `DELETE ... WHERE expires_at < now()`，级联删 events/snapshots |
| `members` | 随房间级联删除 | — |

> **删除事件的硬约束**：只能删 `seq <= latest_snapshot.seq` 的事件。快照之后的日志是增量补齐的唯一来源，删了就会让在线客户端无法追平。这条写进代码注释。

---

## 7. 与协议/架构的对应

| 协议概念 | 存储位置 |
|---|---|
| `room.code` / `title` / `epoch` / `lastActiveAt` | `rooms` |
| `joined.payload.room.seq` | `rooms.last_seq` |
| `resync.events.events[]` | `events` 按 `(room_code, seq)` 范围查 |
| `resync.snapshot.payload.shapes` | `snapshots` 最新一条解压 |
| `ack{duplicate}` | `uq_events_idem` 命中 |
| `readonly-token` 校验 | `rooms.viewer_key_hash`、`rooms.readonly_min_iat` |
| `BROKER=redis` 的 seq | Redis `INCR`，**不落** `rooms.last_seq` 作为权威（异步校正） |

---

## 8. 本地验证命令

```bash
# 起一个临时 PG（映射到 55432，避免占用本机 5432）
docker run --rm -d --name codraw-pg -e POSTGRES_PASSWORD=dev -p 55432:5432 postgres:16-alpine
sleep 5

# 应用 DDL（幂等，可重复执行）
psql "postgres://postgres:dev@localhost:55432/postgres" -v ON_ERROR_STOP=1 \
  -f docs/sql/0001_init.sql

# 验证索引被使用（先插一行房间）
psql "postgres://postgres:dev@localhost:55432/postgres" -v ON_ERROR_STOP=1 -c "
  INSERT INTO rooms (code, editor_key_hash, viewer_key_hash)
  VALUES ('K7M2QX', '\\x00', '\\x00');"
psql "postgres://postgres:dev@localhost:55432/postgres" -c \
  "EXPLAIN SELECT * FROM events WHERE room_code='K7M2QX' AND seq > 100 ORDER BY seq LIMIT 500;"
# 期望: Index Scan using events_pkey

# 清理
docker rm -f codraw-pg
```
