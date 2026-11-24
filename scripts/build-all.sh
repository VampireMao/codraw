#!/usr/bin/env bash
# 唯一可信的构建入口：前端 → embed → 双平台二进制。
#
# 为什么要有这个脚本：之前多个 pm-*.sh 各自实现了构建步骤，
# 其中一个漏了 sync-webui.sh，导致"改了前端代码但二进制里还是旧的"，
# 排查成本极高（表现是"修复无效"）。现在所有验证脚本都调它。
#
# 可在任意目录执行：路径全部基于脚本自身位置推断，CI 与本地通用。
set -euo pipefail

_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"
# shellcheck source=_env.sh
. "$_SCRIPT_DIR/_env.sh"

cd "$ROOT"
DIST="$ROOT/dist"
mkdir -p "$DIST"

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
# 用内容比较而不是 md5sum：macOS 上是 md5，容器里可能没有 md5sum 之外的工具
if ! cmp -s "$SRC_INDEX" "$DST_INDEX"; then
  echo "FAIL: embed 目录与 web/dist 不一致（同步失败）"
  exit 1
fi
echo "  OK: index.html 一致"
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
VERSION="${VERSION:-1.0.0}"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w -X main.version=${VERSION} -X main.buildTime=${BUILD_TIME}"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" \
  -o "$DIST/codraw-linux-amd64" ./cmd/server
echo "  linux   OK"

CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" \
  -o "$DIST/codraw-windows-amd64.exe" ./cmd/server
echo "  windows OK"

echo
echo "=== 产物 ==="
ls -lh "$DIST"
