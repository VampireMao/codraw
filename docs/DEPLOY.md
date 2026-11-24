# 部署指南 · CoDraw（单文件二进制）

> 交付形态：**一个可执行文件**，前端已用 `go:embed` 编译进去。
> 不需要 Node、不需要 Nginx、不需要 Docker、不需要任何静态目录。

---

## 0. 构建产物

```bash
cd /mnt/d/Work/AI-TMP/codraw
make binaries          # 前端构建 → embed → 交叉编译
make verify-binaries   # 验证（Linux 沙箱运行 + 内嵌前端检查）
```

产物在 `dist/`：

| 文件 | 平台 | 大小 | 依赖 |
|---|---|---|---|
| `codraw-linux-amd64` | Linux x86_64 | 23 MB | **无**（静态链接） |
| `codraw-windows-amd64.exe` | Windows x64 | 24 MB | **无**（无 VC++ 运行时需求） |

两个产物都是 `CGO_ENABLED=0` + `-trimpath -ldflags "-s -w"`，完全静态链接。

已验证的交叉编译目标：`linux/amd64`、`linux/arm64`、`windows/amd64`、`darwin/amd64`、`darwin/arm64`。

---

## 1. 最简部署：直接跑

### Linux

```bash
chmod +x codraw-linux-amd64
./codraw-linux-amd64              # http://localhost:8080
./codraw-linux-amd64 -p 9000      # 换端口
```

默认 `STORE=memory BROKER=memory`，数据只在内存，重启即丢。**演示够用，生产不够。**

### Windows

```powershell
.\codraw-windows-amd64.exe
.\codraw-windows-amd64.exe -p 9000
```

首次运行如果被 SmartScreen 拦截：右键 → 属性 → 勾选「解除锁定」，或 `Unblock-File .\codraw-windows-amd64.exe`。

### 命令行参数

| 参数 | 说明 | 环境变量 |
|---|---|---|
| `-p, --port <n>` | HTTP 端口（默认 8080） | `PORT` |
| `--host <addr>` | 绑定地址（默认所有网卡） | `HTTP_HOST` |
| `--store <memory\|postgres>` | 持久化后端 | `STORE` |
| `--broker <memory\|redis>` | 跨实例广播 | `BROKER` |
| `--db <url>` | Postgres DSN | `DATABASE_URL` |
| `--redis <url>` | Redis URL | `REDIS_URL` |
| `--secret <s>` | 只读 token 密钥（≥32 字节） | `READONLY_SECRET` |
| `--base-url <url>` | 分享链接的公开域名 | `PUBLIC_BASE_URL` |
| `--origins <list>` | WS Origin 白名单 | `ALLOWED_ORIGINS` |
| `--log-level <l>` | 日志级别 | `LOG_LEVEL` |
| `--dev` / `--prod` | 开发 / 生产模式 | `APP_ENV` |

**参数优先于环境变量**。双击运行时只能用参数（没有 shell 设置环境变量），这是加参数的主要理由。

### 自带的健康检查子命令

二进制内置 `healthcheck`，供 systemd / 监控调用，无需 curl：

```bash
./codraw-linux-amd64 healthcheck -p 9000 && echo healthy
```

### 端口被占用

默认 8080 常被其他程序占用（例如 Steam 的 `steamwebhelper` 就监听 8080）。启动失败时二进制会直接告诉你怎么办：

```
codraw: fatal: cannot listen on :8080: the port is already in use by another program
  choose a free port:   PORT=8090 codraw
  or find the owner:    (Linux) ss -ltnp | grep 8080
                        (Windows) netstat -ano | findstr 8080
```

---

## 2. 生产部署（Linux + systemd）

