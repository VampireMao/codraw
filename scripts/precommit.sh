#!/usr/bin/env bash
# 提交前的最终验证
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"
. "$_SCRIPT_DIR/_env.sh"
set -uo pipefail
ROOT="$_REPO_ROOT"
cd "$ROOT/backend" || exit 1

echo "=== gofmt（CI 会检查）==="
OUT=$(gofmt -l . 2>&1)
if [ -z "$OUT" ]; then echo "  OK 全部已格式化"; else echo "  FAIL:"; echo "$OUT" | head -5; fi

echo
echo "=== go vet ==="
go vet ./... && echo "  OK"

echo
echo "=== 后端测试（CI 用 -race）==="
go test -race -count=1 ./... 2>&1 | grep -cE '^ok' | xargs -I{} echo "  {} 个包通过"
go test -race -count=1 ./... 2>&1 | grep -E '^FAIL' && echo "  有失败" || echo "  无失败"
