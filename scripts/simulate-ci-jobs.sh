#!/usr/bin/env bash
# 在干净副本里模拟 CI 的其余 job
set -uo pipefail
TMP=/tmp/codraw-ci-sim
[ -d "$TMP" ] || { echo "先跑 simulate-ci.sh"; exit 1; }
cd "$TMP"

export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$HOME/.gvm/gos/go1.27.3/bin"

echo "=== 1. 模拟 backend job: gofmt + vet + test -race ==="
cd "$TMP/backend"
GENV="GOCACHE=/tmp/gocache-ci"
FMT=$(gofmt -l . 2>&1)
if [ -z "$FMT" ]; then echo "  ✅ gofmt"; else echo "  ❌ gofmt:"; echo "$FMT"; fi
env GOCACHE=/tmp/gocache-ci go vet ./... 2>&1 | tail -3 && echo "  ✅ vet"
env GOCACHE=/tmp/gocache-ci go test -race -count=1 ./... 2>&1 | grep -cE '^ok' | xargs -I{} echo "  {} 包通过（-race）"

echo
echo "=== 2. 模拟 frontend job: typecheck + test + build ==="
cd "$TMP/web"
npm run typecheck >/dev/null 2>&1 && echo "  ✅ typecheck"
npm run test 2>&1 | grep -E 'Test Files|Tests ' | sed 's/^/  /'
npm run build 2>&1 | grep -E 'built in' | sed 's/^/  /'
KB=$(du -sk dist | cut -f1)
echo "  dist: ${KB}KB (预算 800KB)"
[ "$KB" -le 800 ] && echo "  ✅ 体积达标" || echo "  ❌ 超预算"

echo
echo "=== 3. 模拟 docs job ==="
cd "$TMP"
MISS=0
for f in docs/PRD.md docs/ARCHITECTURE.md docs/PROTOCOL.md docs/PERMISSIONS.md \
         docs/DATA-MODEL.md docs/DEPLOY-SYSTEMD.md docs/sql/0001_init.sql README.md; do
  [ -f "$f" ] || { echo "  ❌ 缺 $f"; MISS=1; }
done
[ "$MISS" = "0" ] && echo "  ✅ 文档齐全"

echo
echo "=== 4. 模拟 single-binary 的 smoke test ==="
SB=/tmp/ci-sandbox
rm -rf "$SB" && mkdir -p "$SB"
cp "$TMP/dist/codraw-linux-amd64" "$SB/codraw"
cd "$SB"
PORT=19080 APP_ENV=development ALLOW_EMPTY_ORIGIN=true \
  STORE=memory BROKER=memory PUBLIC_BASE_URL=http://127.0.0.1:19080 \
  READONLY_SECRET=ci-smoke-secret-at-least-32-bytes \
  ./codraw > /tmp/ci-smoke.log 2>&1 &
SRV=$!
OK=0
for i in $(seq 1 60); do
  curl -sf http://127.0.0.1:19080/healthz >/dev/null 2>&1 && OK=1 && break
  sleep 0.25
done
if [ "$OK" = "1" ]; then
  echo "  ✅ 空目录启动成功"
  printf '     healthz: '; curl -s http://127.0.0.1:19080/healthz; echo
  printf '     首页: '; curl -s -o /dev/null -w 'HTTP %{http_code}\n' http://127.0.0.1:19080/
  printf '     SPA: '; curl -s -o /dev/null -w 'HTTP %{http_code}\n' http://127.0.0.1:19080/r/ABC123
  printf '     前端已嵌入: '; curl -s http://127.0.0.1:19080/ | grep -oE 'assets/index-[A-Za-z0-9_-]+\.js' | head -1
else
  echo "  ❌ 启动失败"
  tail -5 /tmp/ci-smoke.log
fi
kill $SRV 2>/dev/null

echo
echo "=== 5. 静态链接检查 ==="
file "$TMP/dist/codraw-linux-amd64" | grep -q 'statically linked' && echo "  ✅ 静态链接" || echo "  ❌ 非静态"
grep -qa 'manifest.webmanifest' "$TMP/dist/codraw-linux-amd64" && echo "  ✅ 前端已嵌入" || echo "  ❌ 前端未嵌入"