### 2.1 准备目录与用户

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin codraw
sudo mkdir -p /opt/codraw /etc/codraw
sudo cp codraw-linux-amd64 /opt/codraw/codraw
sudo chmod 755 /opt/codraw/codraw
```

### 2.2 配置文件（权限 600，含密钥）

```bash
sudo tee /etc/codraw/codraw.env > /dev/null <<'EOF'
PORT=8080
APP_ENV=production
PUBLIC_BASE_URL=https://draw.example.com
ALLOWED_ORIGINS=https://draw.example.com
ALLOW_EMPTY_ORIGIN=false
STORE=postgres
BROKER=memory
DATABASE_URL=postgres://codraw:CHANGE_ME@127.0.0.1:5432/codraw?sslmode=require
READONLY_SECRET=CHANGE_ME_OPENSSL_RAND_BASE64_48
LOG_LEVEL=info
METRICS_TOKEN=CHANGE_ME_TOO
EOF
sudo chmod 600 /etc/codraw/codraw.env
```

生成密钥：

```bash
openssl rand -base64 48    # 填进 READONLY_SECRET
```

> **两个会让服务拒绝启动的配置**（这是有意的安全设计）：
> 1. `APP_ENV=production` 且 `READONLY_SECRET` 仍是示例值 → 拒绝启动
> 2. `APP_ENV=production` 且 `ALLOW_EMPTY_ORIGIN=true` → 拒绝启动（关闭了 CSWSH 防护）
>
> 启动失败时看 stderr，错误信息会直接告诉你是哪一项。

### 2.3 systemd 单元

```bash
sudo tee /etc/systemd/system/codraw.service > /dev/null <<'EOF'
[Unit]
Description=CoDraw realtime whiteboard
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=codraw
Group=codraw
WorkingDirectory=/opt/codraw
EnvironmentFile=/etc/codraw/codraw.env
# 参数也可以直接写在 ExecStart 里，两者可混用（参数优先）
ExecStart=/opt/codraw/codraw --port 8080 --prod --store postgres
Restart=always
RestartSec=2s
# 优雅停机：进程收到 SIGTERM 后先关连接再刷盘，预算 5s
TimeoutStopSec=10s

# 安全加固：单文件二进制不需要任何特权
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=false
SystemCallFilter=@system-service
SystemCallArchitectures=native

# 资源限制（可按机器调整）
LimitNOFILE=65536
MemoryMax=1G

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now codraw
sudo systemctl status codraw
```

### 2.4 反向代理（HTTPS + WebSocket）

Caddy 最简单（自动申请证书）：

```bash
sudo tee /etc/caddy/Caddyfile > /dev/null <<'EOF'
draw.example.com {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8080
}
EOF
sudo systemctl reload caddy
```

Nginx 版本（**注意 WebSocket 升级头，漏了会一直 400**）：

```nginx
server {
    listen 443 ssl http2;
    server_name draw.example.com;

    ssl_certificate     /etc/letsencrypt/live/draw.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/draw.example.com/privkey.pem;

    client_max_body_size 1m;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        # WebSocket 必需：
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        # 长连接不要过早超时：
        proxy_read_timeout  3600s;
        proxy_send_timeout  3600s;
    }
}

map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}
```

---

## 3. 多实例（横向扩容）

单文件的好处：多开几个进程就是多实例，不需要容器编排。

```bash
# 实例 1
PORT=8080 BROKER=redis STORE=postgres \
REDIS_URL=redis://127.0.0.1:6379/0 \
DATABASE_URL="postgres://...?sslmode=require" \
READONLY_SECRET="..." APP_ENV=production PUBLIC_BASE_URL=https://draw.example.com \
/opt/codraw/codraw

