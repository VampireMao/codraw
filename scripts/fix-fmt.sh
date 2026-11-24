#!/usr/bin/env bash
set -uo pipefail
export PATH="$HOME/.gvm/gos/go1.27.3/bin:$PATH"
cd /mnt/d/Work/AI-TMP/codraw/backend || exit 1

echo "=== 格式化 ==="
gofmt -w internal/room/roles.go
echo done

echo
echo "=== 检查是否还有未格式化文件 ==="
OUT=$(gofmt -l .)
if [ -z "$OUT" ]; then echo "OK: 全部已格式化"; else echo "仍有:"; echo "$OUT"; fi

echo
echo "=== 确认格式化没破坏编译 ==="
export GOCACHE=/tmp/gocache-pm
go build -o /tmp/codraw-fmt ./cmd/server && echo "build OK"
