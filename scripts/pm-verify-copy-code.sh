#!/usr/bin/env bash
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"
. "$_SCRIPT_DIR/_env.sh"
set -uo pipefail
ROOT="$_REPO_ROOT"

echo "=== typecheck ==="
cd "$ROOT/web"
npm run typecheck 2>&1 | tail -4
echo "exit=${PIPESTATUS[0]}"

echo
echo "=== 构建（统一入口，含同步校验）==="
bash "$ROOT/scripts/build-all.sh" 2>&1 | grep -E 'OK|built in|FAIL|失败'

echo
echo "=== 清理端口 ==="
PIDS=$(ss -ltnp 2>/dev/null | grep ':9000 ' | grep -oE 'pid=[0-9]+' | cut -d= -f2 | sort -u)
for p in $PIDS; do kill -9 "$p" 2>/dev/null || true; done
pkill -f 'codraw-linux-amd64' 2>/dev/null || true
sleep 1.5

echo "=== 启动 localhost:9000 ==="
cd /tmp
PORT=9000 APP_ENV=development ALLOW_EMPTY_ORIGIN=true \
STORE=memory BROKER=memory PUBLIC_BASE_URL="http://localhost:9000" \
READONLY_SECRET="codecopy-secret-at-least-32-bytes" \
"$ROOT/dist/codraw-linux-amd64" > /tmp/codecopy.log 2>&1 &
SRV=$!

for i in $(seq 1 80); do curl -sf "http://127.0.0.1:9000/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
if ! curl -sf "http://127.0.0.1:9000/healthz" >/dev/null 2>&1; then
  echo "FAIL: 服务未就绪"; tail -5 /tmp/codecopy.log; exit 1
fi
echo "服务就绪"

cd "$ROOT/web"
node "$ROOT/scripts/diag-copy-code.mjs" "http://localhost:9000"
RC=$?
kill $SRV 2>/dev/null
exit $RC
