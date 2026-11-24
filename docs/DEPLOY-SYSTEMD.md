# 云服务器部署手册（systemd + 二进制）

> 适用于：已把 `codraw-linux-amd64` 上传到 `/opt/codraw/`
> nginx 与 SSL 由你自行配置，本文只给出**必须的代理要点**
> 环境假设：Ubuntu 22.04 / 24.04（Debian 系同理）

---

## 0. 开始前先确认

```bash
# 确认架构匹配（必须是 x86_64）
uname -m

# 确认二进制可执行
/opt/codraw/codraw-linux-amd64 version
```

**如果报 `Permission denied`**：说明上传后丢了可执行位，见 §1 第 3 步。

**如果报 `cannot execute binary file`**：架构不对（比如传了 arm 机器），需要重新交叉编译。

---

## 1. 创建专用用户并放置文件

### 1.1 创建系统用户

```bash
sudo useradd \
  --system \
  --no-create-home \
  --shell /usr/sbin/nologin \
  --comment "CoDraw whiteboard service" \
  codraw
```

验证：

```bash
id codraw
# 期望：uid=... (codraw) gid=... (codraw) groups=...,65534(nogroup)
```

> **为什么用系统用户**：`--system` 表示非登录账号，`--shell nologin` 防止有人用它登进来。
> 服务不需要 shell，也不需要 home 目录。

### 1.2 整理目录与权限

```bash
# 目录：root 拥有，codraw 只读
sudo chown root:root /opt/codraw
sudo chmod 755 /opt/codraw

# 二进制：可执行，属主 root（服务以 codraw 身份运行，只需读+执行）
sudo mv /opt/codraw/codraw-linux-amd64 /opt/codraw/codraw
sudo chown root:root /opt/codraw/codraw
sudo chmod 755 /opt/codraw/codraw

# 配置目录：独立于 /opt，便于备份与权限分离
sudo mkdir -p /etc/codraw
sudo chown root:root /etc/codraw
sudo chmod 750 /etc/codraw
```

**关键权限说明**：

| 路径 | 属主 | 权限 | 理由 |
|---|---|---|---|
| `/opt/codraw/codraw` | root | 755 | 服务只读执行，防止被篡改 |
| `/etc/codraw/codraw.env` | root | **600** | 内含 HMAC 密钥，绝不能组/其他人可读 |

验证二进制：

```bash
/opt/codraw/codraw version
# 期望输出：codraw 1.0.0 (built ...)
```

---

## 2. 生成配置

### 2.1 生成密钥

```bash
openssl rand -base64 48
```

复制输出备用（下面要填进配置）。

### 2.2 写入环境文件

```bash
sudo tee /etc/codraw/codraw.env > /dev/null <<'EOF'
# ---------- 进程 ----------
PORT=8080
# 只监听回环：nginx 在同一台机器，公网访问必须经过 nginx（含 HTTPS）
HTTP_HOST=127.0.0.1
APP_ENV=production
LOG_LEVEL=info

# ---------- 对外地址（必须与浏览器地址栏完全一致，含协议）----------
PUBLIC_BASE_URL=https://draw.example.com
# WebSocket Origin 白名单；多个用逗号分隔，不要用 *
ALLOWED_ORIGINS=https://draw.example.com
ALLOW_EMPTY_ORIGIN=false

# ---------- 存储 ----------
# 单实例演示用 memory；要持久化改成 postgres 并填 DATABASE_URL
STORE=memory
BROKER=memory

# ---------- 安全 ----------
# 必填，≥32 字节。填上面 openssl 的输出
READONLY_SECRET=在这里粘贴openssl生成的48字节随机串
# 保护 /metrics 与 /ops 面板
METRICS_TOKEN=另生成一个随机串

# ---------- 容量 ----------
MAX_CONN_PER_ROOM=200
MAX_CONN_TOTAL=2000
# 建房限流：每 IP 每小时
ROOM_CREATE_RATE_PER_HOUR=10
ROOM_CREATE_BURST=10
EOF

# 立刻锁死权限
sudo chmod 600 /etc/codraw/codraw.env
sudo chown root:root /etc/codraw/codraw.env
```

### 2.3 三个必须改的地方

| 变量 | 改成什么 | 不改会怎样 |
|---|---|---|
| `PUBLIC_BASE_URL` | 你的真实 HTTPS 域名 | **启动失败**（production 强制校验）|
| `ALLOWED_ORIGINS` | 同上（可加多个） | WebSocket 握手被拒，画不了 |
| `READONLY_SECRET` | `openssl rand -base64 48` 的输出 | **启动失败**（拒绝示例值/过短）|

> 这三条是代码里的硬校验，不是我加的保险。`APP_ENV=production` 时缺一项就直接拒绝启动，并打印具体是哪一项。

