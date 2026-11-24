#!/usr/bin/env bash
# 验证 docs/sql/0001_init.sql 在真实 Postgres 上可执行且幂等
set -uo pipefail
ROOT=/mnt/d/Work/AI-TMP/codraw
SQL="$ROOT/docs/sql/0001_init.sql"
CT=codraw-ddl-check

echo "=== 启动临时 Postgres ==="
docker rm -f $CT >/dev/null 2>&1 || true
docker run -d --name $CT -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=ddl postgres:16-alpine >/dev/null
for i in $(seq 1 40); do
  if docker exec $CT pg_isready -U postgres -d ddl >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec $CT pg_isready -U postgres -d ddl || { echo "PG 未就绪"; docker rm -f $CT; exit 1; }

echo
echo "=== 第 1 次执行 DDL ==="
docker exec -i $CT psql -U postgres -d ddl -v ON_ERROR_STOP=1 < "$SQL" 2>&1 | tail -5
RC1=$?
echo "exit=$RC1"

echo
echo "=== 第 2 次执行（验证幂等）==="
docker exec -i $CT psql -U postgres -d ddl -v ON_ERROR_STOP=1 < "$SQL" 2>&1 | tail -5
RC2=$?
echo "exit=$RC2"

echo
echo "=== 结构核对 ==="
docker exec $CT psql -U postgres -d ddl -t -c "
  SELECT 'rooms.owner_client_id: ' || count(*) FROM information_schema.columns
    WHERE table_name='rooms' AND column_name='owner_client_id'
  UNION ALL
  SELECT 'room_grants table: ' || count(*) FROM information_schema.tables
    WHERE table_name='room_grants'
  UNION ALL
  SELECT 'room_grants.role check: ' || count(*) FROM information_schema.table_constraints
    WHERE table_name='room_grants' AND constraint_type='CHECK';
" 2>&1 | sed 's/^/  /'

echo
echo "=== 约束生效验证（插入非法 role 应失败）==="
docker exec $CT psql -U postgres -d ddl -c "
  INSERT INTO rooms(code, editor_key_hash, viewer_key_hash) VALUES ('ABC234', '\\x00', '\\x00');
" >/dev/null 2>&1
OUT=$(docker exec $CT psql -U postgres -d ddl -c "
  INSERT INTO room_grants(room_code, client_id, role) VALUES ('ABC234', 'c_1', 'owner');
" 2>&1)
echo "$OUT" | grep -qi 'violates check constraint' && echo "  OK: 非法 role 被拒绝" || echo "  FAIL: 非法 role 未被拒绝"

docker rm -f $CT >/dev/null 2>&1
echo
[ "$RC1" = "0" ] && [ "$RC2" = "0" ] && echo "DDL 验证通过" || echo "DDL 验证失败"
