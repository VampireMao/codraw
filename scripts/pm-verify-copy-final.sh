#!/usr/bin/env bash
set -uo pipefail
export PATH="$HOME/.gvm/gos/go1.27.3/bin:$PATH"
export GOCACHE=/tmp/gocache-pm
ROOT=/mnt/d/Work/AI-TMP/codraw
cd "$ROOT" || exit 1

export NVM_DIR="$HOME/.nvm"
# shellcheck disable=SC1091
. "$NVM_DIR/nvm.sh" >/dev/null 2>&1
nvm use 24.18.0 >/dev/null 2>&1

if [ ! -f "$ROOT/dist/codraw-linux-amd64" ]; then
  echo "二进制不存在，先构建"
  cd "$ROOT/web" && npm run build 2>&1 | grep -E 'built in'
  cd "$ROOT" && bash backend/scripts/sync-webui.sh >/dev/null
  cd backend && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$ROOT/dist/codraw-linux-amd64" ./cmd/server
fi

LANIP=$(hostname -I | awk '{print $1}')
PORT=18206

echo "=== 清理残留 ==="
pkill -f 'codraw-linux-amd64' 2>/dev/null || true
sleep 1.5
if ss -ltn 2>/dev/null | grep -q ":$PORT "; then
  echo "端口 $PORT 仍被占用，换端口"
  PORT=18207
fi

echo "=== 启动服务 (port $PORT, LAN $LANIP) ==="
cd /tmp
PORT=$PORT APP_ENV=development ALLOW_EMPTY_ORIGIN=true \
STORE=memory BROKER=memory PUBLIC_BASE_URL="http://$LANIP:$PORT" \
READONLY_SECRET="final-copy-secret-at-least-32-bytes-ok" \
"$ROOT/dist/codraw-linux-amd64" > /tmp/copyfinal2.log 2>&1 &
SRV=$!

READY=0
for i in $(seq 1 80); do
  if curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then READY=1; break; fi
  sleep 0.25
done

if [ "$READY" != "1" ]; then
  echo "FAIL: 服务未就绪"
  tail -6 /tmp/copyfinal2.log
  kill $SRV 2>/dev/null
  exit 1
fi
echo "服务就绪"

echo
cd "$ROOT/web"
node "$ROOT/scripts/diag-copy-final.mjs" "http://127.0.0.1:$PORT" "http://$LANIP:$PORT"
RC=$?
kill $SRV 2>/dev/null
exit $RC
