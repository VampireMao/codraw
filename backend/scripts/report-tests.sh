#!/usr/bin/env bash
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/../.." && pwd)"
. "$_REPO_ROOT/scripts/_env.sh"
set -euo pipefail
cd "$_REPO_ROOT/backend"

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
