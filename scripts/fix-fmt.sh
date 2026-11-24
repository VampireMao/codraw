#!/usr/bin/env bash
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"
. "$_SCRIPT_DIR/_env.sh"
set -uo pipefail
cd "$_REPO_ROOT/backend" || exit 1

echo "=== 格式化 ==="
gofmt -w internal/room/roles.go
echo done

echo
echo "=== 检查是否还有未格式化文件 ==="
OUT=$(gofmt -l .)
if [ -z "$OUT" ]; then echo "OK: 全部已格式化"; else echo "仍有:"; echo "$OUT"; fi

echo
echo "=== 确认格式化没破坏编译 ==="
go build -o /tmp/codraw-fmt ./cmd/server && echo "build OK"