### 2.4 先手工验证配置能起来

**这一步很重要**：在配 systemd 之前先手工跑一次，出错能直接看到原因。

```bash
sudo -u codraw env $(sudo cat /etc/codraw/codraw.env | grep -v '^#' | xargs) \
  /opt/codraw/codraw
```

期望输出：

```json
{"level":"INFO","msg":"starting codraw","env":"production",...}
{"level":"INFO","msg":"listening","addr":"127.0.0.1:8080","startup_ms":2}
```

另开一个终端验证：

```bash
curl -s http://127.0.0.1:8080/healthz
# {"status":"ok"}
curl -s http://127.0.0.1:8080/readyz
# {"broker":"memory","status":"ready","store":"memory"}
```

按 `Ctrl+C` 停止，确认日志出现 `shutdown complete`（优雅停机生效）。

**如果报 `invalid configuration`**：错误信息会直接指出哪个变量有问题，改完重试。

---

## 3. systemd 服务

### 3.1 单元文件

```bash
sudo tee /etc/systemd/system/codraw.service > /dev/null <<'EOF'
[Unit]
Description=CoDraw realtime collaborative whiteboard
Documentation=https://github.com/yourname/codraw
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=codraw
Group=codraw
WorkingDirectory=/opt/codraw
EnvironmentFile=/etc/codraw/codraw.env
ExecStart=/opt/codraw/codraw

# 自动重启
Restart=always
RestartSec=2s

# 优雅停机：进程收到 SIGTERM 后先关连接再退出，预算 5s
KillSignal=SIGTERM
TimeoutStopSec=10s

# ---------- 安全加固 ----------
# 服务不需要任何特权
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
RestrictSUIDSGID=true
RestrictRealtime=true
RestrictNamespaces=true
LockPersonality=true
# 只允许 IPv4/IPv6 socket（不需要 unix socket 通信）
RestrictAddressFamilies=AF_INET AF_INET6
SystemCallFilter=@system-service
SystemCallArchitectures=native

# 整个文件系统只读，除了 /tmp（PrivateTmp 已隔离）
ReadWritePaths=

# ---------- 资源限制 ----------
LimitNOFILE=65536
MemoryMax=1G
TasksMax=512

[Install]
WantedBy=multi-user.target
EOF
```

### 3.2 关于 `ProtectSystem=strict` 的说明

这个选项让**整个文件系统对服务只读**，只有 `/tmp` 和 `/dev` 等例外。

对本项目是安全的，因为：
- 前端产物已 `go:embed` 进二进制，**不需要读磁盘上的静态文件**
- `STORE=memory` 时不写磁盘
- 日志走 stdout，由 journald 收集

**如果你以后改成 `STORE=postgres`**：仍然没问题，数据库连接走网络，不写本地磁盘。

> 唯一的例外：如果你想让服务写日志文件（不建议），需要加 `ReadWritePaths=/var/log/codraw`。

### 3.3 启动

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now codraw
sudo systemctl status codraw --no-pager
```

期望看到 `Active: active (running)`。

**首次启动失败时看日志**：

```bash
sudo journalctl -u codraw -n 30 --no-pager
```

常见错误对照：

| 日志内容 | 原因 | 处理 |
|---|---|---|
| `READONLY_SECRET must be at least 32 bytes` | 密钥没填或太短 | 重新生成并填入 |
| `READONLY_SECRET is still the example value` | 忘了替换 | 同上 |
| `PUBLIC_BASE_URL is required` | 没配或为空 | 填真实域名 |
| `ALLOW_EMPTY_ORIGIN=true is not permitted` | 误开了 | 改 `false` |
| `listen on 127.0.0.1:8080: bind: address already in use` | 端口被占 | `ss -ltnp \| grep 8080` 找占用者 |
| `permission denied` | 二进制没有执行位 | `sudo chmod 755 /opt/codraw/codraw` |

### 3.4 常用运维命令

```bash
sudo systemctl status codraw        # 状态
sudo systemctl restart codraw       # 重启
sudo systemctl stop codraw          # 停止
sudo journalctl -u codraw -f        # 实时日志
sudo journalctl -u codraw --since today   # 今日日志
```

---

## 4. nginx 必须注意的 4 点

你说 nginx 自己处理，但这 4 点漏了会出问题，提前说明：

### 4.1 WebSocket 升级头（漏了就一直 400）

```nginx
proxy_http_version 1.1;
proxy_set_header Upgrade    $http_upgrade;
proxy_set_header Connection "upgrade";
```

**这是最常见的坑**：漏了 `Upgrade`/`Connection`，WebSocket 握手直接失败，页面能打开但**画不了**。

### 4.2 超时时间要长

```nginx
proxy_read_timeout  3600s;
proxy_send_timeout  3600s;
```

WebSocket 是长连接，默认 60s 会被 nginx 掐断，表现为「画着画着断线重连」。

### 4.3 传递真实 IP（限流依赖它）

```nginx
proxy_set_header X-Real-IP         $remote_addr;
proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;
proxy_set_header Host              $host;
```

同时**在环境文件里加**：

```bash
TRUST_PROXY_HEADERS=true
```

不加的话，所有请求的 IP 都是 `127.0.0.1`，建房限流会把所有人当成同一个人（每小时只能建 10 个房间）。

### 4.4 请求体大小

```nginx
client_max_body_size 1m;
```

单条 WS 消息上限 64KB，1m 足够。

### 完整参考配置

```nginx
server {
    listen 443 ssl http2;
    server_name draw.example.com;

    ssl_certificate     /etc/letsencrypt/live/draw.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/draw.example.com/privkey.pem;

    # 安全响应头（后端已发 X-Content-Type-Options，这里补其余）
    add_header X-Frame-Options        "SAMEORIGIN" always;
    add_header Referrer-Policy        "strict-origin-when-cross-origin" always;

    client_max_body_size 1m;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;

        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # WebSocket 必需
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection "upgrade";

        # 长连接不要过早超时
        proxy_read_timeout  3600s;
        proxy_send_timeout  3600s;
        proxy_buffering     off;
    }
}

