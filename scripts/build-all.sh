#!/usr/bin/env bash
# 唯一可信的构建入口：前端 → embed → 双平台二进制。
#
# 为什么要有这个脚本：之前多个 pm-*.sh 各自实现了构建步骤，
# 其中一个漏了 sync-webui.sh，导致"改了前端代码但二进制里还是旧的"，
# 排查成本极高（表现是"修复无效"）。现在所有验证脚本都调它。
set -euo pipefail

export PATH="$HOME/.gvm/gos/go1.27.3/bin:$PATH"
export GOCACHE=/tmp/gocache-build
export GOFLAGS=-mod=mod

ROOT=/mnt/d/Work/AI-TMP/codraw
cd "$ROOT" || exit 1
DIST="$ROOT/dist"
mkdir -p "$DIST"

export NVM_DIR="$HOME/.nvm"
# shellcheck disable=SC1091
. "$NVM_DIR/nvm.sh" >/dev/null 2>&1
nvm use 24.18.0 >/dev/null 2>&1

echo "=== 1/4 构建前端 ==="
cd "$ROOT/web"
if [ ! -d node_modules ]; then
  echo "安装依赖..."
  npm ci --no-audit --no-fund 2>/dev/null || npm install --no-audit --no-fund
fi
npm run build 2>&1 | grep -E 'built in|error|ERROR' || true

echo
echo "=== 2/4 同步到 embed 目录 ==="
cd "$ROOT"
bash backend/scripts/sync-webui.sh

echo
echo "=== 3/4 校验同步结果（防止嵌入旧产物）==="
SRC_INDEX="$ROOT/web/dist/index.html"
DST_INDEX="$ROOT/backend/internal/webui/dist/index.html"
if [ ! -f "$SRC_INDEX" ] || [ ! -f "$DST_INDEX" ]; then
  echo "FAIL: index.html 缺失"
  exit 1
fi
SRC_SUM=$(md5sum "$SRC_INDEX" | cut -d' ' -f1)
DST_SUM=$(md5sum "$DST_INDEX" | cut -d' ' -f1)
if [ "$SRC_SUM" != "$DST_SUM" ]; then
  echo "FAIL: embed 目录与 web/dist 不一致（同步失败）"
  echo "  web/dist : $SRC_SUM"
  echo "  embed    : $DST_SUM"
  exit 1
fi
echo "  OK: index.html 一致 ($SRC_SUM)"
# 顺带确认资源目录也同步了
SRC_JS=$(find "$ROOT/web/dist/assets" -name '*.js' 2>/dev/null | wc -l)
DST_JS=$(find "$ROOT/backend/internal/webui/dist/assets" -name '*.js' 2>/dev/null | wc -l)
if [ "$SRC_JS" -ne "$DST_JS" ]; then
  echo "FAIL: assets 数量不一致 ($SRC_JS vs $DST_JS)"
  exit 1
fi
echo "  OK: assets 数量一致 ($SRC_JS 个 js)"

echo
echo "=== 4/4 编译双平台二进制 ==="
cd "$ROOT/backend"
LDFLAGS="-s -w -X main.version=${VERSION:-1.0.0} -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" \
  -o "$DIST/codraw-linux-amd64" ./cmd/server
echo "  linux   OK"

CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" \
  -o "$DIST/codraw-windows-amd64.exe" ./cmd/server
echo "  windows OK"

echo
echo "=== 产物 ==="
ls -lh "$DIST"
