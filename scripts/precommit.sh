#!/usr/bin/env bash
# 提交前的最终验证
set -uo pipefail
export PATH="$HOME/.gvm/gos/go1.27.3/bin:$PATH"
export GOCACHE=/tmp/gocache-pm
ROOT=/mnt/d/Work/AI-TMP/codraw
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
