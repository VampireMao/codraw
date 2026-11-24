#!/usr/bin/env bash
# 旧协议 E2E（已按 v2 权限模型更新断言）
set -uo pipefail
export PATH="$HOME/.gvm/gos/go1.27.3/bin:$PATH"
export GOCACHE=/tmp/gocache-pm
ROOT=/mnt/d/Work/AI-TMP/codraw
cd "$ROOT" || exit 1

export NVM_DIR="$HOME/.nvm"
# shellcheck disable=SC1091
. "$NVM_DIR/nvm.sh" >/dev/null 2>&1
nvm use 24.18.0 >/dev/null 2>&1

cd "$ROOT/backend"
go build -o /tmp/codraw-e2e ./cmd/server || exit 1

PORT=18230
# 精确清理端口占用（不要用 pkill -f，它会误杀编译进程本身）
PIDS=$(ss -ltnp 2>/dev/null | grep ":$PORT " | grep -oE 'pid=[0-9]+' | cut -d= -f2 | sort -u)
for p in $PIDS; do
  echo "  清理端口占用: pid=$p"
  kill -9 "$p" 2>/dev/null || true
done
sleep 1.5

cd /tmp
PORT=$PORT APP_ENV=development ALLOW_EMPTY_ORIGIN=true \
STORE=memory BROKER=memory PUBLIC_BASE_URL="http://127.0.0.1:$PORT" \
READONLY_SECRET="e2e-secret-at-least-32-bytes-long-ok" \
LEAVE_GRACE=2s \
/tmp/codraw-e2e > /tmp/e2e.log 2>&1 &
SRV=$!

READY=0
for i in $(seq 1 80); do
  if curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then READY=1; break; fi
  sleep 0.25
done
[ "$READY" = "1" ] || { echo "服务未就绪"; tail -8 /tmp/e2e.log; kill $SRV 2>/dev/null; exit 1; }

cd "$ROOT"
node scripts/e2e-acceptance.mjs --base "http://127.0.0.1:$PORT" 2>&1 | tail -30
RC=${PIPESTATUS[0]}

kill $SRV 2>/dev/null
echo "exit=$RC"
exit $RC
