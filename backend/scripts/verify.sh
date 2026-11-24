#!/usr/bin/env bash
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/../.." && pwd)"
. "$_REPO_ROOT/scripts/_env.sh"
set -euo pipefail
cd "$_REPO_ROOT/backend"
echo "=== gofmt ==="
gofmt -l ./cmd ./internal
echo "=== vet ==="
go vet ./... && echo "VET PASS"
echo "=== test -race ==="
go test -race -count=1 -timeout 300s ./... 2>&1 | tail -22
