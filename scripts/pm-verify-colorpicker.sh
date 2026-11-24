#!/usr/bin/env bash
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"
. "$_SCRIPT_DIR/_env.sh"
set -uo pipefail
ROOT="$_REPO_ROOT"
cd "$ROOT" || exit 1


echo "=== typecheck ==="
cd "$ROOT/web"
npm run typecheck 2>&1 | tail -6
echo "exit=${PIPESTATUS[0]}"

echo
echo "=== build ==="
npm run build 2>&1 | grep -E 'built in|error'

echo
echo "=== 编译二进制 ==="
cd "$ROOT"
bash backend/scripts/sync-webui.sh >/dev/null
cd backend
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$ROOT/dist/codraw-linux-amd64" ./cmd/server
echo OK

echo
echo "=== 验证颜色面板 ==="
pkill -f 'codraw-linux-amd64' 2>/dev/null || true
sleep 1
cd /tmp
PORT=18161 APP_ENV=development ALLOW_EMPTY_ORIGIN=true \
STORE=memory BROKER=memory PUBLIC_BASE_URL=http://127.0.0.1:18161 \
READONLY_SECRET="cp2-secret-at-least-32-bytes-long" \
"$ROOT/dist/codraw-linux-amd64" > /tmp/cp2.log 2>&1 &
SRV=$!
for i in $(seq 1 60); do curl -sf http://127.0.0.1:18161/healthz >/dev/null 2>&1 && break; sleep 0.25; done

cd "$ROOT/web"
node "$ROOT/scripts/diag-colorpicker-fixed.mjs" http://127.0.0.1:18161
RC=$?
kill $SRV 2>/dev/null
exit $RC
