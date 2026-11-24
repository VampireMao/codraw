#!/usr/bin/env bash
set -euo pipefail
export PATH="$HOME/.gvm/gos/go1.27.3/bin:$PATH"
export GOCACHE="${GOCACHE:-/tmp/codraw-gocache}"
cd /mnt/d/Work/AI-TMP/codraw/backend

echo "########## go version ##########"
go version

echo
echo "########## go vet ./... ##########"
go vet ./... 2>&1
echo "vet exit: $?"

echo
echo "########## go test -race ./... ##########"
go test -race -count=1 -timeout 300s ./... 2>&1

echo
echo "########## test count ##########"
go test -count=1 -list '.*' ./... 2>/dev/null | grep -c '^Test' || true
