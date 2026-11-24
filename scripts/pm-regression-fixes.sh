#!/usr/bin/env bash
# 三处修复后的完整回归
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"
. "$_SCRIPT_DIR/_env.sh"
set -uo pipefail
ROOT="$_REPO_ROOT"
cd "$ROOT" || exit 1


echo "=== 1. 后端 vet + test -race ==="
cd "$ROOT/backend"
go vet ./... && echo "  vet OK"
go test -race -count=1 ./... 2>&1 | grep -E '^(ok|FAIL)'
BEND=${PIPESTATUS[0]}

echo
echo "=== 2. 前端 typecheck + test + build ==="
cd "$ROOT/web"
npm run typecheck >/dev/null 2>&1 && echo "  typecheck OK"
npm run test 2>&1 | grep -E 'Test Files|Tests '
npm run build >/dev/null 2>&1 && echo "  build OK"

echo
echo "=== 3. 启动服务并跑协议级 E2E（原有 54 断言）==="
pkill -f 'codraw-linux-amd64' 2>/dev/null || true
sleep 1
cd /tmp
PORT=18112 APP_ENV=development ALLOW_EMPTY_ORIGIN=true \
STORE=memory BROKER=memory PUBLIC_BASE_URL=http://127.0.0.1:18112 \
READONLY_SECRET="regression-secret-at-least-32-bytes" \
"$ROOT/dist/codraw-linux-amd64" > /tmp/reg.log 2>&1 &
SRV=$!
for i in $(seq 1 60); do curl -sf http://127.0.0.1:18112/healthz >/dev/null 2>&1 && break; sleep 0.25; done

cd "$ROOT"
node scripts/e2e-acceptance.mjs --base http://127.0.0.1:18112 2>&1 | tail -6
E2E=${PIPESTATUS[0]}

echo
echo "=== 4. 三个修复验证 ==="
node scripts/e2e-verify-fixes.mjs --base http://127.0.0.1:18112 2>&1 | tail -5
FIX=${PIPESTATUS[0]}

kill $SRV 2>/dev/null

echo
echo "=== 汇总 ==="
echo "后端测试: $BEND"
echo "原有 E2E: $E2E"
echo "修复验证: $FIX"
[ "$E2E" = "0" ] && [ "$FIX" = "0" ] && echo "全部通过" || echo "有失败项"
