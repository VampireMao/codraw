# 同绘 CoDraw · 顶层 Makefile
# 用法：make help
#
# 约定：
#   - 所有命令在 WSL Ubuntu 24.04 内执行（Windows 侧请先 `wsl` 进入仓库）
#   - Go 走 ~/.gvm 的 1.27.3；Node 走 nvm 的 24.18.0
#   - 不依赖 sudo

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

# ---------- 工具链（可用环境变量覆盖） ----------
GO      ?= $(HOME)/.gvm/gos/go1.27.3/bin/go
NODE    ?= $(HOME)/.nvm/versions/node/v24.18.0/bin/node
NPM     ?= $(HOME)/.nvm/versions/node/v24.18.0/bin/npm

BACKEND := backend
WEB     := web
BIN     := $(BACKEND)/bin/codraw

# 本地开发默认零依赖
export BROKER ?= memory
export STORE  ?= memory
export PORT   ?= 8080
export APP_ENV ?= development

RACE := -race

.PHONY: help
help: ## 显示所有目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# ============================================================
# 开发
# ============================================================
.PHONY: dev-backend
dev-backend: ## 启动后端（memory 模式，:8080，热重启由 air 可选）
	cd $(BACKEND) && BROKER=$(BROKER) STORE=$(STORE) PORT=$(PORT) $(GO) run ./cmd/server

.PHONY: dev-web
dev-web: ## 启动前端开发服务器（:5173，代理 /api 与 /ws）
	cd $(WEB) && $(NPM) run dev

.PHONY: dev-web-install
dev-web-install: ## 安装前端依赖（Vue 专家初始化后使用）
	cd $(WEB) && $(NPM) install

# ============================================================
# 质量
# ============================================================
.PHONY: test
test: test-backend test-web ## 全部测试

.PHONY: test-backend
test-backend: ## go vet + go test -race
	cd $(BACKEND) && $(GO) vet ./... && $(GO) test $(RACE) -count=1 ./...

.PHONY: test-web
test-web: ## 前端单测（无 web/package.json 时跳过）
	@if [ -f $(WEB)/package.json ]; then \
	  cd $(WEB) && $(NPM) run test --if-present; \
	else echo "skip: $(WEB)/package.json 尚未创建（等 Vue 专家）"; fi

.PHONY: lint
lint: ## gofmt 检查 + 前端 lint
	@echo "== gofmt =="
	@cd $(BACKEND) && test -z "$$($(GO) fmt ./... 2>&1 | tee /dev/stderr)" || \
	  (echo "gofmt 已修复上述文件，请重新提交" && exit 1)
	@echo "== go vet =="
	@cd $(BACKEND) && $(GO) vet ./...
	@echo "== web lint =="
	@if [ -f $(WEB)/package.json ]; then cd $(WEB) && $(NPM) run lint --if-present; \
	 else echo "skip: $(WEB)/package.json 尚未创建"; fi

.PHONY: race
race: ## 只跑 race 检测
	cd $(BACKEND) && $(GO) test $(RACE) -count=1 ./...

# ============================================================
# 构建（单文件交付：前端 embed 进 Go 二进制）
# ============================================================
.PHONY: build
build: build-web embed-web build-backend ## 完整构建（当前平台）

.PHONY: binaries
binaries: ## 双平台单文件：dist/codraw-linux-amd64 + dist/codraw-windows-amd64.exe
	@bash scripts/build-all.sh

.PHONY: build-web
build-web: ## 构建前端到 web/dist
	@if [ -f $(WEB)/package.json ]; then \
	  cd $(WEB) && ($(NPM) ci --no-audit --no-fund 2>/dev/null || $(NPM) install --no-audit --no-fund); \
	  $(NPM) run build; \
	else echo "skip: $(WEB)/package.json 尚未创建（后端将使用占位页）"; fi

.PHONY: embed-web
embed-web: ## 把 web/dist 拷进 backend/internal/webui/dist（go:embed 的源）
	@bash $(BACKEND)/scripts/sync-webui.sh

