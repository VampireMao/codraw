# 同绘 CoDraw

> 打开链接就能**多人同时画**的在线白板：PC 与手机同一套体验，断网续画，扫码入会，只读分享。

### 🚀 在线演示：**[codraw.wakepc.cn](https://codraw.wakepc.cn)**

打开即用，无需注册。建议用**两台设备**（电脑 + 手机）同时进入同一房间，体验实时协作与权限授权。

[![CI](https://github.com/VampireMao/codraw/actions/workflows/ci.yml/badge.svg)](https://github.com/VampireMao/codraw/actions/workflows/ci.yml)

| 指标 | 实测（同机压测，i5-12600K / 15GB / WSL2） |
|---|---|
| 单房间并发 | 100 客户端持续绘制，送达率 **100%**（150 万条广播） |
| 广播延迟 p95 | **39.4 ms**（Postgres + Redis 生产模式） |
| 广播延迟 p95（内存模式） | 31.1 ms |
| 单条 op 确认 | ack 14,978 / 发送 14,978，**零丢失** |
| 单文件产物 | Linux **23 MB** / Windows **24 MB**（静态链接，零运行时依赖） |
| 前端产物 | 385 KB（主包 gzip 45.5 KB） |
| 测试 | 后端 115 个函数 + 前端 **259** 个用例，全绿（含 `-race`） |

> 详细数据与复现命令见 [`docs/LOADTEST-RESULT.md`](docs/LOADTEST-RESULT.md)。**同机测试会争抢 CPU，公网 RTT 会更高**——两个方向都如实声明。

---

## 为什么值得看

1. **真·多人实时**：服务端权威事件日志 + 每房间单 goroutine，不是"本地先画再轮询"。
2. **真·断网续画**：离线 60s 的笔迹落在 IndexedDB，重连按 `seq` 增量补齐，**零丢失、零重复**。
3. **真·分布式**：多个实例经 Redis Pub/Sub 同步，客户端无感。
4. **真·只读分享**：HMAC 签名的限时只读链接，服务端强制拒绝写，不靠前端禁用按钮。
5. **真·单文件交付**：前端产物用 `go:embed` 编译进 Go 二进制，**一个文件即完整应用**，无需 Node、无需静态目录、无需容器。

设计取舍写在 [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) §5（ADR），协议契约在 [`docs/PROTOCOL.md`](docs/PROTOCOL.md)。

---

## 架构

```mermaid
flowchart LR
    B["浏览器<br/>Vue 3 + Canvas 2D + PWA"] -->|"HTTP /api"| A["Go 单二进制 :8080"]
    B <-->|"WS /ws（JSON 信封 + seq）"| A
    A --> H["hub · 房间注册表"]
    H --> R["room · 单 goroutine<br/>seq 分配 / 扇出 / 背压"]
    R --> S[("Postgres 16<br/>events / snapshots")]
    R --> K[("Redis 7<br/>Pub/Sub + INCR seq")]
    A --> C["go:embed<br/>前端静态产物"]
```

详细的模块依赖、并发所有权、背压与停机顺序见 [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)。

---

## 快速开始

### 方式零：直接用在线演示（最快）

打开 **<https://codraw.wakepc.cn>**，点「新建白板」即可。无需安装、无需注册。

### 方式一：直接用发布产物（零依赖）

下载对应平台的单个文件，双击/执行即可，**不需要 Node、不需要 Docker、不需要任何配置文件**：

```bash
# Linux
chmod +x codraw-linux-amd64
./codraw-linux-amd64

# Windows（PowerShell 或直接双击）
.\codraw-windows-amd64.exe
```

打开 <http://localhost:8080> 即可使用。默认 `STORE=memory BROKER=memory`，重启即丢数据（演示够用）。

### 命令行参数

```
codraw [flags]
codraw healthcheck [-p PORT]
codraw version
```

| 参数 | 说明 | 对应环境变量 |
|---|---|---|
| `-p, --port <n>` | HTTP 端口（默认 8080） | `PORT` |
| `--host <addr>` | 绑定地址（默认所有网卡） | `HTTP_HOST` |
| `--store <memory\|postgres>` | 房间持久化 | `STORE` |
| `--broker <memory\|redis>` | 跨实例广播 | `BROKER` |
| `--db <url>` | Postgres 连接串 | `DATABASE_URL` |
| `--redis <url>` | Redis 地址 | `REDIS_URL` |
| `--secret <s>` | 只读 token 的 HMAC 密钥（≥32 字节） | `READONLY_SECRET` |
| `--base-url <url>` | 分享链接用的公开域名 | `PUBLIC_BASE_URL` |
| `--origins <list>` | WebSocket Origin 白名单，逗号分隔 | `ALLOWED_ORIGINS` |
| `--log-level <l>` | `debug\|info\|warn\|error` | `LOG_LEVEL` |
| `--dev` / `--prod` | 开发模式（宽松）/ 生产模式（严格） | `APP_ENV` |
| `-h, --help` | 显示帮助 | — |
| `-v, --version` | 显示版本 | — |

**参数优先于环境变量**，两者可以混用。

### 常见用法

```bash
# 换个端口（8080 被占用时最常用）
./codraw-linux-amd64 -p 9000

# 只允许本机访问
./codraw-linux-amd64 --host 127.0.0.1 -p 9000

# 生产模式 + 持久化（一条命令起完整服务）
./codraw-linux-amd64 --prod --port 80 \
  --base-url https://draw.example.com \
  --origins https://draw.example.com \
  --secret "$(openssl rand -base64 48)" \
  --store postgres --db "postgres://user:pass@127.0.0.1:5432/codraw?sslmode=require"

# 探活（不依赖 curl）
./codraw-linux-amd64 healthcheck -p 9000 && echo healthy
```

```powershell
# Windows 等价写法
.\codraw-windows-amd64.exe -p 9000
.\codraw-windows-amd64.exe --prod --port 8080 --base-url https://draw.example.com `
  --secret "用 openssl rand -base64 48 生成" --store postgres `
  --db "postgres://user:pass@host:5432/codraw?sslmode=require"
.\codraw-windows-amd64.exe healthcheck -p 9000
```

> **只读链接的密钥**：不提供 `--secret` 时，开发模式会自动生成一个临时密钥并打印警告（只读链接重启后失效）。生产模式（`--prod`）**必须**显式提供，否则拒绝启动——这是防止任何人拿到仓库就能伪造只读链接。

### 方式二：从源码构建双平台单文件

```bash
cd /mnt/d/Work/AI-TMP/codraw
make binaries      # 前端构建 + embed + 交叉编译
make verify-binaries

ls -lh dist/
# codraw-linux-amd64        23 MB
# codraw-windows-amd64.exe  24 MB
```

`make binaries` 会依次完成：Vue 构建 → 产物拷入 `backend/internal/webui/dist` → `CGO_ENABLED=0` 交叉编译两个平台。产物是**完全静态链接**的，不依赖 libc。

### 方式三：本地开发（热更新）

```bash
make dev-backend   # 终端 A：:8080，STORE=memory BROKER=memory
make dev-web       # 终端 B：:5173，代理 /api 与 /ws
```

### 跨实例演示

```bash
# 实例 1
PORT=8080 BROKER=redis STORE=postgres REDIS_URL=redis://localhost:6379/0 ./codraw-linux-amd64
# 实例 2（不同端口）
PORT=8081 BROKER=redis STORE=postgres REDIS_URL=redis://localhost:6379/0 ./codraw-linux-amd64
```

两个实例经 Redis Pub/Sub 同步，客户端连哪个都能看到同一块白板。

---

## 常用命令

```bash
make help            # 全部目标
make binaries        # ★ 双平台单文件产物到 dist/
make verify-binaries # 验证产物（Linux 沙箱运行 + 内嵌前端检查）
make test            # go vet + go test -race + 前端测试
make lint            # gofmt + vet + 前端 lint
make verify          # ★ 全部验收（单测 + E2E + 权限 + 重进）
make verify-e2e      # 协议级 E2E（61 断言）
make verify-permissions # 权限模型 P1–P12
make verify-ddl      # docs/sql 的 DDL 幂等验证
make build           # 当前平台构建
make dev-backend     # 本地开发后端
make dev-web         # 本地开发前端
make loadtest        # 压测
```

---

## 目录结构

```
codraw/
├── dist/                    # ★ 构建产物（双平台单文件，git 忽略）
│   ├── codraw-linux-amd64
│   └── codraw-windows-amd64.exe
├── backend/                 # Go
│   ├── cmd/server/          # main：装配 store/broker/hub/api
│   └── internal/
│       ├── api/             # REST + 静态托管 + /ops
│       ├── broker/          # memory | redis 广播
│       ├── hub/             # 房间注册表与生命周期
│       ├── room/            # 单 goroutine 事件循环（核心）
│       ├── store/           # memory | postgres
│       ├── ws/              # 握手、帧编解码、心跳
│       ├── webui/           # ★ go:embed dist（前端编译进二进制）
│       └── protocol/        # 消息结构 + 校验（零依赖）
├── web/                     # Vue 3 + TS + Vite + Pinia
├── tools/loadtest/          # 独立压测器（Go）
├── docs/
│   ├── PRD.md               # 产品需求
│   ├── UX-SPEC.md           # 交互与视觉规范
│   ├── ARCHITECTURE.md      # 架构与 ADR
│   ├── PROTOCOL.md          # ★ 协议契约（唯一事实源）
│   ├── PERMISSIONS.md       # ★ 权限模型（房主/授权/继承）
│   ├── DATA-MODEL.md        # 数据模型
│   ├── DECISIONS.md         # 13 条取舍
│   ├── LOADTEST-RESULT.md   # 真实压测数据
│   ├── ACCEPTANCE.md        # 独立验收报告
│   ├── REVIEW.md            # 整体评审
│   ├── QA.md                # 缺陷记录与回归
│   ├── DEPLOY.md            # 部署总览
│   ├── DEPLOY-SYSTEMD.md    # 云服务器部署手册
│   ├── DEPLOY-CHECKLIST.md  # 上线检查清单
│   ├── INTERVIEW.md         # 面试讲稿
│   └── sql/0001_init.sql    # DDL（已验证幂等）
├── scripts/                 # 构建与验收脚本
├── Makefile
└── README.md
```

---

## 文档索引

| 文档 | 读者 | 内容 |
|---|---|---|
| [PRD](docs/PRD.md) | 所有人 | 功能范围、验收标准、里程碑 |
| [UX-SPEC](docs/UX-SPEC.md) | 前端 | 交互细节、视觉令牌、布局规范 |
| [ARCHITECTURE](docs/ARCHITECTURE.md) | 工程 | 全景图、并发模型、背压、容量分析、ADR |
| [PROTOCOL](docs/PROTOCOL.md) | 前后端 | **字段级契约**、seq 语义、重连时序、错误码 |
| [PERMISSIONS](docs/PERMISSIONS.md) | 前后端 | **权限模型**：房主独占、成员授权、断线继承 |
| [DATA-MODEL](docs/DATA-MODEL.md) | 后端 | DDL、索引、快照压缩、迁移与保留 |
| [DECISIONS](docs/DECISIONS.md) | 所有人 | **13 条产品与架构取舍**，每条含被否决方案 |
| [LOADTEST-RESULT](docs/LOADTEST-RESULT.md) | 所有人 | **真实压测数据**，含 124 倍优化前后对比 |
| [ACCEPTANCE](docs/ACCEPTANCE.md) | 所有人 | 独立验收报告，含发现的真实缺陷 |
| [REVIEW](docs/REVIEW.md) | 所有人 | 整体评审：交付结论、问题清单、行动项 |
| [QA](docs/QA.md) | 所有人 | 缺陷记录、根因分析、回归验证 |
| [DEPLOY](docs/DEPLOY.md) | 运维 | 三种上线方案（云平台/自建/隧道） |
| [DEPLOY-SYSTEMD](docs/DEPLOY-SYSTEMD.md) | 运维 | **云服务器手册**：用户、systemd、nginx、故障排查 |
| [DEPLOY-CHECKLIST](docs/DEPLOY-CHECKLIST.md) | 运维 | 上线前逐项检查 |
| [INTERVIEW](docs/INTERVIEW.md) | 求职者 | 讲稿、10 个高频追问应答、简历写法 |

---

## 已知取舍

- 并发编辑同一图形为 **LWW**，不做 CRDT：白板场景冲突极少，换来实现与调试成本大幅下降。
- 多实例用 **Redis `INCR` + Pub/Sub**，不做选主：极端乱序由客户端 `resync` 兜底。
- 离线队列**有上限**：宁可明确提示，也不做无界缓存导致 OOM。
- 房间码 **31 字符集**（排除 `0 O 1 I L`），约 8.87 亿组合。
- 页面重载后拉**全量快照**而非增量：本地只持久化了 seq 与 pending，没持久化图形；
  存储图形要处理版本一致性，成本远高于每次几 KB 的快照。

---

## 部署

生产部署（systemd + nginx + PostgreSQL）见 **[docs/DEPLOY-SYSTEMD.md](docs/DEPLOY-SYSTEMD.md)**。

```bash
# 最小启动（零依赖，内存存储，重启即丢）
./codraw-linux-amd64 -p 8080

# 生产模式（必须显式提供密钥与域名，否则拒绝启动）
./codraw-linux-amd64 --prod --port 8080 \
  --base-url https://draw.example.com \
  --origins https://draw.example.com \
  --secret "$(openssl rand -base64 48)" \
  --store postgres --db "postgres://user:pass@127.0.0.1:5432/codraw?sslmode=disable"
```

---

## 复现压测

```bash
# 1. 以生产模式启动单实例
STORE=postgres BROKER=redis APP_ENV=development ALLOW_EMPTY_ORIGIN=true \
DATABASE_URL="postgres://..." REDIS_URL="redis://..." \
READONLY_SECRET="loadtest-secret-at-least-32-bytes" \
PORT=8080 ./dist/codraw-linux-amd64 &

# 2. 跑 S1：单房间 100 并发绘制
cd tools/loadtest && go build -o /tmp/lt .
/tmp/lt --scenario s1 --base http://127.0.0.1:8080 \
  --clients 100 --rate 5 --duration 30s --ramp 4s
```

数字与优化过程见 **[docs/LOADTEST-RESULT.md](docs/LOADTEST-RESULT.md)**。

---

## License

[MIT](LICENSE) © 2026 BH8GLK
