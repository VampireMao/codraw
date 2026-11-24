#!/usr/bin/env bash
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"
. "$_REPO_ROOT/scripts/_env.sh"
#
# End-to-end acceptance in memory mode: real binary, real HTTP, real WebSocket.
#
# Asserts the contract behaviours a reviewer will click through:
#   1. create a room via REST, code matches the DDL CHECK, keys are 43 chars
#   2. get room without a key -> viewer, with the editor key -> editor
#   3. editor joins, draws, gets applied+seq, connection stays alive
#   4. viewer draws -> ack{rejected, forbidden}, connection stays alive
#   5. same (clientId, opId) twice -> second is duplicate with the same seq
#   6. readonly token issuance, use, and revocation
#   7. snapshot paging
#   8. /healthz /readyz /metrics /ops /ops/data
#   9. graceful SIGTERM under 5s
#  10. secrets and nicknames never reach the access log
set -euo pipefail

BACKEND=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$BACKEND"

if ! command -v go >/dev/null 2>&1; then
  export PATH="$HOME/.gvm/gos/go1.27.3/bin:$PATH"
fi

BIN=/tmp/codraw-e2e
rm -rf "$BIN"; mkdir -p "$BIN"
go build -o "$BIN/server" ./cmd/server
go build -o "$BIN/wssmoke" ./cmd/wssmoke

PORT=${PORT:-18100}
LOG=/tmp/codraw-e2e.log
rm -f "$LOG"

STORE=memory BROKER=memory PORT=$PORT LOG_LEVEL=debug LOG_FORMAT=text \
  READONLY_SECRET="dev-only-change-me-please-32bytes-min" \
  PUBLIC_BASE_URL="http://localhost:$PORT" \
  "$BIN/server" >"$LOG" 2>&1 &
PID=$!
cleanup() { kill -TERM "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
trap cleanup EXIT

for i in $(seq 1 150); do
  curl -fsS "http://localhost:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.05
done

FAIL=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAIL=1; }
expect_eq() { if [ "$1" = "$2" ]; then pass "$3 ($1)"; else fail "$3: got '$1', want '$2'"; fi; }
expect_contains() { if echo "$1" | grep -q "$2"; then pass "$3"; else fail "$3: '$2' not in '$1'"; fi; }

echo "=== 1. create room ==="
CREATE=$(curl -s -X POST "http://localhost:$PORT/api/rooms" -H 'Content-Type: application/json' -d '{"title":"联调"}')
CODE=$(echo "$CREATE" | python3 -c 'import sys,json;print(json.load(sys.stdin)["room"]["code"])')
EKEY=$(echo "$CREATE" | python3 -c 'import sys,json;print(json.load(sys.stdin)["keys"]["editorKey"])')
VKEY=$(echo "$CREATE" | python3 -c 'import sys,json;print(json.load(sys.stdin)["keys"]["viewerKey"])')
echo "$CREATE" | python3 -m json.tool | head -20
if [[ "$CODE" =~ ^[2-9A-HJKMNP-Z]{6}$ ]]; then pass "code $CODE matches ^[2-9A-HJKMNP-Z]{6}$"; else fail "code $CODE"; fi
expect_eq "${#EKEY}" 43 "editorKey length"
expect_eq "${#VKEY}" 43 "viewerKey length"

echo "=== 2. room metadata ==="
expect_eq "$(curl -s "http://localhost:$PORT/api/rooms/$CODE" | python3 -c 'import sys,json;print(json.load(sys.stdin)["role"])')" viewer "role without key"
expect_eq "$(curl -s "http://localhost:$PORT/api/rooms/$CODE?key=$EKEY" | python3 -c 'import sys,json;print(json.load(sys.stdin)["role"])')" editor "role with editor key"
expect_eq "$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:$PORT/api/rooms/NOPE")" 400 "malformed code"
expect_eq "$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:$PORT/api/rooms/ZZZZZZ")" 404 "unknown code"

echo "=== 3. editor draws ==="
OUT=$("$BIN/wssmoke" -base "http://localhost:$PORT" -room "$CODE" -key "$EKEY" -send-op 1 -name "编辑者" 2>&1)
echo "$OUT" | grep -E 'joined|snapshot|ack|pong' | head -6
expect_contains "$OUT" 'status=applied seq=1' "editor op applied with seq 1"
expect_contains "$OUT" 'connection alive' "connection alive after write"

echo "=== 4. viewer write is rejected, connection survives ==="
OUT=$("$BIN/wssmoke" -base "http://localhost:$PORT" -room "$CODE" -key "$VKEY" -send-op 1 -name "只读" 2>&1)
echo "$OUT" | grep -E 'role=viewer|ack|pong' | head -4
expect_contains "$OUT" 'status=rejected' "viewer op rejected"
expect_contains "$OUT" 'code=forbidden' "rejection code is forbidden"
expect_contains "$OUT" 'connection alive' "viewer connection survives a rejected write"

