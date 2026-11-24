#!/usr/bin/env bash
# PM 独立验收：权限模型 P1–P12
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"
. "$_SCRIPT_DIR/_env.sh"
set -uo pipefail
ROOT="$_REPO_ROOT"
cd "$ROOT" || exit 1

echo "=== 编译后端 ==="
cd "$ROOT/backend"
go build -o /tmp/codraw-perm ./cmd/server || { echo "BUILD FAILED"; exit 1; }
echo "build OK"

PORT=18220
pkill -f 'codraw-perm' 2>/dev/null || true
sleep 1

echo
echo "=== 启动（memory 模式，独立端口 $PORT，LEAVE_GRACE=2s 加速移交测试）==="
cd /tmp
PORT=$PORT APP_ENV=development ALLOW_EMPTY_ORIGIN=true \
STORE=memory BROKER=memory PUBLIC_BASE_URL="http://127.0.0.1:$PORT" \
READONLY_SECRET="perm-secret-at-least-32-bytes-long" \
LEAVE_GRACE=2s \
/tmp/codraw-perm > /tmp/perm.log 2>&1 &
SRV=$!

READY=0
for i in $(seq 1 80); do
  if curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then READY=1; break; fi
  sleep 0.25
done
if [ "$READY" != "1" ]; then
  echo "FAIL: 服务未就绪"
  tail -8 /tmp/perm.log
  kill $SRV 2>/dev/null
  exit 1
fi
echo "服务就绪"

echo
echo "=== 跑权限验收 ==="
cd "$ROOT"
node scripts/e2e-permissions.mjs --base "http://127.0.0.1:$PORT"
RC=$?

echo
echo "=== 服务端日志（查 panic/error）==="
grep -icE 'panic|"level":"ERROR"' /tmp/perm.log | xargs -I{} echo "  panic/ERROR 条数: {}"

kill $SRV 2>/dev/null
echo "exit=$RC"
exit $RC