.PHONY: build-backend
build-backend: ## 编译当前平台单二进制（前端已嵌入）到 backend/bin/codraw
	@mkdir -p $(BACKEND)/bin
	cd $(BACKEND) && CGO_ENABLED=0 $(GO) build \
	  -trimpath -ldflags "-s -w -X main.version=$$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
	  -o bin/codraw ./cmd/server
	@echo "built: $(BIN)"
	@ls -lh $(BIN) 2>/dev/null || true

.PHONY: verify-binaries
verify-binaries: ## 验证双平台产物（Linux 沙箱运行 + 内嵌前端检查）
	@bash scripts/verify-binaries.sh

.PHONY: clean
clean: ## 清理构建产物（不动 node_modules 与 .env）
	rm -rf $(BACKEND)/bin $(WEB)/dist
	@echo "cleaned"

# ============================================================
# 运行（单文件二进制）
# ============================================================
.PHONY: run
run: ## 本地跑 Linux 产物（memory 模式，:8080）
	@test -f dist/codraw-linux-amd64 || $(MAKE) binaries
	BROKER=memory STORE=memory PORT=$(PORT) APP_ENV=development \
	ALLOW_EMPTY_ORIGIN=true PUBLIC_BASE_URL=http://localhost:$(PORT) \
	READONLY_SECRET=local-dev-secret-at-least-32-bytes-ok \
	./dist/codraw-linux-amd64

.PHONY: health
health: ## 探活 + 就绪（HTTP 探针，需服务已启动）
	@curl -s localhost:$(PORT)/healthz; echo
	@curl -s localhost:$(PORT)/readyz | jq . 2>/dev/null || curl -s localhost:$(PORT)/readyz; echo

.PHONY: healthcheck
healthcheck: ## 用二进制自带子命令探活（不依赖 curl）
	@./dist/codraw-linux-amd64 healthcheck && echo "healthy" || echo "unhealthy"

# ============================================================
# 压测 / 运维
# ============================================================
.PHONY: loadtest
loadtest: ## 压测：make loadtest SCENARIO=s1 CLIENTS=100 RATE=5 DURATION=30s
	@cd tools/loadtest && $(GO) build -o /tmp/codraw-lt . && \
	  /tmp/codraw-lt --scenario $${SCENARIO:-s1} --base http://localhost:$(PORT) \
	    --clients $${CLIENTS:-100} --rate $${RATE:-5} --duration $${DURATION:-30s} \
	    --origin http://localhost:$(PORT)

.PHONY: ops
ops: ## 打印运维面板数据
	curl -s localhost:$(PORT)/ops/data | jq .

# ============================================================
# 验收（PM 独立验证，见 docs/ACCEPTANCE.md）
# ============================================================
.PHONY: verify
verify: ## 全部验收：单元测试 + 协议 E2E + 权限 + 重新加入
	@bash scripts/pm-test-all.sh
	@bash scripts/pm-verify-e2e-v2.sh
	@bash scripts/pm-verify-permissions.sh
	@bash scripts/pm-verify-rejoin.sh

.PHONY: verify-e2e
verify-e2e: ## 仅协议级 E2E（61 断言）
	@bash scripts/pm-verify-e2e-v2.sh

.PHONY: verify-permissions
verify-permissions: ## 仅权限模型验收（P1–P12）
	@bash scripts/pm-verify-permissions.sh

.PHONY: verify-ddl
verify-ddl: ## 验证 docs/sql 的 DDL 可执行且幂等（临时容器）
	@bash scripts/verify-ddl-doc.sh

# ============================================================
# 数据库（用本地已装的 psql / redis-cli）
# ============================================================
.PHONY: psql
psql: ## 连本地 Postgres（需 DATABASE_URL 或默认本地实例）
	psql "$${DATABASE_URL:-postgres://codraw:codraw@127.0.0.1:5432/codraw}"

.PHONY: redis-cli
redis-cli: ## 连本地 Redis
	redis-cli

.PHONY: migrate-verify
migrate-verify: verify-ddl ## verify-ddl 的别名（保留旧名字）
