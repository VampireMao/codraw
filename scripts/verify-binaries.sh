#!/usr/bin/env bash
# 双平台产物运行验证：Linux 直接跑，Windows 产物用 wine（若无则只做结构检查）
set -uo pipefail
ROOT=/mnt/d/Work/AI-TMP/codraw
DIST="$ROOT/dist"
cd "$ROOT" || exit 1

check_http() {
  local port="$1" label="$2"
  printf '  %-22s' "$label"
  local body code
  code=$(curl -s -o /tmp/body.html -w '%{http_code}' "http://127.0.0.1:$port/" 2>/dev/null)
  body=$(cat /tmp/body.html 2>/dev/null)
  echo "HTTP $code"
  return 0
}

echo "=============================================="
echo " Linux 产物：空目录隔离运行"
echo "=============================================="
SB=/tmp/sb-linux
rm -rf "$SB" && mkdir -p "$SB"
cp "$DIST/codraw-linux-amd64" "$SB/codraw"
chmod +x "$SB/codraw"
echo "沙箱文件数: $(ls -1 "$SB" | wc -l)"

cd "$SB"
PORT=18101 APP_ENV=development ALLOW_EMPTY_ORIGIN=true \
STORE=memory BROKER=memory PUBLIC_BASE_URL=http://127.0.0.1:18101 \
READONLY_SECRET="linux-sandbox-secret-32-bytes-min-ok" \
./codraw > /tmp/sb-linux.log 2>&1 &
SRV=$!
for i in $(seq 1 40); do curl -sf http://127.0.0.1:18101/healthz >/dev/null 2>&1 && break; sleep 0.25; done

check_http 18101 "/ (首页)"
printf '  %-22s' "/r/ABC123 (SPA)"; curl -s -o /dev/null -w 'HTTP %{http_code}\n' http://127.0.0.1:18101/r/ABC123
printf '  %-22s' "/healthz"; curl -s http://127.0.0.1:18101/healthz; echo
printf '  %-22s' "首页含真实 bundle"; curl -s http://127.0.0.1:18101/ | grep -oE 'assets/index-[A-Za-z0-9_-]+\.js' | head -1
printf '  %-22s' "version 子命令"; ./codraw version 2>&1 | head -1 || echo "(无 version 子命令)"

# WebSocket 握手验证（不带 Origin，dev 模式放行）
printf '  %-22s' "WS 握手"
curl -s -o /dev/null -w 'HTTP %{http_code}\n' \
  -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
  "http://127.0.0.1:18101/ws?room=ZZZZZZ&cid=probe" || true

kill $SRV 2>/dev/null
sleep 1

echo
echo "=============================================="
echo " Windows 产物：结构检查 + wine（若可用）"
echo "=============================================="
file "$DIST/codraw-windows-amd64.exe" | sed 's/^/  /'
echo "  PE 头:"; xxd -l 64 "$DIST/codraw-windows-amd64.exe" | head -2 | sed 's/^/    /'

if command -v wine >/dev/null 2>&1 || command -v wine64 >/dev/null 2>&1; then
  echo "  wine 可用，尝试运行..."
  WINEDEBUG=-all timeout 25 wine "$DIST/codraw-windows-amd64.exe" version 2>&1 | head -3
else
  echo "  wine 不可用（正常）：Windows 产物需在 Windows 上运行验证"
  echo "  → 下一步在 Windows 侧直接执行该 exe 验证"
fi

echo
echo "=============================================="
echo " 二进制内嵌前端确认（两平台）"
echo "=============================================="
for f in codraw-linux-amd64 codraw-windows-amd64.exe; do
  if grep -qa 'manifest.webmanifest' "$DIST/$f" && grep -qa 'assets/index-' "$DIST/$f"; then
    echo "  OK   $f 内含前端资源"
  else
    echo "  FAIL $f 未含前端资源"
  fi
done