echo "=== 5. idempotent retransmit ==="
OUT=$("$BIN/wssmoke" -base "http://localhost:$PORT" -room "$CODE" -key "$EKEY" -send-op 2 -same-op-id -name "编辑者" 2>&1)
echo "$OUT" | grep -E 'ack|idempotency' | head -4
expect_contains "$OUT" 'status=duplicate' "second op is duplicate"
expect_contains "$OUT" 'idempotency verified' "duplicate reused the original seq"

echo "=== 6. readonly token lifecycle ==="
RTJSON=$(curl -s -X POST "http://localhost:$PORT/api/rooms/$CODE/readonly-token" \
  -H 'Content-Type: application/json' -d "{\"editorKey\":\"$EKEY\",\"ttlSeconds\":3600}")
RT=$(echo "$RTJSON" | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
expect_eq "$(echo "$RTJSON" | python3 -c 'import sys,json;print(json.load(sys.stdin)["role"])')" viewer "token role"
OUT=$("$BIN/wssmoke" -base "http://localhost:$PORT" -room "$CODE" -rt "$RT" -send-op 1 -name "只读token" 2>&1)
expect_contains "$OUT" 'role=viewer' "token grants viewer"
expect_contains "$OUT" 'status=rejected' "token cannot write"
# A viewer key must not be able to mint tokens.
expect_eq "$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://localhost:$PORT/api/rooms/$CODE/readonly-token" \
  -H 'Content-Type: application/json' -d "{\"editorKey\":\"$VKEY\"}")" 403 "viewer key cannot issue tokens"
# Revoke.
curl -s -X POST "http://localhost:$PORT/api/rooms/$CODE/readonly-token/revoke" \
  -H 'Content-Type: application/json' -d "{\"editorKey\":\"$EKEY\",\"all\":true}" >/dev/null
expect_eq "$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:$PORT/ws?room=$CODE&cid=c_revoked1&rt=$RT")" 403 "revoked token refused at handshake"

echo "=== 7. snapshot paging ==="
PAGE=$(curl -s "http://localhost:$PORT/api/rooms/$CODE/snapshot?key=$EKEY&limit=1")
echo "$PAGE" | python3 -c 'import sys,json;d=json.load(sys.stdin);print("seq",d["seq"],"shapes",len(d["shapes"]),"complete",d["complete"],"next",d["next"])'
expect_contains "$PAGE" '"complete":false' "first page is not complete"
expect_contains "$PAGE" '"next":"sh_' "first page has a cursor"

echo "=== 8. ops endpoints ==="
for p in /healthz /readyz /metrics /ops /ops/data /api/meta; do
  expect_eq "$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:$PORT$p")" 200 "GET $p"
done
expect_contains "$(curl -s "http://localhost:$PORT/metrics")" 'codraw_msgs_total{dir="in",type="op.add"}' "metrics has op.add counter"
expect_contains "$(curl -s "http://localhost:$PORT/ops/data")" '"broadcastLatencyP95Ms"' "/ops/data has p95"

echo "=== 9. unknown API path is JSON 404 ==="
BODY=$(curl -s "http://localhost:$PORT/api/nope")
expect_contains "$BODY" '"code":"not_found"' "JSON error envelope"

echo "=== 10. secrets never reach the log ==="
curl -s -o /dev/null "http://localhost:$PORT/api/rooms/$CODE?key=SUPERSECRET&name=%E5%B0%8F%E6%9E%97"
if grep -q 'SUPERSECRET' "$LOG"; then fail "access log leaked a key"; else pass "key not logged"; fi
if grep -q '%E5%B0%8F' "$LOG"; then fail "access log leaked a nickname"; else pass "nickname not logged"; fi
expect_contains "$(grep 'http request' "$LOG" | tail -3)" 'key=\*\*\*' "query key masked"

echo "=== 11. graceful shutdown ==="
kill -TERM "$PID"
start=$(date +%s%N)
wait "$PID" || true
end=$(date +%s%N)
ms=$(( (end - start) / 1000000 ))
if [ "$ms" -le 5000 ]; then pass "shutdown in ${ms}ms (budget 5s)"; else fail "shutdown took ${ms}ms"; fi
trap - EXIT

if grep -E 'level=ERROR' "$LOG" >/dev/null 2>&1; then
  fail "server logged errors:"; grep -E 'level=ERROR' "$LOG" | head -5
else
  pass "no error-level logs"
fi

echo
if [ "$FAIL" -ne 0 ]; then echo "E2E FAILED"; exit 1; fi
echo "E2E PASSED"
