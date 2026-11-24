#!/usr/bin/env bash
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"
. "$_SCRIPT_DIR/_env.sh"
set -uo pipefail
cd "$_REPO_ROOT/web" || exit 1

echo "=== 单元测试汇总 ==="
npm run test 2>&1 | grep -E 'Test Files|Tests  |FAIL|✓ tests' | tail -20

echo
echo "=== 后端测试 ==="
cd "$_REPO_ROOT/backend"
go vet ./... && echo "vet OK"
go test -race -count=1 ./... 2>&1 | grep -E '^(ok|FAIL)'
