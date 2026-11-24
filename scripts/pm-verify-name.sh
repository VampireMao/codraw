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

echo "=== typecheck + build ==="
cd "$ROOT/web"
npm run typecheck >/dev/null 2>&1 && echo "typecheck OK"
npm run build 2>&1 | grep -E 'built in|error'

echo
echo "=== 编译二进制 ==="
cd "$ROOT"
bash backend/scripts/sync-webui.sh >/dev/null
cd backend
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$ROOT/dist/codraw-linux-amd64" ./cmd/server
echo OK

echo
echo "=== 验证昵称功能 ==="
pkill -f 'codraw-linux-amd64' 2>/dev/null || true
sleep 1
cd /tmp
PORT=18170 APP_ENV=development ALLOW_EMPTY_ORIGIN=true \
STORE=memory BROKER=memory PUBLIC_BASE_URL=http://127.0.0.1:18170 \
READONLY_SECRET="name-secret-at-least-32-bytes-long" \
"$ROOT/dist/codraw-linux-amd64" > /tmp/name.log 2>&1 &
SRV=$!
for i in $(seq 1 60); do curl -sf http://127.0.0.1:18170/healthz >/dev/null 2>&1 && break; sleep 0.25; done

cd "$ROOT/web"
node "$ROOT/scripts/diag-name-prompt.mjs" http://127.0.0.1:18170
RC=$?
kill $SRV 2>/dev/null
exit $RC
