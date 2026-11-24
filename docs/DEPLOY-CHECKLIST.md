# 上线清单 · CoDraw

> PM 维护。D2 下午部署窗口按此逐条执行，每项都要「做过」而不是「打算做」。

## 1. 部署前（本地必须全绿）

- [ ] `make test` 全绿（后端 `go vet` + `go test -race`，前端 typecheck + build + test）
- [ ] `make binaries` 构建成功，`dist/` 下有 Linux 与 Windows 两个文件
- [ ] `make verify-binaries` 通过（沙箱运行 + 内嵌前端检查）
- [ ] `dist/codraw-linux-amd64 healthcheck` 退出码 0
- [ ] `curl localhost:8080/healthz` → `{"status":"ok"}`
- [ ] `curl localhost:8080/readyz` → `status: ready`（Postgres/Redis 都连上）
- [ ] 二进制以非 root 用户运行（systemd `User=codraw`）
- [ ] `.env` 里 `READONLY_SECRET` 已换成随机值（`openssl rand -base64 48`）
- [ ] `ALLOW_EMPTY_ORIGIN=false`
- [ ] `PUBLIC_BASE_URL` 与实际访问域名一致（否则分享链接错）
- [ ] **建房限流**：默认 `ROOM_CREATE_RATE_PER_HOUR=10`（每 IP 每小时 10 个房间）。演示前若已反复建过房间，调高或换网络，否则面试时点「新建白板」会收到 429
- [ ] **配置生效检查**：`systemctl show codraw -p Environment`（Linux）或 `Get-ChildItem Env:`（Windows）确认变量已生效。二进制不自动读 `.env` 文件，必须由 systemd `EnvironmentFile` 或 shell 注入
- [ ] 仓库里没有 `.env`、没有密钥、没有 sudo 密码：`git status` 干净

## 2. 首次上线

- [ ] 选平台并记录：Koyeb / Fly.io / 自建 VPS（三选一，见 §4）
- [ ] 设置环境变量（**逐项对照 `.env.example`，不要漏 `READONLY_SECRET`**）
- [ ] 部署后拿到公网 URL
- [ ] 用**手机 4G**（不是公司 WiFi）打开，验证：
  - [ ] 首屏 < 3s
  - [ ] 新建房间可用
  - [ ] 与笔记本实时同步
  - [ ] 「添加到主屏幕」后可独立打开（PWA）
- [ ] 检查响应头：`curl -I https://<host>` 有 `content-security-policy`、`x-content-type-options`
- [ ] 确认 HTTPS 有效（免费平台默认有；自建用 Caddy 自动签发）

## 3. 演示前 30 分钟（关键，别省）

- [ ] 打开线上地址预热（免费层会休眠，第一个请求可能 10s+）
- [ ] 建好演示房间，画 2 个图形，确认恢复后内容还在
- [ ] 手机提前扫码进入，**保持页面在前台**
- [ ] 本地再起一个实例（不同端口）作为备份（断网时切本地，讲"这正是我设计的零依赖单文件模式"）
- [ ] 录屏文件在桌面，文件名清晰
- [ ] 终端窗口准备好：`make up2`、`make ops`、`curl` 建房间三条命令提前打好

## 4. 平台选择（按推荐顺序）

| 平台 | 免费层 | 优点 | 坑 |
|---|---|---|---|
| **Koyeb** | 1 个 Web 服务 + Postgres 小实例 | 支持 WebSocket、Docker 部署、有免费 Postgres | 免费实例会缩容到 0，需预热 |
| **Fly.io** | 3 个 256MB 实例 | 支持多实例、内网、真正能演示横向扩容 | 需绑卡；免费额度需留意 |
| **自建 VPS + Caddy** | 无 | 完全可控，能跑多实例 | 要买机器、配域名 |

> **建议**：主用 Koyeb（够快、够简单），面试前把多实例演示放本地两个进程，讲清"线上单实例、本地演示两实例"——诚实且不影响说服力。

### Cloudflare Tunnel 备选（网络受限时）

```bash
# 本地 compose 起好后
cloudflared tunnel --url http://localhost:8080
# 得到 https://xxx.trycloudflare.com，可直接给面试官
```

优点：零成本、HTTPS、不需要公网 IP。缺点：URL 随机、依赖本地网络。**作为兜底方案写在演示卡片上。**

## 5. 上线后 24 小时观察

- [ ] `/ops` 无异常增长（连接数、房间数）
- [ ] 日志无 panic、无 `error` 刷屏
- [ ] `codraw_dropped_connections_total` 为 0 或极低（高说明有慢消费者或网络问题）
- [ ] `codraw_ops_rejected_total{reason="forbidden"}` 有值说明只读链路被真实使用
- [ ] 内存稳定（无明显泄漏）：`docker stats`

## 6. 出问题时的降级顺序

1. 线上挂了 → 切 `cloudflared` 本地穿透，地址换成隧道 URL 给面试官
2. 隧道也不行 → 本地 `http://<局域网IP>:8080`，让面试官连同一 WiFi
3. 都不行 → 放录屏，边播边讲架构；**不要现场 debug**，那是面试事故

## 7. 明确不做（避免 scope creep）

- 自定义域名与 ICP 备案（时间不够，免费平台子域名够用）
- 多区域部署、CDN
- Prometheus + Grafana 全栈（`/ops` 已够）
- 用户账号体系
