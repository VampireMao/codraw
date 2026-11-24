#!/usr/bin/env bash
# 重新构建含修复的前端 + 双平台二进制，并跑协议级验证
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"
. "$_SCRIPT_DIR/_env.sh"
set -uo pipefail
ROOT="$_REPO_ROOT"
cd "$ROOT" || exit 1


echo "=== 1. 构建前端 ==="
cd "$ROOT/web"
npm run build 2>&1 | grep -E 'built in|precache'

echo
echo "=== 2. 同步到 embed 并编译 ==="
cd "$ROOT"
bash backend/scripts/sync-webui.sh
cd backend
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$ROOT/dist/codraw-linux-amd64" ./cmd/server
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$ROOT/dist/codraw-windows-amd64.exe" ./cmd/server
ls -lh "$ROOT/dist"

echo
echo "=== 3. 启动服务（内存模式，独立端口）==="
pkill -f 'codraw-linux-amd64' 2>/dev/null || true
sleep 1
cd /tmp
PORT=18110 APP_ENV=development ALLOW_EMPTY_ORIGIN=true \
STORE=memory BROKER=memory PUBLIC_BASE_URL=http://127.0.0.1:18110 \
READONLY_SECRET="fix-verify-secret-at-least-32-bytes" \
"$ROOT/dist/codraw-linux-amd64" > /tmp/fix.log 2>&1 &
SRV=$!
for i in $(seq 1 60); do curl -sf http://127.0.0.1:18110/healthz >/dev/null 2>&1 && break; sleep 0.25; done
echo -n "  healthz: "; curl -s http://127.0.0.1:18110/healthz; echo

echo
echo "=== 4. 协议级验证三个修复 ==="
node "$ROOT/scripts/e2e-verify-fixes.mjs" --base http://127.0.0.1:18110
RC=$?

kill $SRV 2>/dev/null
echo
echo "退出码: $RC"
exit $RC