server {
    listen 80;
    server_name draw.example.com;
    return 301 https://$host$request_uri;
}
```

---

## 5. 部署后验证清单

按顺序做，每步都要过：

```bash
# 1. 服务在跑
sudo systemctl is-active codraw
# active

# 2. 本地健康检查
curl -s http://127.0.0.1:8080/healthz
# {"status":"ok"}

# 3. 就绪检查（含依赖）
curl -s http://127.0.0.1:8080/readyz
# {"broker":"memory","status":"ready","store":"memory"}

# 4. 内置探针（不依赖 curl）
sudo -u codraw /opt/codraw/codraw healthcheck
# exit=0

# 5. 经 nginx 的 HTTPS 访问
curl -sI https://draw.example.com/healthz | head -1
# HTTP/2 200

# 6. 前端能加载
curl -s https://draw.example.com/ | grep -o '<title>[^<]*</title>'
# <title>同绘 CoDraw</title>

# 7. 建房接口（注意 201）
curl -s -X POST https://draw.example.com/api/rooms \
  -H 'Content-Type: application/json' -d '{"title":"部署测试"}' \
  | head -c 200
# {"room":{"code":"XXXXXX",...
```

**第 8 步必须用浏览器做**（curl 测不了 WebSocket）：

1. 打开 `https://draw.example.com`
2. 点「新建白板」→ 应该立刻进房间，左上角显示房号
3. 画一笔 → 应该能画（你是房主）
4. 复制分享链接 → 手机 4G 打开 → 应该能进、能看、默认只读

**如果第 3 步画不了**：99% 是 nginx 漏了 WebSocket 升级头，见 §4.1。

---

## 6. 升级与回滚

### 升级

```bash
# 1. 备份旧版本
sudo cp /opt/codraw/codraw /opt/codraw/codraw.bak.$(date +%F)

# 2. 上传新二进制到临时位置，再原子替换
sudo mv /tmp/codraw-linux-amd64 /opt/codraw/codraw.new
sudo chmod 755 /opt/codraw/codraw.new
sudo mv /opt/codraw/codraw.new /opt/codraw/codraw   # mv 是原子的

# 3. 重启
sudo systemctl restart codraw
sudo systemctl status codraw --no-pager
```

> 用 `mv` 而不是 `cp` 覆盖：`mv` 在同一文件系统内是原子操作，避免服务正在执行时文件被写坏。

### 回滚

```bash
sudo mv /opt/codraw/codraw.bak.2026-11-24 /opt/codraw/codraw
sudo systemctl restart codraw
```

---

## 7. 常见故障速查

| 现象 | 原因 | 处理 |
|---|---|---|
| 服务起不来，日志说配置无效 | 三个必填项之一没改 | 见 §3.3 对照表 |
| **页面能开，但所有人（含房主）都是只读** | **`PUBLIC_BASE_URL`/`ALLOWED_ORIGINS` 的域名与实际访问域名不一致**（如配 `.com` 实际访问 `.cn`，或配 `http://` 实际走 `https://`） | 见下方「最常见的部署事故」 |
| 页面能开，但画不了 | nginx 没转发 WebSocket 升级头 | 见 §4.1 |
| 画着画着断线重连 | nginx 超时太短 | 见 §4.2 |
| 所有人都被限流 | 没传真实 IP | 见 §4.3 |
| 分享链接域名错 | `PUBLIC_BASE_URL` 不对 | 改成真实域名 |
| 复制房间号失败 | 非 HTTPS 访问 | 必须走 HTTPS（浏览器安全上下文限制）|
| 手机装不上 PWA | 同上 | 必须 HTTPS |
| 重启后内容全丢 | `STORE=memory` | 改 `postgres` 并配 `DATABASE_URL` |

