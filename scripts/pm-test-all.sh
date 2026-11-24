#!/usr/bin/env bash
set -uo pipefail
cd /mnt/d/Work/AI-TMP/codraw/web || exit 1
export NVM_DIR="$HOME/.nvm"
# shellcheck disable=SC1091
. "$NVM_DIR/nvm.sh" >/dev/null 2>&1
nvm use 24.18.0 >/dev/null 2>&1

echo "=== 单元测试汇总 ==="
npm run test 2>&1 | grep -E 'Test Files|Tests  |FAIL|✓ tests' | tail -20

echo
echo "=== 后端测试 ==="
export PATH="$HOME/.gvm/gos/go1.27.3/bin:$PATH"
export GOCACHE=/tmp/gocache-pm
cd /mnt/d/Work/AI-TMP/codraw/backend
go vet ./... && echo "vet OK"
go test -race -count=1 ./... 2>&1 | grep -E '^(ok|FAIL)'
