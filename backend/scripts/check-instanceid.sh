#!/usr/bin/env bash
# Proves joined.payload.instanceId and /ops/data.instanceId are present and equal,
# and that two instances report different IDs.
set -euo pipefail
export PATH="$HOME/.gvm/gos/go1.27.3/bin:$PATH"
export GOCACHE="${GOCACHE:-/tmp/codraw-gocache}"
cd /mnt/d/Work/AI-TMP/codraw/backend

GOBIN=/tmp/codraw-instid
rm -rf "$GOBIN"; mkdir -p "$GOBIN"
go build -o "$GOBIN/server" ./cmd/server
go build -o "$GOBIN/wssmoke" ./cmd/wssmoke

P1=18200; P2=18201
start() {
  STORE=memory BROKER=memory PORT=$1 LOG_LEVEL=error LOG_FORMAT=text \
    READONLY_SECRET="dev-only-change-me-please-32bytes-min" \
    PUBLIC_BASE_URL="http://localhost:$1" "$GOBIN/server" >/tmp/instid-$1.log 2>&1 &
  echo $!
}
PID1=$(start $P1); PID2=$(start $P2)
trap 'kill -TERM $PID1 $PID2 2>/dev/null || true; wait 2>/dev/null || true' EXIT
for i in $(seq 1 120); do
  curl -fsS "http://localhost:$P1/healthz" >/dev/null 2>&1 && curl -fsS "http://localhost:$P2/healthz" >/dev/null 2>&1 && break
  sleep 0.05
done

CREATE=$(curl -s -X POST "http://localhost:$P1/api/rooms" -H 'Content-Type: application/json' -d '{"title":"instid"}')
CODE=$(echo "$CREATE" | python3 -c 'import sys,json;print(json.load(sys.stdin)["room"]["code"])')
EKEY=$(echo "$CREATE" | python3 -c 'import sys,json;print(json.load(sys.stdin)["keys"]["editorKey"])')

echo "--- /ops/data instanceId ---"
ID1=$(curl -s "http://localhost:$P1/ops/data" | python3 -c 'import sys,json;print(json.load(sys.stdin)["instanceId"])')
ID2=$(curl -s "http://localhost:$P2/ops/data" | python3 -c 'import sys,json;print(json.load(sys.stdin)["instanceId"])')
echo "inst1 = $ID1"
echo "inst2 = $ID2"
[ "$ID1" != "$ID2" ] && echo "OK: two instances report different IDs" || { echo "FAIL: same instanceId"; exit 1; }

echo "--- joined.payload.instanceId from a real WS handshake ---"
OUT=$("$GOBIN/wssmoke" -base "http://localhost:$P1" -room "$CODE" -key "$EKEY" -send-op 0 -name idcheck 2>&1)
JID=$(echo "$OUT" | grep -o '"instanceId":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "joined instanceId = $JID"
[ "$JID" = "$ID1" ] && echo "OK: joined.instanceId matches /ops/data" || { echo "FAIL: joined=$JID ops=$ID1"; exit 1; }

echo "INSTANCE ID CHECK PASSED"