### 🔴 最常见的部署事故：域名/协议不一致

**这是本项目最容易踩、且症状最迷惑的坑。**

`PUBLIC_BASE_URL` 和 `ALLOWED_ORIGINS` 必须与**浏览器地址栏里的完全一致**，包括：

- 域名后缀（`.com` vs `.cn` vs `.net`）——**最难发现，因为长得太像**
- 协议（`http://` vs `https://`）
- 端口（默认 443 不写）
- 是否带 `www.`

**配错时的症状**：页面能正常打开，但**所有人（包括房主）都显示只读**。

原因链路：

```
Origin 不匹配 → WebSocket 握手 403 → 前端收不到 joined
→ 房间状态没建立 → role 保持默认 viewer → UI 显示只读
```

**排查方法**：

```bash
# 1. 看服务端记的是哪个域名
sudo journalctl -u codraw -n 20 --no-pager | grep -E 'public origin|allowlist'

# 2. 直接测 WS 握手，错误信息会同时给出两边的值
CODE=你的房间号
curl -s -m 10 \
  -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" \
  -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
  -H "Origin: https://你实际访问的域名" \
  "https://你的域名/ws?room=$CODE&cid=devprobe01"
```

期望返回 `101 Switching Protocols`。

如果返回：

```json
{"error":{"code":"forbidden","message":"origin not allowed: got \"https://codraw.wakepc.cn\", allowed \"https://codraw.wakepc.com,...\""}}
```

**`got` 和 `allowed` 的差别就是问题所在**，改 `/etc/codraw/codraw.env` 后重启即可。

**正确配置示例**（访问 `https://codraw.example.cn`）：

```bash
PUBLIC_BASE_URL=https://codraw.example.cn
ALLOWED_ORIGINS=https://codraw.example.cn
```

> 需要多个域名（如同时支持 `.com` 和 `.cn`）时用逗号分隔：
> `ALLOWED_ORIGINS=https://a.com,https://b.cn`
> 但 `PUBLIC_BASE_URL` 只能填一个——它决定分享链接里用哪个域名。

---

## 8. 需要持久化时（可选）

如果你想重启后内容还在：

```bash
# 1. 装 Postgres
sudo apt install -y postgresql

# 2. 建库建用户
sudo -u postgres psql <<'SQL'
CREATE USER codraw WITH PASSWORD '换成强密码';
CREATE DATABASE codraw OWNER codraw;
SQL

# 3. 改环境文件
sudo sed -i 's/^STORE=.*/STORE=postgres/' /etc/codraw/codraw.env
sudo tee -a /etc/codraw/codraw.env <<'EOF'
DATABASE_URL=postgres://codraw:换成强密码@127.0.0.1:5432/codraw?sslmode=disable
EOF
# 注意：本机连接可用 sslmode=disable；跨机器必须 require
sudo chmod 600 /etc/codraw/codraw.env

# 4. 重启（表会自动创建，DDL 内嵌在二进制里）
sudo systemctl restart codraw

# 5. 验证
curl -s http://127.0.0.1:8080/readyz
# {"broker":"memory","status":"ready","store":"postgres"}
```

> 建表不需要手工执行 SQL：迁移文件已 `go:embed` 进二进制，启动时自动跑，且幂等。

---

## 9. 一页速查

```bash
# 建用户
sudo useradd --system --no-create-home --shell /usr/sbin/nologin codraw

# 放文件
sudo mv /opt/codraw/codraw-linux-amd64 /opt/codraw/codraw
sudo chmod 755 /opt/codraw/codraw && sudo chown root:root /opt/codraw/codraw

# 配密钥
sudo mkdir -p /etc/codraw
openssl rand -base64 48   # 复制到下面的 READONLY_SECRET
sudo nano /etc/codraw/codraw.env   # 按 §2.2 填写
sudo chmod 600 /etc/codraw/codraw.env

# 手工验证
sudo -u codraw env $(sudo cat /etc/codraw/codraw.env | grep -v '^#' | xargs) /opt/codraw/codraw

# 装服务
sudo nano /etc/systemd/system/codraw.service   # 按 §3.1 粘贴
sudo systemctl daemon-reload && sudo systemctl enable --now codraw
sudo systemctl status codraw --no-pager

# 验证
curl -s http://127.0.0.1:8080/healthz
```
