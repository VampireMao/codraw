#!/usr/bin/env bash
# Contract-alignment self-check, mirroring docs/CONTRACT-ALIGNMENT.md §5.
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/../.." && pwd)"
set -uo pipefail
cd "$_REPO_ROOT"
FAIL=0
ok() { echo "  [OK]   $1"; }
bad() { echo "  [FAIL] $1"; FAIL=1; }

echo "=== 1. no old /api/v1 prefix ==="
if grep -rn '"/api/v1' backend/ --include='*.go' | grep -v 'v1/rooms' >/dev/null 2>&1; then
  bad "old /api/v1 prefix still present"; grep -rn '"/api/v1' backend/ --include='*.go' | head -5
else
  ok "no /api/v1 routes"
fi

echo "=== 2. all protocol message types exist in Go ==="
for t in hello joined op.add ack presence.join presence.leave presence.cursor \
         resync.request resync.snapshot resync.events room.meta error ping pong bye; do
  if grep -rq "\"$t\"" backend/internal; then ok "type $t"; else bad "MISSING type $t"; fi
done

echo "=== 3. room code alphabet is 31 chars without L ==="
if grep -q '23456789ABCDEFGHJKMNPQRSTUVWXYZ' backend/internal/room/code.go; then
  ok "alphabet is the 31-char set"
else
  bad "alphabet mismatch"; grep -n 'CodeAlphabet' backend/internal/room/code.go
fi
if grep -q '23456789ABCDEFGHJKLMNPQRSTUVWXYZ' backend/internal/room/code.go; then
  bad "alphabet still contains L"
else
  ok "alphabet excludes L"
fi
if grep -q '2-9A-HJKMNP-Z' docs/sql/0001_init.sql; then ok "DDL CHECK matches"; else bad "DDL CHECK mismatch"; fi

echo "=== 4. envelope shape {v,type,seq,roomId,clientId,ts,payload} ==="
for f in 'json:"v"' 'json:"type"' 'json:"seq,omitempty"' 'json:"roomId"' 'json:"clientId"' 'json:"ts"' 'json:"payload"'; do
  if grep -q "$f" backend/internal/protocol/protocol.go; then ok "envelope field $f"; else bad "envelope missing $f"; fi
done

echo "=== 5. payload is json.RawMessage ==="
if grep -q 'Payload  json.RawMessage' backend/internal/protocol/protocol.go; then
  ok "payload is json.RawMessage"
else
  bad "payload is not json.RawMessage"
fi

echo "=== 6. ack seq lives in payload, not the envelope ==="
if awk '/^type AckPayload struct/,/^}/' backend/internal/protocol/protocol.go | grep -q 'Seq .*json:"seq,omitempty"'; then
  ok "AckPayload carries its own Seq"
else
  bad "AckPayload has no payload-level Seq"
fi
if grep -q 'PersistentTypes = map\[string\]bool{' backend/internal/protocol/protocol.go; then
  ok "persistent types are an explicit set"
fi

echo "=== 7. metrics vocabulary uses new types only ==="
for t in 'op.add' 'resync.request' 'resync.snapshot' 'resync.events' 'presence.join' 'presence.leave' 'presence.cursor' 'room.meta'; do
  if grep -q "\"$t\"" backend/internal/metrics/metrics.go; then ok "metric type $t"; else bad "metric missing $t"; fi
done
for stale in '"op"' '"snapshot"' '"sync.ops"' '"sync.reset"'; do
  if grep -q "$stale" backend/internal/metrics/metrics.go; then
    bad "metric vocabulary still has $stale"
  fi
done
ok "no obsolete metric labels"

echo "=== 8. protocol package has zero internal imports ==="
if grep -q 'github.com/codraw/codraw/backend/internal' backend/internal/protocol/*.go; then
  bad "protocol imports an internal package"; grep -n 'codraw/backend/internal' backend/internal/protocol/*.go
else
  ok "protocol is dependency-free"
fi

echo "=== 9. room does not import ws or hub ==="
if grep -E '^\s*"github.com/codraw/codraw/backend/internal/(ws|hub)"' backend/internal/room/*.go >/dev/null 2>&1; then
  bad "room imports ws or hub"
  grep -nE '^\s*"github.com/codraw/codraw/backend/internal/(ws|hub)"' backend/internal/room/*.go
else
  ok "room has no ws/hub import"
fi

echo "=== 10. readonly token endpoints exist ==="
for r in 'readonly-token' 'readonly-token/revoke'; do
  if grep -q "$r" backend/internal/api/api.go; then ok "route $r"; else bad "missing route $r"; fi
done

echo "=== 11. no docs/API.md references as the source of truth ==="
if grep -rn 'docs/API.md' backend/ --include='*.go' | grep -v 'deprecated\|废弃' >/dev/null 2>&1; then
  bad "backend still cites docs/API.md"; grep -rn 'docs/API.md' backend/ --include='*.go' | head -3
else
  ok "no stale docs/API.md citations"
fi

echo "=== 12. ws-smoke tool exists ==="
if [ -f backend/cmd/wssmoke/main.go ]; then ok "cmd/wssmoke"; else bad "missing cmd/wssmoke"; fi
if [ -f backend/scripts/sync-webui.sh ]; then ok "scripts/sync-webui.sh"; else bad "missing sync-webui.sh"; fi

echo
if [ "$FAIL" -ne 0 ]; then echo "CONTRACT CHECK FAILED"; exit 1; fi
echo "CONTRACT CHECK PASSED"