# 实例 2
PORT=8081 BROKER=redis STORE=postgres \
REDIS_URL=redis://127.0.0.1:6379/0 \
DATABASE_URL="postgres://...?sslmode=require" \
READONLY_SECRET="..." APP_ENV=production PUBLIC_BASE_URL=https://draw.example.com \
/opt/codraw/codraw
```

上游做负载均衡即可（Caddy 示例）：

```
draw.example.com {
    reverse_proxy 127.0.0.1:8080 127.0.0.1:8081
}
```

**硬性约束**：多实例必须 `BROKER=redis` + `STORE=postgres`。
- `STORE=memory` 时房间在各自进程堆里，实例 2 看不到实例 1 建的房间
- `BROKER=redis` 但 `STORE=memory` 时，`INCR` 分配 seq 后崩溃会留下永久空洞，无法从事件日志补齐

启动时如果检测到 `redis + memory` 组合，会打 WARN 日志。

---

## 4. 数据依赖

| 组件 | 必需性 | 说明 |
|---|---|---|
| Postgres 16+ | 生产必需 | 事件日志 + 快照。启动时自动建表（DDL 内嵌，幂等） |
| Redis 7+ | 多实例必需 | Pub/Sub 广播 + `INCRBY` 分配 seq。单实例可不用 |

**都不需要时**（`STORE=memory BROKER=memory`）二进制零依赖运行，适合演示。

---

## 5. 常见故障

| 现象 | 原因 | 处理 |
|---|---|---|
| 启动即退出，`READONLY_SECRET must be at least 32 bytes` | 生产模式没给密钥 | 加 `--secret "$(openssl rand -base64 48)"` |
| 启动即退出，`ALLOW_EMPTY_ORIGIN=true is not permitted` | 生产模式关闭了 Origin 校验 | 加 `--origins https://你的域名`，不要用 `ALLOW_EMPTY_ORIGIN` |
| `the port is already in use` | 端口被占用（8080 常被 Steam 等占用） | 加 `-p 9000` 换端口 |
| 页面能开但画不了 | WebSocket 被拒 | 检查 `--origins` 是否含实际域名；反代是否转发 `Upgrade` 头 |
| **复制链接显示成功但粘贴为空** | 通过 **HTTP + 非 localhost** 访问，`navigator.clipboard` 在非安全上下文不存在 | 用 HTTPS 部署（推荐），或确认复制时浏览器地址栏不是 `http://内网IP` |
| **PWA 无法安装 / Service Worker 不注册** | 同上，需安全上下文 | 同上，必须 HTTPS |
| 点「新建白板」返回 429 | 建房限流（默认 10 次/小时/IP） | 换网络，或调 `ROOM_CREATE_RATE_PER_HOUR` |
| 刷新后房间没了 | `STORE=memory` | 加 `--store postgres --db <url>` |
| 分享链接域名错 | `PUBLIC_BASE_URL` 没改 | 加 `--base-url https://你的域名` |
| 多实例看不到对方的数据 | `STORE=memory` | 加 `--store postgres` |
| 只读链接重启后失效 | 没给 `--secret`，用了临时密钥 | 固定 `--secret` |
| Windows 下双击闪退 | 配置错误，窗口关了看不到报错 | 在 PowerShell 里运行以保留输出 |

### 排查配置是否真的生效

二进制从环境变量读取配置，没有 `.env` 文件自动加载。确认方法：

```bash
# Linux
systemctl show codraw -p Environment

# Windows
Get-ChildItem Env: | Where-Object Name -match 'STORE|BROKER|PORT'
```

---

## 6. 面试当天的操作顺序

| 时间 | 动作 |
|---|---|
| T-30min | 启动服务并访问一次预热 |
| T-25min | 建好演示房间，画 2 个图形，刷新确认还在 |
| T-20min | 手机扫码进入，保持页面在前台 |
| T-15min | 本地再起一份（不同端口）作备份 |
| T-5min | 终端准备好 `healthcheck` 与建房间的 curl 命令 |
| T-0 | 开始演示 |

**兜底**：网络不稳就用本地 `http://localhost:8080`，说清"这正是我设计的零依赖单文件模式"。

---

## 7. 安全底线（上线前逐条确认）

- [ ] `READONLY_SECRET` 是随机生成的，不是示例值
- [ ] `ALLOW_EMPTY_ORIGIN=false`
- [ ] `ALLOWED_ORIGINS` 精确匹配实际域名（不要用 `*`，生产环境会拒绝启动）
- [ ] `METRICS_TOKEN` 已设置（否则 `/metrics` 与 `/ops` 对公网开放）
- [ ] `DATABASE_URL` 含 `sslmode=require`
- [ ] 二进制文件权限 `755`，配置文件 `600`，运行用户非 root
- [ ] 反向代理已转发 `Upgrade` 与 `Connection` 头
