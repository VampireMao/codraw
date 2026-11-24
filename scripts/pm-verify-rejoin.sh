#!/usr/bin/env bash
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"
. "$_SCRIPT_DIR/_env.sh"
set -uo pipefail
ROOT="$_REPO_ROOT"
cd "$ROOT" || exit 1


echo "=== 构建（含前端最新代码）==="
cd "$ROOT/web"
npm run build 2>&1 | grep -E 'built in|error'
cd "$ROOT"
bash backend/scripts/sync-webui.sh >/dev/null
cd backend
CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /tmp/codraw-rejoin ./cmd/server || exit 1
echo "build OK"

PORT=18250
PIDS=$(ss -ltnp 2>/dev/null | grep ":$PORT " | grep -oE 'pid=[0-9]+' | cut -d= -f2 | sort -u)
for p in $PIDS; do kill -9 "$p" 2>/dev/null || true; done
sleep 1

cd /tmp
PORT=$PORT APP_ENV=development ALLOW_EMPTY_ORIGIN=true \
STORE=memory BROKER=memory PUBLIC_BASE_URL="http://127.0.0.1:$PORT" \
READONLY_SECRET="rejoin-secret-at-least-32-bytes-ok" \
LEAVE_GRACE=2s \
/tmp/codraw-rejoin > /tmp/rejoin.log 2>&1 &
SRV=$!

READY=0
for i in $(seq 1 80); do
  if curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then READY=1; break; fi
  sleep 0.25
done
[ "$READY" = "1" ] || { echo "服务未就绪"; tail -8 /tmp/rejoin.log; kill $SRV 2>/dev/null; exit 1; }
echo "服务就绪"

cd "$ROOT/web"
node "$ROOT/scripts/e2e-rejoin-history.mjs" "http://127.0.0.1:$PORT" 4000
RC=$?
kill $SRV 2>/dev/null
exit $RC
