#!/usr/bin/env bash
set -euo pipefail
export PATH="$HOME/.gvm/gos/go1.27.3/bin:$PATH"
export GOCACHE="${GOCACHE:-/tmp/codraw-gocache}"
cd /mnt/d/Work/AI-TMP/codraw/backend
echo "=== gofmt ==="
gofmt -l ./cmd ./internal
echo "=== vet ==="
go vet ./... && echo "VET PASS"
echo "=== test -race ==="
go test -race -count=1 -timeout 300s ./... 2>&1 | tail -22
