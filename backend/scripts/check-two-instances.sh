#!/usr/bin/env bash
# Two-instance integration check with assertions. Exits non-zero on any mismatch,
# so it can run in CI and be quoted in the report.
set -euo pipefail
export PATH="$HOME/.gvm/gos/go1.27.3/bin:$PATH"
export GOCACHE="${GOCACHE:-/tmp/codraw-gocache}"
cd /mnt/d/Work/AI-TMP/codraw/backend

PGPORT=${PGPORT:-55436}; RPORT=${RPORT:-56383}
PGNAME=codraw-pg-ci; RNAME=codraw-redis-ci
PGURL="postgres://postgres:dev@127.0.0.1:$PGPORT/postgres?sslmode=disable"
RURL="redis://127.0.0.1:$RPORT/0"
FAIL=0
cleanup() {
  kill $(jobs -p) 2>/dev/null || true
  docker rm -f "$PGNAME" "$RNAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAIL=1; }

echo "=== infra ==="
docker rm -f "$PGNAME" "$RNAME" >/dev/null 2>&1 || true
docker run -d --name "$PGNAME" -e POSTGRES_PASSWORD=dev -p "$PGPORT:5432" postgres:16-alpine >/dev/null
docker run -d --name "$RNAME" -p "$RPORT:6379" redis:7-alpine >/dev/null
for i in $(seq 1 80); do
  docker exec "$PGNAME" pg_isready -U postgres >/dev/null 2>&1 && \
  docker exec "$RNAME" redis-cli ping >/dev/null 2>&1 && break
  sleep 0.5
done

GOBIN=/tmp/codraw-2inst-ci
rm -rf "$GOBIN"; mkdir -p "$GOBIN"
go build -o "$GOBIN/server" ./cmd/server
go build -o "$GOBIN/wssmoke" ./cmd/wssmoke

P1=18150; P2=18151
start() {
  STORE=postgres BROKER=redis PORT=$1 LOG_LEVEL=info LOG_FORMAT=text \
    DATABASE_URL="$PGURL" REDIS_URL="$RURL" \
    READONLY_SECRET="dev-only-change-me-please-32bytes-min" \
    PUBLIC_BASE_URL="http://localhost:$1" "$GOBIN/server" >>"/tmp/ci-$1.log" 2>&1 &
  echo $!
}
rm -f /tmp/ci-*.log
PID1=$(start $P1); PID2=$(start $P2)
for i in $(seq 1 150); do
  curl -fsS "http://localhost:$P1/healthz" >/dev/null 2>&1 && \
  curl -fsS "http://localhost:$P2/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done

shapeCount() { curl -s "http://localhost:$1/api/rooms/$2" | python3 -c 'import sys,json;print(json.load(sys.stdin)["shapeCount"])'; }
seqAck() { grep -o '"seq":[0-9]*' | head -1 | cut -d: -f2; }

echo "=== readyz ==="
for p in $P1 $P2; do
  body=$(curl -s "http://localhost:$p/readyz")
  if echo "$body" | grep -q '"store":"postgres"' && echo "$body" | grep -q '"broker":"redis"'; then
    pass "readyz on :$p reports postgres+redis"
  else
    fail "readyz on :$p: $body"
  fi
done

CREATE=$(curl -s -X POST "http://localhost:$P1/api/rooms" -H 'Content-Type: application/json' -d '{"title":"双实例"}')
CODE=$(echo "$CREATE" | python3 -c 'import sys,json;print(json.load(sys.stdin)["room"]["code"])')
EKEY=$(echo "$CREATE" | python3 -c 'import sys,json;print(json.load(sys.stdin)["keys"]["editorKey"])')
VKEY=$(echo "$CREATE" | python3 -c 'import sys,json;print(json.load(sys.stdin)["keys"]["viewerKey"])')
echo "room=$CODE"

if [[ "$CODE" =~ ^[2-9A-HJKMNP-Z]{6}$ ]]; then pass "code matches the DDL CHECK"; else fail "code $CODE"; fi

# Both instances resolve the shared Postgres row.
c1=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:$P1/api/rooms/$CODE")
c2=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:$P2/api/rooms/$CODE")
[ "$c1" = 200 ] && pass "inst1 sees the room" || fail "inst1 GET -> $c1"
[ "$c2" = 200 ] && pass "inst2 sees the room (shared Postgres)" || fail "inst2 GET -> $c2"

echo "=== alternate writes across instances; seq must be globally monotonic ==="
s1=$("$GOBIN/wssmoke" -base "http://localhost:$P1" -room "$CODE" -key "$EKEY" -send-op 1 -name A 2>&1 | grep -E '^<- ack' | seqAck)
s2=$("$GOBIN/wssmoke" -base "http://localhost:$P2" -room "$CODE" -key "$EKEY" -send-op 1 -name B 2>&1 | grep -E '^<- ack' | seqAck)
s3=$("$GOBIN/wssmoke" -base "http://localhost:$P1" -room "$CODE" -key "$EKEY" -send-op 1 -name A2 2>&1 | grep -E '^<- ack' | seqAck)
echo "     acks: inst1=$s1 inst2=$s2 inst1=$s3"
if [ "$s1" = 1 ] && [ "$s2" = 2 ] && [ "$s3" = 3 ]; then
  pass "seq is globally monotonic across instances (1,2,3)"
else
  fail "seq across instances was $s1,$s2,$s3 (want 1,2,3)"
fi

redisSeq=$(docker exec "$RNAME" redis-cli GET "codraw:seq:$CODE")
[ "$redisSeq" = 3 ] && pass "redis INCR counter reached 3" || fail "redis counter = $redisSeq"

pgRows=$(docker exec "$PGNAME" psql -U postgres -tAc \
  "SELECT string_agg(seq::text, ',' ORDER BY seq) FROM events WHERE room_code='$CODE'")
[ "$pgRows" = "1,2,3" ] && pass "postgres holds a single contiguous log 1,2,3" || fail "postgres seqs = $pgRows"

# Cross-instance convergence. Pub/sub delivery is asynchronous, so poll with a
# generous budget rather than assuming a settle time; the assertion is that both
# instances converge, not that they do so within one scheduling quantum.
converged=0
for i in $(seq 1 100); do
  a=$(shapeCount $P1 "$CODE"); b=$(shapeCount $P2 "$CODE")
  if [ "$a" = 3 ] && [ "$b" = 3 ]; then converged=1; break; fi
  sleep 0.1
done
echo "     convergence: inst1=$a inst2=$b (after $i polls, converged=$converged)"
a=$(shapeCount $P1 "$CODE"); b=$(shapeCount $P2 "$CODE")
[ "$a" = 3 ] && pass "inst1 converged to 3 shapes" || fail "inst1 shapeCount=$a (want 3)"
[ "$b" = 3 ] && pass "inst2 converged to 3 shapes" || fail "inst2 shapeCount=$b (want 3)"

echo "=== a fresh client on inst2 must see the full room from the shared log ==="
out=$("$GOBIN/wssmoke" -base "http://localhost:$P2" -room "$CODE" -key "$VKEY" -send-op 0 -name 观众 2>&1)
if echo "$out" | grep -q 'shapes=3'; then
  pass "inst2 snapshot contains all 3 shapes"
else
  fail "inst2 snapshot did not contain 3 shapes: $(echo "$out" | grep snapshot | head -1)"
fi

echo "=== metrics on both ==="
for p in $P1 $P2; do
  if curl -s "http://localhost:$p/metrics" | grep -q '^codraw_rooms 1'; then
    pass "metrics on :$p reports one room"
  else
    fail "metrics on :$p"
  fi
done

echo "=== logs must be clean ==="
if grep -E 'level=ERROR' /tmp/ci-*.log | grep -v 'in-memory\|in-process' >/dev/null 2>&1; then
  fail "server logged errors:"; grep -E 'level=ERROR' /tmp/ci-*.log | head -5
else
  pass "no error-level logs on either instance"
fi

kill -TERM "$PID1" "$PID2" 2>/dev/null || true
wait "$PID1" "$PID2" 2>/dev/null || true

echo
if [ "$FAIL" -ne 0 ]; then echo "TWO-INSTANCE CHECK FAILED"; exit 1; fi
echo "TWO-INSTANCE CHECK PASSED"
